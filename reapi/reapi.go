// Package reapi serves greenline's cache over the Bazel Remote Execution API
// v2 gRPC services that a remote *cache* needs: Capabilities,
// ContentAddressableStorage (FindMissingBlobs, BatchUpdateBlobs,
// BatchReadBlobs), ActionCache and google.bytestream.ByteStream. Remote
// execution is out of scope.
//
// Action-cache values are stored as serialized ActionResult protobufs in the
// same actioncache.Raw store that the HTTP cache uses (Bazel's HTTP protocol
// stores the same bytes under /ac/), so both front ends share one cache.
// Instance names are accepted and ignored: there is a single namespace.
package reapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/bazelbuild/remote-apis/build/bazel/semver"
	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MaxBatchBytes is advertised as max_batch_total_size_bytes.
const MaxBatchBytes = 4 << 20

// Server implements the cache services.
type Server struct {
	repb.UnimplementedContentAddressableStorageServer
	repb.UnimplementedActionCacheServer
	repb.UnimplementedCapabilitiesServer
	bspb.UnimplementedByteStreamServer

	CAS cas.Store
	AC  actioncache.Raw
	// MaxBlobBytes bounds ByteStream uploads (default 1 GiB).
	MaxBlobBytes int64
}

// Register registers every service on s.
func (srv *Server) Register(s *grpc.Server) {
	repb.RegisterContentAddressableStorageServer(s, srv)
	repb.RegisterActionCacheServer(s, srv)
	repb.RegisterCapabilitiesServer(s, srv)
	bspb.RegisterByteStreamServer(s, srv)
}

func toDigest(d *repb.Digest) (cas.Digest, error) {
	if d == nil {
		return cas.Digest{}, status.Error(codes.InvalidArgument, "missing digest")
	}
	dg := cas.Digest{Hash: d.Hash, Size: d.SizeBytes}
	if err := dg.Validate(); err != nil {
		return cas.Digest{}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return dg, nil
}

func checkDigestFunction(f repb.DigestFunction_Value) error {
	if f != repb.DigestFunction_UNKNOWN && f != repb.DigestFunction_SHA256 {
		return status.Errorf(codes.InvalidArgument, "only SHA256 is supported, got %v", f)
	}
	return nil
}

// GetCapabilities implements Capabilities.
func (srv *Server) GetCapabilities(context.Context, *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	return &repb.ServerCapabilities{
		CacheCapabilities: &repb.CacheCapabilities{
			DigestFunctions:               []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
			ActionCacheUpdateCapabilities: &repb.ActionCacheUpdateCapabilities{UpdateEnabled: true},
			MaxBatchTotalSizeBytes:        MaxBatchBytes,
			SymlinkAbsolutePathStrategy:   repb.SymlinkAbsolutePathStrategy_DISALLOWED,
		},
		LowApiVersion:  &semver.SemVer{Major: 2},
		HighApiVersion: &semver.SemVer{Major: 2, Minor: 3},
	}, nil
}

// FindMissingBlobs implements ContentAddressableStorage.
func (srv *Server) FindMissingBlobs(ctx context.Context, req *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	resp := &repb.FindMissingBlobsResponse{}
	for _, d := range req.BlobDigests {
		dg, err := toDigest(d)
		if err != nil {
			return nil, err
		}
		ok, err := srv.CAS.Has(ctx, dg)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
		if !ok {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
		}
	}
	return resp, nil
}

func rpcStatus(err error) *rpcstatus.Status {
	if err == nil {
		return &rpcstatus.Status{Code: int32(codes.OK)}
	}
	return status.Convert(err).Proto()
}

func (srv *Server) put(ctx context.Context, dg cas.Digest, data []byte) error {
	err := srv.CAS.Put(ctx, dg, strings.NewReader(string(data)))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, cas.ErrDigestMismatch):
		return status.Errorf(codes.InvalidArgument, "data does not match digest %s", dg)
	case errors.Is(err, cas.ErrTooLarge):
		return status.Errorf(codes.ResourceExhausted, "blob %s exceeds capacity", dg)
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// BatchUpdateBlobs implements ContentAddressableStorage.
func (srv *Server) BatchUpdateBlobs(ctx context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	var total int64
	for _, r := range req.Requests {
		total += int64(len(r.Data))
	}
	if total > MaxBatchBytes {
		return nil, status.Errorf(codes.InvalidArgument, "batch of %d bytes exceeds %d", total, MaxBatchBytes)
	}
	resp := &repb.BatchUpdateBlobsResponse{}
	for _, r := range req.Requests {
		var err error
		if r.Compressor != repb.Compressor_IDENTITY {
			err = status.Error(codes.InvalidArgument, "compression not supported")
		} else if dg, derr := toDigest(r.Digest); derr != nil {
			err = derr
		} else {
			err = srv.put(ctx, dg, r.Data)
		}
		resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{Digest: r.Digest, Status: rpcStatus(err)})
	}
	return resp, nil
}

