// Package localgit is a forge backed by a bare git repository on disk,
// driven through the git CLI. PRs are branches refs/heads/pr/<id>; main is
// refs/heads/main. Candidate commits are built without a worktree using
// `git merge-tree --write-tree` and `git commit-tree` with fixed author and
// committer identities and dates, so they are deterministic.
package localgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/xartus2004/greenline/forge"
	"github.com/xartus2004/greenline/queue"
)

// Repo is a bare repository.
type Repo struct {
	Dir string // path to the bare repo
	mu  sync.Mutex
}

// Fixed identity and date for queue-made commits.
var commitEnv = []string{
	"GIT_AUTHOR_NAME=greenline", "GIT_AUTHOR_EMAIL=queue@greenline.invalid",
	"GIT_COMMITTER_NAME=greenline", "GIT_COMMITTER_EMAIL=queue@greenline.invalid",
	"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
}

// Git runs git against the repo and returns trimmed stdout.
func (r *Repo) Git(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", r.Dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(out.String()), &GitError{Args: args, Err: err, Stderr: errb.String(), Stdout: out.String()}
	}
	return strings.TrimSpace(out.String()), nil
}

// GitError carries git's stderr.
type GitError struct {
	Args           []string
	Err            error
	Stderr, Stdout string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

// ExitCode returns git's exit code (or -1).
func (e *GitError) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(e.Err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Init creates a bare repo at dir (if needed) whose main is a single
// commit of the files in srcDir.
func Init(ctx context.Context, dir, srcDir string) (*Repo, error) {
	r := &Repo{Dir: dir}
	if _, err := os.Stat(dir); err == nil {
		return r, nil
	}
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git init: %v: %s", err, out)
	}
	if srcDir == "" {
		return r, nil
	}
	work, err := os.MkdirTemp("", "greenline-init-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	w := &Work{Dir: work, Remote: dir}
	if err := w.Clone(ctx); err != nil {
		return nil, err
	}
	if err := copyDir(srcDir, work); err != nil {
		return nil, err
	}
	if err := w.CommitAll(ctx, "initial import"); err != nil {
		return nil, err
	}
	if err := w.Push(ctx, "HEAD:refs/heads/main"); err != nil {
		return nil, err
	}
	return r, nil
}

// Head implements queue.Forge.
func (r *Repo) Head() (string, error) {
	return r.Git(context.Background(), nil, "rev-parse", "--verify", "refs/heads/main^{commit}")
}

// Advance implements queue.Forge: main moves old → next only if next's
// sole parent is old, via an atomic `update-ref` with the old value.
func (r *Repo) Advance(old, next string) error {
	ctx := context.Background()
	r.mu.Lock()
	defer r.mu.Unlock()
	parents, err := r.Git(ctx, nil, "rev-list", "--parents", "-n1", next)
	if err != nil {
		return err
	}
	f := strings.Fields(parents)
	if len(f) != 2 || f[1] != old {
		return fmt.Errorf("localgit: refusing to advance main to %s: parents %v, want exactly %s", next, f[1:], old)
	}
	if _, err := r.Git(ctx, nil, "update-ref", "-m", "greenline merge", "refs/heads/main", next, old); err != nil {
		if head, herr := r.Head(); herr == nil && head != old {
			return queue.ErrStale
		}
		return err
	}
	return nil
}

// Contains implements queue.Forge (first-parent history of main).
func (r *Repo) Contains(commit string) (bool, error) {
	out, err := r.Git(context.Background(), nil, "rev-list", "--first-parent", "refs/heads/main")
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(out, "\n") {
		if l == commit {
			return true, nil
		}
	}
	return false, nil
}

// MainHistory returns main's first-parent history, oldest first.
func (r *Repo) MainHistory(ctx context.Context) ([]string, error) {
	out, err := r.Git(ctx, nil, "rev-list", "--first-parent", "--reverse", "refs/heads/main")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// BranchHead returns the commit of refs/heads/<branch>.
func (r *Repo) BranchHead(ctx context.Context, branch string) (string, error) {
	return r.Git(ctx, nil, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
}

// Apply implements forge.Merger: it merges each item's head into parent in
// order (three-way, using merge-tree's own merge base) and records the
// result as ONE commit whose only parent is parent.
func (r *Repo) Apply(parent string, group []queue.Item) (string, error) {
	ctx := context.Background()
	cur := parent
	var msg strings.Builder
	msg.WriteString("greenline: merge")
	for _, it := range group {
		fmt.Fprintf(&msg, " #%d", it.PR)
	}
	msg.WriteString("\n\n")
	for _, it := range group {
		fmt.Fprintf(&msg, "PR #%d v%d at %s\n", it.PR, it.Version, it.Head)
		tree, err := r.Git(ctx, nil, "merge-tree", "--write-tree", "--no-messages", cur, it.Head)
		if err != nil {
			var ge *GitError
			if errors.As(err, &ge) && ge.ExitCode() == 1 {
				return "", &forge.ConflictError{PR: it.PR}
			}
			return "", err
		}
		tree = strings.Fields(tree)[0]
		// Intermediate commit so the next merge-tree has a proper base.
		cur, err = r.Git(ctx, commitEnv, "commit-tree", tree, "-p", cur, "-p", it.Head, "-m", "intermediate")
		if err != nil {
			return "", err
		}
	}
	tree, err := r.Git(ctx, nil, "rev-parse", cur+"^{tree}")
	if err != nil {
		return "", err
	}
	return r.Git(ctx, commitEnv, "commit-tree", tree, "-p", parent, "-m", msg.String())
}

// Export writes the tree of commit into dir.
func (r *Repo) Export(ctx context.Context, commit, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	arch := exec.CommandContext(ctx, "git", "--git-dir", r.Dir, "archive", "--format=tar", commit)
	tar := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
	pipe, err := arch.StdoutPipe()
	if err != nil {
		return err
	}
	tar.Stdin = pipe
	var errb, tarErr bytes.Buffer
	arch.Stderr, tar.Stderr = &errb, &tarErr
	if err := tar.Start(); err != nil {
		return err
	}
	if err := arch.Run(); err != nil {
		tar.Wait()
		return fmt.Errorf("git archive %s: %v: %s", commit, err, errb.String())
	}
	if err := tar.Wait(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, tarErr.String())
	}
	return nil
}

var _ queue.Forge = (*Repo)(nil)
var _ forge.Merger = (*Repo)(nil)
