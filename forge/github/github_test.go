package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xartus2004/greenline/queue"
)

// fakeGitHub models refs, commits (with parents) and fast-forward-only
// ref updates.
type fakeGitHub struct {
	mu      sync.Mutex
	main    string
	parents map[string][]string
	checks  []CheckRun
	auth    []string
}

func (f *fakeGitHub) ancestor(a, b string) bool {
	for c := b; c != ""; {
		if c == a {
			return true
		}
		p := f.parents[c]
		if len(p) == 0 {
			return false
		}
		c = p[0]
	}
	return false
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	p := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
	switch {
	case r.Method == "GET" && p == "/git/ref/heads/main":
		json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": f.main}})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
		sha := strings.TrimPrefix(p, "/git/commits/")
		ps, ok := f.parents[sha]
		if !ok {
			w.WriteHeader(404)
			return
		}
		var out []map[string]string
		for _, x := range ps {
			out = append(out, map[string]string{"sha": x})
		}
		json.NewEncoder(w).Encode(map[string]any{"parents": out})
	case r.Method == "PATCH" && p == "/git/refs/heads/main":
		var body struct {
			SHA   string `json:"sha"`
			Force bool   `json:"force"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !body.Force && !f.ancestor(f.main, body.SHA) {
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(map[string]string{"message": "Update is not a fast forward"})
			return
		}
		f.main = body.SHA
		json.NewEncoder(w).Encode(map[string]any{})
	case r.Method == "GET" && strings.HasPrefix(p, "/compare/"):
		a, _, _ := strings.Cut(strings.TrimPrefix(p, "/compare/"), "...")
		status := "diverged"
		if a == f.main {
			status = "identical"
		} else if f.ancestor(a, f.main) {
			status = "ahead"
		}
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	case r.Method == "POST" && p == "/check-runs":
		var cr CheckRun
		json.NewDecoder(r.Body).Decode(&cr)
		f.checks = append(f.checks, cr)
		w.WriteHeader(201)
		w.Write([]byte("{}"))
	default:
		w.WriteHeader(404)
	}
}

func TestForge(t *testing.T) {
	f := &fakeGitHub{main: "a", parents: map[string][]string{"a": nil, "b": {"a"}, "c": {"b"}, "m": {"a", "x"}, "x": {"a"}}}
	hs := httptest.NewServer(f)
	defer hs.Close()
	c := New(hs.URL, "o", "r", "s3cret")
	if h, err := c.Head(); err != nil || h != "a" {
		t.Fatalf("head %q %v", h, err)
	}
	if err := c.Advance("a", "c"); err == nil {
		t.Fatal("advanced by two commits")
	}
	if err := c.Advance("a", "m"); err == nil {
		t.Fatal("advanced to a merge commit")
	}
	if err := c.Advance("a", "b"); err != nil {
		t.Fatal(err)
	}
	// Someone else moved main to x' — fake: main is now b, try advancing
	// x (parent a) with stale old=a: the parent check passes but GitHub
	// refuses the non-fast-forward → ErrStale.
	if err := c.Advance("a", "x"); !errors.Is(err, queue.ErrStale) {
		t.Fatalf("stale advance: %v", err)
	}
	for sha, want := range map[string]bool{"a": true, "b": true, "x": false} {
		if got, err := c.Contains(sha); err != nil || got != want {
			t.Errorf("Contains(%s) = %v %v", sha, got, err)
		}
	}
	if err := c.ReportPR(context.Background(), queue.PR{ID: 1, Head: "h", State: queue.Ejected, Reason: "test_failed", FailedTest: []string{"//a:t"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.checks) != 1 || f.checks[0].Conclusion != "failure" || !strings.Contains(f.checks[0].Output.Summary, "//a:t") {
		t.Fatalf("checks %+v", f.checks)
	}
	for _, a := range f.auth {
		if a != "Bearer s3cret" {
			t.Fatalf("auth header %q", a)
		}
	}
	// Errors never carry the token.
	_, err := New(hs.URL, "o", "nope", "s3cret").Head()
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks token or is nil: %v", err)
	}
}

type fakeQueue struct{ calls []string }

func (q *fakeQueue) EnqueueSHA(_ context.Context, id queue.PRID, head string) error {
	q.calls = append(q.calls, "enqueue "+head)
	return nil
}
func (q *fakeQueue) Remove(_ context.Context, id queue.PRID) error {
	q.calls = append(q.calls, "remove")
	return errors.New("queue: PR 1 not queued")
}
func (q *fakeQueue) MainMoved(context.Context) error { q.calls = append(q.calls, "main"); return nil }

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestWebhook(t *testing.T) {
	q := &fakeQueue{}
	wh := &Webhook{Secret: []byte("hook"), Queue: q}
	send := func(event, body, sig string) int {
		r := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", event)
		r.Header.Set("X-Hub-Signature-256", sig)
		w := httptest.NewRecorder()
		wh.ServeHTTP(w, r)
		return w.Code
	}
	pr := func(action, label, sha string, labels ...string) string {
		var ls []map[string]string
		for _, l := range labels {
			ls = append(ls, map[string]string{"name": l})
		}
		ev := map[string]any{"action": action, "number": 1, "pull_request": map[string]any{
			"state": "open", "head": map[string]string{"sha": sha}, "base": map[string]string{"ref": "main"}, "labels": ls}}
		if label != "" {
			ev["label"] = map[string]string{"name": label}
		}
		b, _ := json.Marshal(ev)
		return string(b)
	}
	if c := send("pull_request", pr("labeled", "merge-queue", "s1", "merge-queue"), "sha256=00"); c != 401 {
		t.Fatalf("bad signature accepted: %d", c)
	}
	steps := []struct{ event, body string }{
		{"pull_request", pr("opened", "", "s0")},                            // not labeled: ignored
		{"pull_request", pr("labeled", "merge-queue", "s1", "merge-queue")}, // enqueue
		{"pull_request", pr("synchronize", "", "s2", "merge-queue")},        // update
		{"pull_request", pr("unlabeled", "merge-queue", "s2")},              // remove (not queued → ok)
		{"push", `{"ref":"refs/heads/main"}`},                               // main moved
		{"push", `{"ref":"refs/heads/feature"}`},                            // ignored
	}
	for _, s := range steps {
		if c := send(s.event, s.body, sign("hook", s.body)); c != 204 {
			t.Fatalf("%s: %d", s.body, c)
		}
	}
	if got := strings.Join(q.calls, ","); got != "enqueue s1,enqueue s2,remove,main" {
		t.Fatalf("calls: %s", got)
	}
}

func FuzzVerify(f *testing.F) {
	f.Add([]byte("body"), "sha256=00")
	f.Fuzz(func(t *testing.T, body []byte, header string) {
		if Verify([]byte("k"), body, header) && !strings.EqualFold(header, sign("k", string(body))) {
			t.Fatal("accepted a wrong signature")
		}
	})
}
