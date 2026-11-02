package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func setup(t *testing.T) (*Runner, *localgit.Work, chan queue.Result) {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	write := func(p, c string) {
		os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(src, p), []byte(c), 0o644)
	}
	write("a/BUILD.yaml", "targets:\n- {name: a, srcs: [a.txt], cmd: 'cp a/a.txt $OUT', outs: [a.out]}\n- name: a_test\n  deps: [':a']\n  cmd: grep -q ok a/a.out\n  test: true\n")
	write("a/a.txt", "ok\n")
	write("b/BUILD.yaml", "targets:\n- {name: slow, srcs: [b.txt], cmd: 'sleep 30'}\n")
	write("b/b.txt", "x\n")
	repo, err := localgit.Init(ctx, filepath.Join(t.TempDir(), "r.git"), src)
	if err != nil {
		t.Fatal(err)
	}
	w := &localgit.Work{Dir: t.TempDir(), Remote: repo.Dir}
	if err := w.Clone(ctx); err != nil {
		t.Fatal(err)
	}
	results := make(chan queue.Result, 10)
	r := &Runner{Repo: repo, CAS: cas.NewMemory(), AC: actioncache.NewMemory(), Results: results, Jobs: 2}
	t.Cleanup(r.Close)
	return r, w, results
}

func pr(t *testing.T, w *localgit.Work, r *Runner, name, file, content string) queue.Item {
	ctx := context.Background()
	w.Checkout(ctx, name, "main")
	w.WriteFile(file, content)
	w.CommitAll(ctx, name)
	if err := w.Push(ctx, name+":refs/heads/"+name); err != nil {
		t.Fatal(err)
	}
	h, _ := r.Repo.BranchHead(ctx, name)
	return queue.Item{PR: 1, Version: 1, Head: h}
}

func wait(t *testing.T, ch chan queue.Result) queue.Result {
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("no result")
	}
	return queue.Result{}
}

func TestPassFailConflict(t *testing.T) {
	r, w, results := setup(t)
	main, _ := r.Repo.Head()
	good := pr(t, w, r, "pr/good", "a/a.txt", "ok again\n")
	bad := pr(t, w, r, "pr/bad", "a/a.txt", "nope\n")

	r.Start(queue.Run{ID: 1, Base: main, Groups: [][]queue.Item{{good}}})
	res := wait(t, results)
	if res.Outcome != queue.Pass || res.Parent != main || res.Commit == "" {
		t.Fatalf("good: %+v", res)
	}
	r.Start(queue.Run{ID: 2, Base: main, Groups: [][]queue.Item{{bad}}})
	if res := wait(t, results); res.Outcome != queue.Fail || len(res.FailedTests) != 1 || res.FailedTests[0] != "//a:a_test" {
		t.Fatalf("bad: %+v", res)
	}
	// good then bad: textual conflict on a/a.txt.
	r.Start(queue.Run{ID: 3, Base: main, Groups: [][]queue.Item{{good}, {bad}}})
	if res := wait(t, results); res.FailedTests[0] != MergeConflict {
		t.Fatalf("conflict: %+v", res)
	}
}

func TestCancelKillsBuild(t *testing.T) {
	r, w, results := setup(t)
	main, _ := r.Repo.Head()
	slow := pr(t, w, r, "pr/slow", "b/b.txt", "y\n")
	r.Start(queue.Run{ID: 1, Base: main, Groups: [][]queue.Item{{slow}}})
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	r.Cancel(1)
	r.Close()
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the build promptly")
	}
	select {
	case res := <-results:
		t.Fatalf("cancelled run reported %+v", res)
	default:
	}
}
