//go:build bazel

package reapi

// This test needs a real `bazel` (or `bazelisk`) on PATH. It could not be
// run in the development environment (Bazel downloads are blocked there),
// so its result is not claimed anywhere; run it with:
//
//	go test -tags bazel ./reapi -run Bazel -v

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
	"github.com/xartus2004/greenline/httpcache"
	"google.golang.org/grpc"
)

func bazelBin(t *testing.T) string {
	for _, b := range []string{"bazel", "bazelisk"} {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	t.Fatal("bazel not on PATH")
	return ""
}

// runTwice builds the workspace twice with a fresh output base each time and
// returns the second run's stderr.
func runTwice(t *testing.T, flag string) string {
	bin := bazelBin(t)
	ws, _ := filepath.Abs("testdata/bazelws")
	var second string
	for i := 0; i < 2; i++ {
		out := t.TempDir()
		cmd := exec.Command(bin, "--output_base="+out, "build", "//:twice", flag, "--noshow_progress")
		cmd.Dir = ws
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("bazel run %d: %v\n%s", i, err, stderr.String())
		}
		exec.Command(bin, "--output_base="+out, "shutdown").Run()
		second = stderr.String()
	}
	os.RemoveAll(filepath.Join(ws, "bazel-bazelws"))
	return second
}

func stores(t *testing.T) (cas.Store, actioncache.Raw) {
	st, err := cas.OpenDisk(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ac, _ := actioncache.OpenDisk(t.TempDir())
	return st, ac
}

func TestBazelHTTPCache(t *testing.T) {
	st, ac := stores(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, &httpcache.Server{CAS: st, AC: ac})
	out := runTwice(t, "--remote_cache=http://"+ln.Addr().String())
	if !strings.Contains(out, "2 remote cache hit") {
		t.Fatalf("second build not fully remote-cached:\n%s", out)
	}
}

func TestBazelGRPCCache(t *testing.T) {
	st, ac := stores(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	gs := grpc.NewServer()
	(&Server{CAS: st, AC: ac}).Register(gs)
	go gs.Serve(ln)
	defer gs.Stop()
	out := runTwice(t, "--remote_cache=grpc://"+ln.Addr().String())
	if !strings.Contains(out, "2 remote cache hit") {
		t.Fatalf("second build not fully remote-cached:\n%s", out)
	}
}
