package build

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
)

// copyTree copies examples/mono into a temp dir so tests can mutate it.
func copyTree(t testing.TB, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func newBuilder(t testing.TB, root string, st cas.Store, raw actioncache.Raw) *Builder {
	ws, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Builder{WS: ws, CAS: st, Cache: actioncache.Typed{Raw: raw, CAS: st}, Jobs: 8}
}

func executed(rep *Report) []string {
	var out []string
	for l, r := range rep.Results {
		if !r.Cached && !r.Skipped {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func TestMonoColdWarmAndLeafChange(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real shell actions")
	}
	ctx := context.Background()
	root := copyTree(t, "../examples/mono")
	st, raw := cas.NewMemory(), actioncache.NewMemory()
	b := newBuilder(t, root, st, raw)
	all := b.WS.Labels()
	if len(all) != 15 {
		t.Fatalf("expected 15 targets, got %d", len(all))
	}

	cold, err := b.Build(ctx, all)
	if err != nil || !cold.OK() {
		t.Fatalf("cold build: %v failed=%v", err, cold.Failed())
	}
	if h, e := cold.Hits(); h != 0 || e != 15 {
		t.Fatalf("cold: hits=%d executed=%d", h, e)
	}

	warm, err := newBuilder(t, root, st, raw).Build(ctx, all)
	if err != nil || !warm.OK() {
		t.Fatal(err)
	}
	if h, e := warm.Hits(); h != 15 || e != 0 {
		t.Fatalf("warm: hits=%d executed=%d", h, e)
	}
	if warm.Duration > cold.Duration/2 {
		t.Errorf("warm build not faster: cold=%v warm=%v", cold.Duration, warm.Duration)
	}

	// Change a leaf: only its reverse deps rebuild.
	os.WriteFile(filepath.Join(root, "services/worker/jobs.txt"), []byte("build\ntest\nmerge\ndeploy\n"), 0o644)
	inc, err := newBuilder(t, root, st, raw).Build(ctx, all)
	if err != nil || !inc.OK() {
		t.Fatal(err, inc.Failed())
	}
	want := []string{"//app:app", "//app:app_test", "//services/worker:worker", "//services/worker:worker_test"}
	if got := executed(inc); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("incremental rebuilt %v, want %v", got, want)
	}

	// Breaking change: test fails, result is not cached, and the failure
	// reproduces (no false cache hit).
	os.WriteFile(filepath.Join(root, "services/worker/jobs.txt"), []byte("build\nbuild\n"), 0o644)
	for i := 0; i < 2; i++ {
		bad, err := newBuilder(t, root, st, raw).Build(ctx, all)
		if err != nil {
			t.Fatal(err)
		}
		if f := bad.FailedTests(); len(f) != 1 || f[0] != "//services/worker:worker_test" {
			t.Fatalf("run %d: failed tests %v", i, f)
		}
		if bad.Results["//services/worker:worker_test"].Cached {
			t.Fatal("failed test served from cache")
		}
	}
}

func writeWS(t *testing.T, files map[string]string) string {
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	return root
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"cycle":       {"BUILD.yaml": "targets:\n- {name: a, cmd: x, deps: [':b']}\n- {name: b, cmd: x, deps: [':a']}\n"},
		"unknown dep": {"BUILD.yaml": "targets:\n- {name: a, cmd: x, deps: ['//nope:x']}\n"},
		"escape src":  {"p/BUILD.yaml": "targets:\n- {name: a, cmd: x, srcs: ['../secret']}\n", "secret": "s"},
		"abs out":     {"BUILD.yaml": "targets:\n- {name: a, cmd: x, outs: ['/etc/passwd']}\n"},
		"missing src": {"BUILD.yaml": "targets:\n- {name: a, cmd: x, srcs: [nope.txt]}\n"},
		"dup":         {"BUILD.yaml": "targets:\n- {name: a, cmd: x}\n- {name: a, cmd: y}\n"},
	}
	for name, files := range cases {
		if _, err := Load(writeWS(t, files)); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}
}

func TestHermeticSandbox(t *testing.T) {
	ctx := context.Background()
	root := writeWS(t, map[string]string{
		"BUILD.yaml": "targets:\n- {name: a, srcs: [in.txt], cmd: 'ls -R . > $OUT; [ ! -e undeclared.txt ]', outs: [o]}\n",
		"in.txt":     "x", "undeclared.txt": "y",
	})
	b := newBuilder(t, root, cas.NewMemory(), actioncache.NewMemory())
	rep, err := b.Build(ctx, []string{"//:a"})
	if err != nil || !rep.OK() {
		t.Fatalf("%v %+v", err, rep.Results["//:a"])
	}
}

func TestMissingOutputFailsAndTimeout(t *testing.T) {
	ctx := context.Background()
	root := writeWS(t, map[string]string{
		"BUILD.yaml": "targets:\n- {name: a, cmd: 'true', outs: [o]}\n- {name: slow, cmd: 'sleep 30'}\n",
	})
	b := newBuilder(t, root, cas.NewMemory(), actioncache.NewMemory())
	b.KeepGoing = true
	b.Timeout = 300 * time.Millisecond
	start := time.Now()
	rep, err := b.Build(ctx, []string{"//:a", "//:slow"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Results["//:a"].OK() {
		t.Fatal("missing output accepted")
	}
	if r := rep.Results["//:slow"]; r.OK() || r.Err != "timeout" {
		t.Fatalf("slow: %+v", r)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	root := writeWS(t, map[string]string{
		// The child sleeps in a subshell; killing only sh would leak it.
		"BUILD.yaml": "targets:\n- {name: a, cmd: '(sleep 30; touch /tmp/greenline-leak-$$) & wait'}\n",
	})
	b := newBuilder(t, root, cas.NewMemory(), actioncache.NewMemory())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := b.Build(ctx, []string{"//:a"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not kill the action promptly")
	}
}

func TestActionKeySensitivity(t *testing.T) {
	b := &Builder{EnvAllow: []string{}}
	tg := &Target{Cmd: "c", OutFiles: []string{"o"}}
	in := map[string]cas.Digest{"a": cas.Of([]byte("1"))}
	k1 := b.ActionKey(tg, in)
	if k1 != b.ActionKey(tg, map[string]cas.Digest{"a": cas.Of([]byte("1"))}) {
		t.Fatal("key not deterministic")
	}
	variants := []string{
		b.ActionKey(&Target{Cmd: "c2", OutFiles: []string{"o"}}, in),
		b.ActionKey(tg, map[string]cas.Digest{"a": cas.Of([]byte("2"))}),
		b.ActionKey(tg, map[string]cas.Digest{"b": cas.Of([]byte("1"))}),
		b.ActionKey(&Target{Cmd: "c", OutFiles: []string{"p"}}, in),
		b.ActionKey(&Target{Cmd: "c", OutFiles: []string{"o"}, Test: true}, in),
	}
	for i, v := range variants {
		if v == k1 {
			t.Errorf("variant %d collides", i)
		}
	}
}
