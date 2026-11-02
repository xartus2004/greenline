package localgit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xartus2004/greenline/forge"
	"github.com/xartus2004/greenline/queue"
)

func setup(t *testing.T) (*Repo, *Work) {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(src, "b.txt"), []byte("b\n"), 0o644)
	r, err := Init(ctx, filepath.Join(t.TempDir(), "repo.git"), src)
	if err != nil {
		t.Fatal(err)
	}
	w := &Work{Dir: t.TempDir(), Remote: r.Dir}
	if err := w.Clone(ctx); err != nil {
		t.Fatal(err)
	}
	return r, w
}

func branch(t *testing.T, w *Work, name, file, content string) {
	t.Helper()
	ctx := context.Background()
	if err := w.Checkout(ctx, name, "main"); err != nil {
		t.Fatal(err)
	}
	w.WriteFile(file, content)
	if err := w.CommitAll(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := w.Push(ctx, name+":refs/heads/"+name); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAdvanceContains(t *testing.T) {
	ctx := context.Background()
	r, w := setup(t)
	branch(t, w, "pr/1", "a.txt", "a1\n")
	branch(t, w, "pr/2", "b.txt", "b2\n")
	branch(t, w, "pr/3", "a.txt", "a3\n") // conflicts with pr/1
	h1, _ := r.BranchHead(ctx, "pr/1")
	h2, _ := r.BranchHead(ctx, "pr/2")
	h3, _ := r.BranchHead(ctx, "pr/3")
	main, _ := r.Head()

	c1, err := r.Apply(main, []queue.Item{{PR: 1, Head: h1}})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := r.Apply(main, []queue.Item{{PR: 1, Head: h1}})
	if again != c1 {
		t.Fatal("Apply not deterministic")
	}
	c12, err := r.Apply(c1, []queue.Item{{PR: 2, Head: h2}})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := r.Apply(main, []queue.Item{{PR: 1, Head: h1}, {PR: 2, Head: h2}})
	if err != nil {
		t.Fatal(err)
	}
	if bt, _ := r.Git(ctx, nil, "rev-parse", batch+"^{tree}"); bt != mustTree(t, r, c12) {
		t.Fatal("batch tree differs from sequential")
	}
	if _, err := r.Apply(c1, []queue.Item{{PR: 3, Head: h3}}); !isConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// Non-fast-forward and stale advances are refused.
	if err := r.Advance(main, c12); err == nil {
		t.Fatal("advanced by two commits")
	}
	if err := r.Advance(main, c1); err != nil {
		t.Fatal(err)
	}
	if err := r.Advance(main, c1); err == nil {
		t.Fatal("stale advance accepted")
	}
	if err := r.Advance(c1, c12); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{main, c1, c12} {
		if ok, _ := r.Contains(c); !ok {
			t.Fatalf("history missing %s", c)
		}
	}
	if ok, _ := r.Contains(batch); ok {
		t.Fatal("contains unmerged commit")
	}
	dir := t.TempDir()
	if err := r.Export(ctx, c12, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(b) != "b2\n" {
		t.Fatalf("export: %q", b)
	}
}

func mustTree(t *testing.T, r *Repo, c string) string {
	tr, err := r.Git(context.Background(), nil, "rev-parse", c+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func isConflict(err error) bool {
	var ce *forge.ConflictError
	return errors.As(err, &ce)
}
