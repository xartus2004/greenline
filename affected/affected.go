// Package affected computes which build targets a change can influence:
// the targets owning a changed file (or whose definition changed), plus
// their reverse-dependency closure. Two changes with disjoint affected sets
// cannot change each other's build results, which is what makes
// independent merge-queue lanes safe to test in parallel.
package affected

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/xartus2004/greenline/build"
)

// Targets returns the sorted affected labels in head for a change from base
// to head touching changed (workspace-relative, slash-separated paths).
// base may be nil (everything in head is new).
func Targets(base, head *build.Workspace, changed []string) []string {
	direct := map[string]bool{}
	for _, f := range changed {
		for _, l := range head.Owner[f] {
			direct[l] = true
		}
		if base != nil {
			// A file deleted or moved out of a glob still affects its
			// former owner if that target still exists.
			for _, l := range base.Owner[f] {
				if _, ok := head.Targets[l]; ok {
					direct[l] = true
				}
			}
		}
	}
	for l, t := range head.Targets {
		if base == nil {
			direct[l] = true
			continue
		}
		bt, ok := base.Targets[l]
		if !ok || defn(bt) != defn(t) {
			direct[l] = true
		}
	}
	rdeps := head.ReverseDeps()
	out := map[string]bool{}
	var walk func(string)
	walk = func(l string) {
		if out[l] {
			return
		}
		out[l] = true
		for _, r := range rdeps[l] {
			walk(r)
		}
	}
	for l := range direct {
		walk(l)
	}
	res := make([]string, 0, len(out))
	for l := range out {
		res = append(res, l)
	}
	sort.Strings(res)
	return res
}

func defn(t *build.Target) string {
	b, _ := json.Marshal(struct {
		Cmd  string
		Srcs []string
		Deps []string
		Outs []string
		Test bool
	}{t.Cmd, t.SrcFiles, t.DepLabels, t.OutFiles, t.Test})
	return string(b)
}

// Disjoint reports whether two sorted label sets share no element.
func Disjoint(a, b []string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return false
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return true
}

// ChangedFiles lists files that differ between two commits of a git repo.
func ChangedFiles(ctx context.Context, repo, base, head string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--name-only", "--no-renames", "-z", base, head)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff: %v: %s", err, errb.String())
	}
	var files []string
	for _, f := range strings.Split(out.String(), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}
