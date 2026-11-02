// Package build is a small, self-contained build tool so greenline does not
// depend on Bazel. Each directory may contain a BUILD.yaml declaring targets:
//
//	targets:
//	  - name: core
//	    srcs: [core.txt, "*.in"]
//	    deps: [//lib/util:util, :other]
//	    cmd: cat $SRCS > $OUT
//	    outs: [core.out]
//	  - name: core_test
//	    srcs: [core_test.sh]
//	    deps: [:core]
//	    cmd: sh core_test.sh
//	    test: true
//
// Every action runs in a fresh temp directory holding only its declared
// inputs (its srcs plus the outputs of its direct and transitive deps), with
// a scrubbed environment. Its cache key is the sha256 of the command, the
// allowlisted environment and the digests of every input file, so a change
// to any input yields a new key and everything else is a cache hit.
package build

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// BuildFile is the name of per-directory build files.
const BuildFile = "BUILD.yaml"

// Target is one declared build or test target.
type Target struct {
	Label string   `yaml:"-" json:"label"`
	Dir   string   `yaml:"-" json:"dir"` // workspace-relative, "" for root
	Name  string   `yaml:"name" json:"name"`
	Srcs  []string `yaml:"srcs" json:"srcs"` // as written (may be globs)
	Deps  []string `yaml:"deps" json:"deps"` // as written
	Cmd   string   `yaml:"cmd" json:"cmd"`
	Outs  []string `yaml:"outs" json:"outs"`
	Test  bool     `yaml:"test" json:"test"`

	// Resolved fields.
	SrcFiles  []string `yaml:"-" json:"src_files"` // workspace-relative, sorted
	DepLabels []string `yaml:"-" json:"dep_labels"`
	OutFiles  []string `yaml:"-" json:"out_files"` // workspace-relative
}

// Workspace is a loaded set of targets.
type Workspace struct {
	Root    string
	Targets map[string]*Target // by label
	// Owner maps a workspace-relative source file to the labels using it.
	Owner map[string][]string
	// Files maps build file path → labels declared in it.
	Files map[string][]string
}

type buildFile struct {
	Targets []*Target `yaml:"targets"`
}

// Load parses every BUILD.yaml under root and resolves labels and globs.
func Load(root string) (*Workspace, error) {
	ws := &Workspace{Root: root, Targets: map[string]*Target{}, Owner: map[string][]string{}, Files: map[string][]string{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "greenline-out") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != BuildFile {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		return ws.loadFile(p, rel)
	})
	if err != nil {
		return nil, err
	}
	for _, t := range ws.Targets {
		for _, dep := range t.Deps {
			l, err := resolveLabel(t.Dir, dep)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t.Label, err)
			}
			if _, ok := ws.Targets[l]; !ok {
				return nil, fmt.Errorf("%s: unknown dep %s", t.Label, l)
			}
			t.DepLabels = append(t.DepLabels, l)
		}
		sort.Strings(t.DepLabels)
	}
	if err := ws.checkCycles(); err != nil {
		return nil, err
	}
	return ws, nil
}

func (ws *Workspace) loadFile(p, dir string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var bf buildFile
	if err := yaml.Unmarshal(b, &bf); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	bfRel := path.Join(dir, BuildFile)
	for _, t := range bf.Targets {
		if t.Name == "" || strings.ContainsAny(t.Name, ":/ ") {
			return fmt.Errorf("%s: bad target name %q", p, t.Name)
		}
		t.Dir = dir
		t.Label = "//" + dir + ":" + t.Name
		if _, dup := ws.Targets[t.Label]; dup {
			return fmt.Errorf("duplicate target %s", t.Label)
		}
		if t.Cmd == "" {
			return fmt.Errorf("%s: empty cmd", t.Label)
		}
		seen := map[string]bool{}
		for _, s := range t.Srcs {
			files, err := ws.expand(dir, s)
			if err != nil {
				return fmt.Errorf("%s: %w", t.Label, err)
			}
			for _, f := range files {
				if !seen[f] {
					seen[f] = true
					t.SrcFiles = append(t.SrcFiles, f)
					ws.Owner[f] = append(ws.Owner[f], t.Label)
				}
			}
		}
		sort.Strings(t.SrcFiles)
		for _, o := range t.Outs {
			if err := checkRel(o); err != nil {
				return fmt.Errorf("%s: out %w", t.Label, err)
			}
			t.OutFiles = append(t.OutFiles, path.Join(dir, o))
		}
		ws.Targets[t.Label] = t
		ws.Files[bfRel] = append(ws.Files[bfRel], t.Label)
	}
	return nil
}

