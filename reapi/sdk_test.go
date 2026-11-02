package reapi

// Compatibility test against the official REAPI Go client SDK
// (github.com/bazelbuild/remote-apis-sdks, used by Google's reclient). It
// exercises the same RPCs a remote-cache client such as Bazel uses:
// GetCapabilities, FindMissingBlobs, BatchUpdateBlobs, ByteStream Write and
// Read for large blobs, BatchReadBlobs, and the ActionCache.

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
	"google.golang.org/grpc"
)

func TestOfficialSDKClient(t *testing.T) {
	ctx := context.Background()
	st, err := cas.OpenDisk(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ac, _ := actioncache.OpenDisk(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := map[string]int{}
	count := func(m string) { mu.Lock(); calls[m]++; mu.Unlock() }
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(16<<20),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			count(info.FullMethod)
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			count(info.FullMethod)
			return h(srv, ss)
		}))
	(&Server{CAS: st, AC: ac}).Register(gs)
	go gs.Serve(ln)
	defer gs.Stop()

	c, err := client.NewClient(ctx, "some/instance", client.DialParams{Service: ln.Addr().String(), NoSecurity: true})
	if err != nil {
		t.Fatalf("SDK could not connect / check capabilities: %v", err)
	}
	defer c.Close()

	small := []byte("small blob")
	big := bytes.Repeat([]byte("0123456789abcdef"), (6<<20)/16+3) // > batch limit: forces ByteStream
	dSmall, dBig := digest.NewFromBlob(small), digest.NewFromBlob(big)

	missing, err := c.MissingBlobs(ctx, []digest.Digest{dSmall, dBig})
	if err != nil || len(missing) != 2 {
		t.Fatalf("MissingBlobs cold: %v %v", missing, err)
	}
	if err := c.WriteBlobs(ctx, map[digest.Digest][]byte{dSmall: small, dBig: big}); err != nil {
		t.Fatalf("WriteBlobs: %v", err)
	}
	missing, err = c.MissingBlobs(ctx, []digest.Digest{dSmall, dBig})
	if err != nil || len(missing) != 0 {
		t.Fatalf("MissingBlobs warm: %v %v", missing, err)
	}
	got, _, err := c.ReadBlob(ctx, dBig)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("ReadBlob (ByteStream) big: %d bytes, %v", len(got), err)
	}
	extra := map[digest.Digest][]byte{}
	for _, b := range []string{"a", "bb", "ccc"} {
		extra[digest.NewFromBlob([]byte(b))] = []byte(b)
	}
	if err := c.BatchWriteBlobs(ctx, extra); err != nil {
		t.Fatalf("BatchWriteBlobs: %v", err)
	}
	batch, err := c.BatchDownloadBlobs(ctx, []digest.Digest{dSmall})
	if err != nil || !bytes.Equal(batch[dSmall], small) {
		t.Fatalf("BatchDownloadBlobs: %v", err)
	}

	action := digest.NewFromBlob([]byte("an action"))
	ar := &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: "out/big", Digest: dBig.ToProto()}}, StdoutDigest: dSmall.ToProto()}
	if _, err := c.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: c.InstanceName, ActionDigest: action.ToProto(), ActionResult: ar}); err != nil {
		t.Fatalf("UpdateActionResult: %v", err)
	}
	res, err := c.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: c.InstanceName, ActionDigest: action.ToProto()})
	if err != nil || len(res.OutputFiles) != 1 || res.OutputFiles[0].Path != "out/big" {
		t.Fatalf("GetActionResult: %v %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range []string{
		"/build.bazel.remote.execution.v2.Capabilities/GetCapabilities",
		"/build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs",
		"/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchUpdateBlobs",
		"/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchReadBlobs",
		"/google.bytestream.ByteStream/Write",
		"/google.bytestream.ByteStream/Read",
		"/build.bazel.remote.execution.v2.ActionCache/UpdateActionResult",
		"/build.bazel.remote.execution.v2.ActionCache/GetActionResult",
	} {
		if calls[m] == 0 {
			t.Errorf("SDK never called %s (calls: %v)", m, calls)
		}
	}
}
