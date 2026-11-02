package affected

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xartus2004/greenline/build"
)

func load(t *testing.T, files map[string]string) *build.Workspace {
	root := t.TempDir()
	for p, c := range files {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(c), 0o644)
	}
	ws, err := build.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

var base = map[string]string{
	"a/BUILD.yaml": "targets:\n- {name: a, srcs: [a.txt], cmd: x}\n",
	"a/a.txt":      "1",
	"b/BUILD.yaml": "targets:\n- {name: b, srcs: [b.txt], deps: ['//a:a'], cmd: x}\n",
	"b/b.txt":      "1",
	"c/BUILD.yaml": "targets:\n- {name: c, srcs: ['*.txt'], cmd: x}\n",
	"c/c.txt":      "1",
	"README":       "hi",
}

func TestTargets(t *testing.T) {
	bw := load(t, base)
	cases := []struct {
		changed []string
		head    map[string]string
		want    string
	}{
		{[]string{"a/a.txt"}, nil, "//a:a,//b:b"},
		{[]string{"b/b.txt"}, nil, "//b:b"},
		{[]string{"README"}, nil, ""},
		{[]string{"c/new.txt"}, map[string]string{"c/new.txt": "n"}, "//c:c"},
		{[]string{"b/BUILD.yaml"}, map[string]string{"b/BUILD.yaml": "targets:\n- {name: b, srcs: [b.txt], deps: ['//a:a'], cmd: y}\n"}, "//b:b"},
	}
	for _, c := range cases {
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		for k, v := range c.head {
			files[k] = v
		}
		got := strings.Join(Targets(bw, load(t, files), c.changed), ",")
		if got != c.want {
			t.Errorf("changed %v: got %q want %q", c.changed, got, c.want)
		}
	}
}

func TestDisjoint(t *testing.T) {
	if !Disjoint([]string{"a", "c"}, []string{"b", "d"}) || Disjoint([]string{"a", "c"}, []string{"c"}) || !Disjoint(nil, []string{"a"}) {
		t.Fatal("Disjoint wrong")
	}
}