func checkRel(p string) error {
	if p == "" || path.IsAbs(p) || strings.HasPrefix(path.Clean(p), "..") || strings.Contains(p, "\\") {
		return fmt.Errorf("%q must be a relative path inside the package", p)
	}
	return nil
}

func (ws *Workspace) expand(dir, pattern string) ([]string, error) {
	if err := checkRel(pattern); err != nil {
		return nil, err
	}
	full := filepath.Join(ws.Root, filepath.FromSlash(dir), filepath.FromSlash(pattern))
	if !strings.ContainsAny(pattern, "*?[") {
		st, err := os.Stat(full)
		if err != nil {
			return nil, fmt.Errorf("src %s: %w", pattern, err)
		}
		if st.IsDir() {
			return nil, fmt.Errorf("src %s is a directory", pattern)
		}
		return []string{path.Join(dir, pattern)}, nil
	}
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && !st.IsDir() && filepath.Base(m) != BuildFile {
			rel, _ := filepath.Rel(ws.Root, m)
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out, nil
}

func resolveLabel(dir, l string) (string, error) {
	switch {
	case strings.HasPrefix(l, ":"):
		return "//" + dir + l, nil
	case strings.HasPrefix(l, "//"):
		pkg, name, ok := strings.Cut(l[2:], ":")
		if !ok {
			name = path.Base(pkg)
		}
		if strings.Contains(pkg, "..") {
			return "", fmt.Errorf("bad label %q", l)
		}
		return "//" + pkg + ":" + name, nil
	}
	return "", fmt.Errorf("bad label %q (want //pkg:name or :name)", l)
}

// ErrCycle reports a dependency cycle.
var ErrCycle = errors.New("build: dependency cycle")

func (ws *Workspace) checkCycles() error {
	state := map[string]int{}
	var visit func(string, []string) error
	visit = func(l string, stack []string) error {
		switch state[l] {
		case 1:
			return fmt.Errorf("%w: %s", ErrCycle, strings.Join(append(stack, l), " -> "))
		case 2:
			return nil
		}
		state[l] = 1
		for _, d := range ws.Targets[l].DepLabels {
			if err := visit(d, append(stack, l)); err != nil {
				return err
			}
		}
		state[l] = 2
		return nil
	}
	for _, l := range ws.Labels() {
		if err := visit(l, nil); err != nil {
			return err
		}
	}
	return nil
}

// Labels returns all labels sorted.
func (ws *Workspace) Labels() []string {
	out := make([]string, 0, len(ws.Targets))
	for l := range ws.Targets {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Closure returns labels plus all their transitive deps, sorted
// topologically (deps first, ties broken by label).
func (ws *Workspace) Closure(labels []string) ([]string, error) {
	seen := map[string]bool{}
	var order []string
	var visit func(string) error
	visit = func(l string) error {
		if seen[l] {
			return nil
		}
		t, ok := ws.Targets[l]
		if !ok {
			return fmt.Errorf("unknown target %s", l)
		}
		seen[l] = true
		for _, d := range t.DepLabels {
			if err := visit(d); err != nil {
				return err
			}
		}
		order = append(order, l)
		return nil
	}
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	for _, l := range sorted {
		if err := visit(l); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// ReverseDeps returns the map label → labels that directly depend on it.
func (ws *Workspace) ReverseDeps() map[string][]string {
	r := map[string][]string{}
	for _, t := range ws.Targets {
		for _, d := range t.DepLabels {
			r[d] = append(r[d], t.Label)
		}
	}
	for k := range r {
		sort.Strings(r[k])
	}
	return r
}