// BatchReadBlobs implements ContentAddressableStorage.
func (srv *Server) BatchReadBlobs(ctx context.Context, req *repb.BatchReadBlobsRequest) (*repb.BatchReadBlobsResponse, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	var total int64
	for _, d := range req.Digests {
		if d != nil {
			total += d.SizeBytes
		}
	}
	if total > MaxBatchBytes {
		return nil, status.Errorf(codes.InvalidArgument, "batch of %d bytes exceeds %d", total, MaxBatchBytes)
	}
	resp := &repb.BatchReadBlobsResponse{}
	for _, d := range req.Digests {
		r := &repb.BatchReadBlobsResponse_Response{Digest: d}
		dg, err := toDigest(d)
		if err == nil {
			r.Data, err = srv.CAS.Get(ctx, dg)
			if errors.Is(err, cas.ErrNotFound) {
				err = status.Errorf(codes.NotFound, "blob %s not found", dg)
			} else if err != nil {
				err = status.Errorf(codes.Internal, "%v", err)
			}
		}
		r.Status = rpcStatus(err)
		resp.Responses = append(resp.Responses, r)
	}
	return resp, nil
}

// GetActionResult implements ActionCache. An entry whose outputs are no
// longer in the CAS is reported as NotFound so the client re-executes.
func (srv *Server) GetActionResult(ctx context.Context, req *repb.GetActionResultRequest) (*repb.ActionResult, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	dg, err := toDigest(req.ActionDigest)
	if err != nil {
		return nil, err
	}
	b, err := srv.AC.GetRaw(ctx, dg.Hash)
	if errors.Is(err, actioncache.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no result for action %s", dg)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	var ar repb.ActionResult
	if err := proto.Unmarshal(b, &ar); err != nil {
		return nil, status.Errorf(codes.NotFound, "undecodable result for %s", dg)
	}
	refs := []*repb.Digest{ar.StdoutDigest, ar.StderrDigest}
	for _, f := range ar.OutputFiles {
		refs = append(refs, f.Digest)
	}
	for _, d := range ar.OutputDirectories {
		refs = append(refs, d.TreeDigest)
	}
	for _, d := range refs {
		if d == nil || d.Hash == "" {
			continue
		}
		cd, err := toDigest(d)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "result references bad digest")
		}
		if ok, _ := srv.CAS.Has(ctx, cd); !ok {
			return nil, status.Errorf(codes.NotFound, "output %s of action %s was evicted", cd, dg)
		}
	}
	return &ar, nil
}

// UpdateActionResult implements ActionCache. Only successful results
// (exit code 0) are stored.
func (srv *Server) UpdateActionResult(ctx context.Context, req *repb.UpdateActionResultRequest) (*repb.ActionResult, error) {
	if err := checkDigestFunction(req.DigestFunction); err != nil {
		return nil, err
	}
	dg, err := toDigest(req.ActionDigest)
	if err != nil {
		return nil, err
	}
	if req.ActionResult == nil {
		return nil, status.Error(codes.InvalidArgument, "missing action_result")
	}
	if req.ActionResult.ExitCode != 0 {
		return nil, status.Error(codes.InvalidArgument, "refusing to cache a failed action")
	}
	b, err := proto.Marshal(req.ActionResult)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if err := srv.AC.PutRaw(ctx, dg.Hash, b); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return req.ActionResult, nil
}

