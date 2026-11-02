package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xartus2004/greenline/forge/github"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
)

// fakeHub is a GitHub REST API backed by a real bare repository, which is
// also the git remote. It enforces fast-forward-only ref updates.
type fakeHub struct {
	repo   *localgit.Repo
	mu     sync.Mutex
	checks []github.CheckRun
}

func (f *fakeHub) git(args ...string) (string, error) {
	return f.repo.Git(context.Background(), nil, args...)
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
	switch {
	case r.Method == "GET" && p == "/git/ref/heads/main":
		h, _ := f.git("rev-parse", "refs/heads/main")
		json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": h}})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
		out, err := f.git("rev-list", "--parents", "-n1", strings.TrimPrefix(p, "/git/commits/"))
		if err != nil {
			w.WriteHeader(404)
			return
		}
		var parents []map[string]string
		for _, x := range strings.Fields(out)[1:] {
			parents = append(parents, map[string]string{"sha": x})
		}
		json.NewEncoder(w).Encode(map[string]any{"parents": parents})
	case r.Method == "PATCH" && p == "/git/refs/heads/main":
		var body struct {
			SHA   string `json:"sha"`
			Force bool   `json:"force"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, err := f.git("merge-base", "--is-ancestor", "refs/heads/main", body.SHA); err != nil && !body.Force {
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"Update is not a fast forward"}`))
			return
		}
		f.git("update-ref", "refs/heads/main", body.SHA)
		w.Write([]byte("{}"))
	case r.Method == "GET" && strings.HasPrefix(p, "/compare/"):
		a, _, _ := strings.Cut(strings.TrimPrefix(p, "/compare/"), "...")
		st := "diverged"
		if _, err := f.git("merge-base", "--is-ancestor", a, "refs/heads/main"); err == nil {
			st = "ahead"
		}
		json.NewEncoder(w).Encode(map[string]string{"status": st})
	case r.Method == "POST" && p == "/check-runs":
		var cr github.CheckRun
		json.NewDecoder(r.Body).Decode(&cr)
		f.mu.Lock()
		f.checks = append(f.checks, cr)
		f.mu.Unlock()
		w.WriteHeader(201)
		w.Write([]byte("{}"))
	default:
		w.WriteHeader(404)
	}
}

func TestGitHubModeEndToEnd(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "BUILD.yaml"), []byte("targets:\n- name: t\n  srcs: [v.txt]\n  cmd: grep -q good v.txt\n  test: true\n"), 0o644)
	os.WriteFile(filepath.Join(src, "v.txt"), []byte("good\n"), 0o644)
	remote, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "remote.git"), src)
	if err != nil {
		t.Fatal(err)
	}
	// Open PRs the way GitHub exposes them: refs/pull/<n>/head.
	w := &localgit.Work{Dir: t.TempDir(), Remote: remote.Dir}
	w.Clone(ctx)
	for n, content := range map[string]string{"1": "good change\n", "2": "broken\n"} {
		w.Checkout(ctx, "b"+n, "main")
		w.WriteFile("v.txt", content)
		w.CommitAll(ctx, "pr "+n)
		if err := w.Push(ctx, "b"+n+":refs/pull/"+n+"/head"); err != nil {
			t.Fatal(err)
		}
	}
	oldMain, _ := remote.Head()

	hub := &fakeHub{repo: remote}
	api := httptest.NewServer(hub)
	defer api.Close()
	client := github.New(api.URL, "o", "r", "tok")
	mirror, err := github.OpenMirror(ctx, client, remote.Dir, filepath.Join(t.TempDir(), "mirror.git"))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("whsec")
	srv, err := New(Config{StateDir: t.TempDir(), Repo: mirror.Mirror, Forge: mirror, Fetcher: mirror,
		WebhookSecret: secret, Queue: queue.Config{FlakyRetries: -1}, PollMain: time.Second,
		OnTransition: func(tr Transition) { client.ReportPR(context.Background(), tr.PR) }})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- srv.Run(runCtx) }()
	hs := httptest.NewServer(srv.Handler())

	for _, n := range []int{1, 2} {
		head, _ := remote.Git(ctx, nil, "rev-parse", "refs/pull/"+string(rune('0'+n))+"/head")
		body, _ := json.Marshal(map[string]any{"action": "labeled", "number": n, "label": map[string]string{"name": "merge-queue"},
			"pull_request": map[string]any{"state": "open", "head": map[string]string{"sha": head}, "base": map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "merge-queue"}}}})
		m := hmac.New(sha256.New, secret)
		m.Write(body)
		req, _ := http.NewRequest("POST", hs.URL+"/webhook", strings.NewReader(string(body)))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 204 {
			t.Fatalf("webhook %d: %v %v", n, err, resp.Status)
		}
		resp.Body.Close()
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := srv.Status()
		if find(st, 1).State == queue.Merged && find(st, 2).State == queue.Ejected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %+v %+v", find(st, 1), find(st, 2))
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	hs.Close()
	http.DefaultClient.CloseIdleConnections()
	client.HTTP.CloseIdleConnections()

	newMain, _ := remote.Head()
	parents, _ := remote.Git(ctx, nil, "rev-list", "--parents", "-n1", newMain)
	if f := strings.Fields(parents); len(f) != 2 || f[1] != oldMain {
		t.Fatalf("remote main %s is not a single-parent child of %s", newMain, oldMain)
	}
	if v, _ := remote.Git(ctx, nil, "show", newMain+":v.txt"); v != "good change" {
		t.Fatalf("remote main has v.txt=%q", v)
	}
	if refs, _ := remote.Git(ctx, nil, "for-each-ref", "refs/heads/greenline/"); refs != "" {
		t.Fatalf("temporary branches left behind: %s", refs)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	final := map[string]string{}
	for _, c := range hub.checks {
		if c.Status == "completed" {
			final[c.HeadSHA] = c.Conclusion
		}
	}
	if len(final) != 2 {
		t.Fatalf("check runs: %+v", hub.checks)
	}
}
