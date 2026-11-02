package reapi

import (
	"context"
	"net"
	"strconv"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func dial(t *testing.T) (*grpc.ClientConn, *Server) {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	srv := &Server{CAS: cas.NewMemory(), AC: actioncache.NewMemory()}
	gs := grpc.NewServer()
	srv.Register(gs)
	go gs.Serve(ln)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return ln.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, srv
}

func pd(d cas.Digest) *repb.Digest { return &repb.Digest{Hash: d.Hash, SizeBytes: d.Size} }

func TestCASAndActionCache(t *testing.T) {
	ctx := context.Background()
	conn, _ := dial(t)
	c := repb.NewContentAddressableStorageClient(conn)
	ac := repb.NewActionCacheClient(conn)
	caps, err := repb.NewCapabilitiesClient(conn).GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	if err != nil || !caps.CacheCapabilities.ActionCacheUpdateCapabilities.UpdateEnabled {
		t.Fatalf("caps %v %v", caps, err)
	}
	blob := []byte("output bytes")
	d := cas.Of(blob)
	miss, err := c.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{pd(d)}})
	if err != nil || len(miss.MissingBlobDigests) != 1 {
		t.Fatalf("missing: %v %v", miss, err)
	}
	up, err := c.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{Requests: []*repb.BatchUpdateBlobsRequest_Request{
		{Digest: pd(d), Data: blob},
		{Digest: pd(d), Data: []byte("tampered!!!!")},
	}})
	if err != nil || up.Responses[0].Status.Code != 0 || codes.Code(up.Responses[1].Status.Code) != codes.InvalidArgument {
		t.Fatalf("update: %v %v", up, err)
	}
	rd, err := c.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{pd(d), pd(cas.Of([]byte("x")))}})
	if err != nil || string(rd.Responses[0].Data) != string(blob) || codes.Code(rd.Responses[1].Status.Code) != codes.NotFound {
		t.Fatalf("read: %v %v", rd, err)
	}
	action := pd(cas.Of([]byte("action")))
	if _, err := ac.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: action}); status.Code(err) != codes.NotFound {
		t.Fatalf("cold AC: %v", err)
	}
	ar := &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out", Digest: pd(d)}}}
	if _, err := ac.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: ar}); err != nil {
		t.Fatal(err)
	}
	got, err := ac.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: action})
	if err != nil || got.OutputFiles[0].Path != "out" {
		t.Fatalf("warm AC: %v %v", got, err)
	}
	// Dangling output → NotFound.
	dangling := pd(cas.Of([]byte("action2")))
	ac.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: dangling, ActionResult: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "o", Digest: pd(cas.Of([]byte("evicted")))}}}})
	if _, err := ac.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: dangling}); status.Code(err) != codes.NotFound {
		t.Fatalf("dangling: %v", err)
	}
	if _, err := ac.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: &repb.ActionResult{ExitCode: 1}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("failed action cached: %v", err)
	}
	if _, err := c.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: []*repb.Digest{{Hash: "../../etc", SizeBytes: 1}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad digest: %v", err)
	}
}

func TestByteStream(t *testing.T) {
	ctx := context.Background()
	conn, _ := dial(t)
	bs := bspb.NewByteStreamClient(conn)
	blob := make([]byte, 3<<20+17)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	d := cas.Of(blob)
	name := "inst/uploads/0f1e/blobs/" + d.Hash + "/" + itoa(d.Size)
	w, err := bs.Write(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(blob); off += 1 << 20 {
		end := min(off+1<<20, len(blob))
		if err := w.Send(&bspb.WriteRequest{ResourceName: name, WriteOffset: int64(off), Data: blob[off:end], FinishWrite: end == len(blob)}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := w.CloseAndRecv()
	if err != nil || resp.CommittedSize != d.Size {
		t.Fatalf("write: %v %v", resp, err)
	}
	q, err := bs.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: name})
	if err != nil || !q.Complete {
		t.Fatalf("query: %v %v", q, err)
	}
	r, err := bs.Read(ctx, &bspb.ReadRequest{ResourceName: "inst/blobs/" + d.Hash + "/" + itoa(d.Size), ReadOffset: 5})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		m, err := r.Recv()
		if err != nil {
			break
		}
		got = append(got, m.Data...)
	}
	if string(got) != string(blob[5:]) {
		t.Fatalf("read %d bytes, want %d", len(got), len(blob)-5)
	}
	for _, bad := range []string{"blobs/xyz/1", "uploads/u/blobs/" + d.Hash + "/-1", "compressed-blobs/zstd/" + d.Hash + "/1"} {
		if _, err := parseResource(bad, true); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