// parseResource extracts the digest from ByteStream resource names:
//
//	[{instance}/]blobs/{hash}/{size}[/...]
//	[{instance}/]uploads/{uuid}/blobs/{hash}/{size}[/...]
func parseResource(name string, upload bool) (cas.Digest, error) {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if p == "compressed-blobs" {
			return cas.Digest{}, status.Error(codes.InvalidArgument, "compressed blobs not supported")
		}
		if p != "blobs" || i+2 >= len(parts) {
			continue
		}
		if upload && (i < 2 || parts[i-2] != "uploads") {
			continue
		}
		size, err := strconv.ParseInt(parts[i+2], 10, 64)
		if err != nil {
			break
		}
		dg := cas.Digest{Hash: parts[i+1], Size: size}
		if err := dg.Validate(); err != nil {
			return cas.Digest{}, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		return dg, nil
	}
	return cas.Digest{}, status.Errorf(codes.InvalidArgument, "bad resource name %q", name)
}

// Read implements ByteStream.
func (srv *Server) Read(req *bspb.ReadRequest, stream bspb.ByteStream_ReadServer) error {
	dg, err := parseResource(req.ResourceName, false)
	if err != nil {
		return err
	}
	data, err := srv.CAS.Get(stream.Context(), dg)
	if errors.Is(err, cas.ErrNotFound) {
		return status.Errorf(codes.NotFound, "blob %s not found", dg)
	}
	if err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	if req.ReadOffset < 0 || req.ReadOffset > int64(len(data)) {
		return status.Error(codes.OutOfRange, "read_offset out of range")
	}
	data = data[req.ReadOffset:]
	if req.ReadLimit > 0 && req.ReadLimit < int64(len(data)) {
		data = data[:req.ReadLimit]
	}
	const chunk = 1 << 20
	for len(data) > 0 {
		n := min(chunk, len(data))
		if err := stream.Send(&bspb.ReadResponse{Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// Write implements ByteStream (non-resumable).
func (srv *Server) Write(stream bspb.ByteStream_WriteServer) error {
	maxBytes := srv.MaxBlobBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 30
	}
	var (
		dg      cas.Digest
		started bool
		buf     []byte
	)
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "stream ended without finish_write")
		}
		if err != nil {
			return err
		}
		if !started {
			if dg, err = parseResource(req.ResourceName, true); err != nil {
				return err
			}
			if dg.Size > maxBytes {
				return status.Errorf(codes.InvalidArgument, "blob of %d bytes too large", dg.Size)
			}
			started = true
			// Already present: tell the client it can stop.
			if ok, _ := srv.CAS.Has(stream.Context(), dg); ok {
				return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: dg.Size})
			}
			buf = make([]byte, 0, dg.Size)
		}
		if req.WriteOffset != int64(len(buf)) {
			return status.Errorf(codes.InvalidArgument, "write_offset %d, expected %d", req.WriteOffset, len(buf))
		}
		buf = append(buf, req.Data...)
		if int64(len(buf)) > dg.Size {
			return status.Errorf(codes.InvalidArgument, "more data than digest size %d", dg.Size)
		}
		if req.FinishWrite {
			if err := srv.put(stream.Context(), dg, buf); err != nil {
				return err
			}
			return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: int64(len(buf))})
		}
	}
}

// QueryWriteStatus implements ByteStream. Uploads are not resumable, so a
// blob is either complete or absent.
func (srv *Server) QueryWriteStatus(ctx context.Context, req *bspb.QueryWriteStatusRequest) (*bspb.QueryWriteStatusResponse, error) {
	dg, err := parseResource(req.ResourceName, true)
	if err != nil {
		return nil, err
	}
	if ok, _ := srv.CAS.Has(ctx, dg); ok {
		return &bspb.QueryWriteStatusResponse{CommittedSize: dg.Size, Complete: true}, nil
	}
	return &bspb.QueryWriteStatusResponse{}, nil
}

// String describes the server for logs.
func (srv *Server) String() string { return fmt.Sprintf("reapi(cas=%T)", srv.CAS) }
