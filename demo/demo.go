// Package demo builds a throwaway git repo from examples/mono, opens 20 PR
// branches with known properties (good, independently broken, broken only
// in combination, textually conflicting, flaky, fixed while queued), runs
// the real queue on them with real builds through the cache, and then
// rebuilds every commit on main from scratch to prove it stayed green.
package demo

import (
	"context"
	"fmt"
	"strings"

	"github.com/xartus2004/greenline/forge/localgit"
)

// PR describes one demo pull request.
type PR struct {
	ID    int
	Title string
	Kind  string // good, broken, pair, conflict, flaky, fixed
	Files map[string]string
	// Delete lists files to remove.
	Delete []string
	// Fix, if set, is pushed as an update shortly after enqueueing.
	Fix map[string]string
}

// Scenario returns the 20 demo PRs. File contents are complete
// replacements, written relative to the repo root.
func Scenario() []PR {
	return []PR{
		{ID: 1, Title: "docs: add usage line", Kind: "good", Files: map[string]string{"docs/guide.md": "# guide\ngreenline keeps main green.\nRun greenline demo to see it.\n"}},
		{ID: 2, Title: "gen: add email field", Kind: "good", Files: map[string]string{"tools/gen/schema.txt": "id:int\nname:string\nemail:string\n"}},
		{ID: 3, Title: "mathx: add 5", Kind: "good", Files: map[string]string{"lib/mathx/nums.txt": "1\n2\n3\n4\n5\n"}},
		{ID: 4, Title: "api: add /metrics route", Kind: "good", Files: map[string]string{"services/api/routes.txt": "GET /status\nPOST /enqueue\nGET /metrics\n"}},
		{ID: 5, Title: "worker: add lint job", Kind: "good", Files: map[string]string{"services/worker/jobs.d/lint.txt": "lint\n"}},
		{ID: 6, Title: "strutil: add 'lane'", Kind: "good", Files: map[string]string{"lib/strutil/words.txt": "merge\nqueue\ngreen\nlane\n"}},
		{ID: 7, Title: "api: drop /status (breaks api_test)", Kind: "broken", Files: map[string]string{"services/api/BUILD.yaml": apiBuild, "services/api/removed_routes.txt": "GET /status\n"}},
		{ID: 8, Title: "textfmt: lose banner prefix (breaks textfmt_test)", Kind: "broken", Files: map[string]string{"lib/textfmt/template.txt": "banner\n"}},
		{ID: 9, Title: "worker: add deploy job (a)", Kind: "pair", Files: map[string]string{"services/worker/jobs.d/deploy-a.txt": "deploy\n"}},
		{ID: 10, Title: "worker: add deploy job (b)", Kind: "pair", Files: map[string]string{"services/worker/jobs.d/deploy-b.txt": "deploy\n"}},
		{ID: 11, Title: "docs: reword tagline (conflicts with #1)", Kind: "conflict", Files: map[string]string{"docs/guide.md": "# guide\ngreenline keeps main green, always.\n"}},
		{ID: 12, Title: "gen: add flaky smoke test", Kind: "flaky", Files: map[string]string{"tools/gen/BUILD.yaml": genBuildFlaky}},
		{ID: 13, Title: "app: bump version", Kind: "good", Files: map[string]string{"app/VERSION": "1.0.1\n"}},
		{ID: 14, Title: "api: stricter test", Kind: "good", Files: map[string]string{"services/api/api_test.sh": "set -e\ngrep -q '^GET /status$' services/api/api.bin\ngrep -q '^routes=' services/api/api.bin\ngrep -q '^weight=' services/api/api.bin\n"}},
		{ID: 15, Title: "worker: duplicate job (fixed while queued)", Kind: "fixed",
			Files: map[string]string{"services/worker/jobs.txt": "build\ntest\nmerge\nbuild\n"},
			Fix:   map[string]string{"services/worker/jobs.txt": "build\ntest\nmerge\npackage\n"}},
		{ID: 16, Title: "docs: add FAQ", Kind: "good", Files: map[string]string{"docs/faq.md": "# faq\nWhy? Because main should be green.\n"}},
		{ID: 17, Title: "gen: test email field optionality", Kind: "good", Files: map[string]string{"tools/gen/gen_test.sh": "set -e\ngrep -q '^field id int$' tools/gen/schema.gen\ngrep -q '^field name string$' tools/gen/schema.gen\n"}},
		{ID: 18, Title: "mathx: bound check", Kind: "good", Files: map[string]string{"lib/mathx/mathx_test.sh": "set -e\nn=$(cat lib/mathx/sum.out)\n[ \"$n\" -gt 0 ] || { echo \"sum must be positive, got $n\"; exit 1; }\n[ \"$n\" -lt 1000 ]\n"}},
		{ID: 19, Title: "app: impossible assertion (breaks app_test)", Kind: "broken", Files: map[string]string{"app/app_test.sh": "set -e\ngrep -q '^jobs=99$' app/app.bundle\n"}},
		{ID: 20, Title: "worker: add report job", Kind: "good", Files: map[string]string{"services/worker/jobs.d/report.txt": "report\n"}},
	}
}

const apiBuild = `targets:
  - name: api
    srcs: [routes.txt, removed_routes.txt]
    deps: [//lib/textfmt:textfmt, //lib/mathx:mathx]
    cmd: |
      sleep 0.3; { cat lib/textfmt/banner.out; echo "routes=$(cat $SRCS | wc -l)"; echo "weight=$(cat lib/mathx/sum.out)"; grep -vxF -f services/api/removed_routes.txt services/api/routes.txt; } > $OUT
    outs: [api.bin]
  - name: api_test
    srcs: [api_test.sh]
    deps: [":api"]
    cmd: |
      sh services/api/api_test.sh
    test: true
`

const genBuildFlaky = `targets:
  - name: gen
    srcs: [schema.txt]
    cmd: |
      sleep 0.2; awk -F: '{print "field " $1 " " $2}' $SRCS > $OUT
    outs: [schema.gen]
  - name: gen_test
    srcs: [gen_test.sh]
    deps: [":gen"]
    cmd: |
      sh tools/gen/gen_test.sh
    test: true
  - name: gen_smoke_test
    deps: [":gen"]
    # Flaky on purpose: fails ~35% of runs regardless of inputs.
    cmd: |
      [ "$(od -An -N1 -tu1 /dev/urandom | tr -d ' ')" -ge 90 ]
    test: true
`

// Push creates or updates refs/heads/pr/<id> with the PR's files on top of
// main (or, for fixes, on top of the PR's current head).
func Push(ctx context.Context, w *localgit.Work, p PR, files map[string]string, onTop bool) error {
	branch := fmt.Sprintf("pr/%d", p.ID)
	from := "main"
	if onTop {
		from = branch
	}
	if err := w.Checkout(ctx, branch, from); err != nil {
		return err
	}
	for f, c := range files {
		if err := w.WriteFile(f, c); err != nil {
			return err
		}
	}
	if !onTop {
		for _, f := range p.Delete {
			w.Remove(f)
		}
	}
	msg := p.Title
	if onTop {
		msg = "fix: " + strings.TrimSuffix(p.Title, " (fixed while queued)")
	}
	if err := w.CommitAll(ctx, msg); err != nil {
		return err
	}
	return w.Push(ctx, branch+":refs/heads/"+branch)
}
