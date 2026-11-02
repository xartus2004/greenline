package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestAPIEndToEnd(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "BUILD.yaml"), []byte("targets:\n- name: t\n  srcs: [v.txt]\n  cmd: grep -q good v.txt\n  test: true\n"), 0o644)
	os.WriteFile(filepath.Join(src, "v.txt"), []byte("good\n"), 0o644)
	repo, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "r.git"), src)
	if err != nil {
		t.Fatal(err)
	}
	w := &localgit.Work{Dir: t.TempDir(), Remote: repo.Dir}
	w.Clone(ctx)
	for name, content := range map[string]string{"pr/1": "good one\n", "pr/2": "bad\n"} {
		w.Checkout(ctx, name, "main")
		w.WriteFile("v.txt", content)
		w.CommitAll(ctx, name)
		w.Push(ctx, name+":refs/heads/"+name)
	}
	srv, err := New(Config{StateDir: t.TempDir(), Repo: repo, Queue: queue.Config{FlakyRetries: -1}, PollMain: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- srv.Run(runCtx) }()
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	for _, body := range []string{`{"id":1}`, `{"id":2,"branch":"pr/2"}`} {
		resp, err := http.Post(hs.URL+"/api/prs", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusAccepted {
			t.Fatalf("enqueue %s: %v %v", body, err, resp.Status)
		}
		resp.Body.Close()
	}
	resp, err := http.Post(hs.URL+"/api/prs", "application/json", strings.NewReader(`{"id":3,"branch":"../x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad branch accepted: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := srv.Status()
		p1, p2 := find(st, 1), find(st, 2)
		if p1.State == queue.Merged && p2.State == queue.Ejected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %+v %+v", p1, p2)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for path, want := range map[string]string{"/metrics": "greenline_merged_prs_total 1", "/": "EJECTED", "/api/status": `"state":"MERGED"`} {
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %q", path, want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	hs.Close()
	http.DefaultClient.CloseIdleConnections()
}

func find(st *Status, id queue.PRID) queue.PR {
	for _, p := range st.PRs {
		if p.ID == id {
			return p
		}
	}
	return queue.PR{}
}
