package localgit

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// Work is a non-bare clone used to author PR branches (demo and tests).
type Work struct {
	Dir    string
	Remote string
}

func (w *Work) git(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = w.Dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=demo", "GIT_AUTHOR_EMAIL=demo@greenline.invalid",
		"GIT_COMMITTER_NAME=demo", "GIT_COMMITTER_EMAIL=demo@greenline.invalid")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %v: %v: %s", args, err, out.String())
	}
	return nil
}

// Clone clones the remote into Dir (which may be empty).
func (w *Work) Clone(ctx context.Context) error {
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	if err := w.git(ctx, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := w.git(ctx, "remote", "add", "origin", w.Remote); err != nil {
		return err
	}
	_ = w.git(ctx, "fetch", "-q", "origin")
	_ = w.git(ctx, "checkout", "-q", "-B", "main", "origin/main")
	return nil
}

// Checkout creates/resets a local branch at origin/<from>.
func (w *Work) Checkout(ctx context.Context, branch, from string) error {
	if err := w.git(ctx, "fetch", "-q", "origin"); err != nil {
		return err
	}
	return w.git(ctx, "checkout", "-q", "-B", branch, "origin/"+from)
}

// CommitAll stages everything and commits.
func (w *Work) CommitAll(ctx context.Context, msg string) error {
	if err := w.git(ctx, "add", "-A"); err != nil {
		return err
	}
	return w.git(ctx, "commit", "-q", "--allow-empty", "-m", msg)
}

// Push pushes a refspec (force).
func (w *Work) Push(ctx context.Context, refspec string) error {
	return w.git(ctx, "push", "-q", "-f", "origin", refspec)
}

// WriteFile writes a file relative to the work tree.
func (w *Work) WriteFile(rel, content string) error {
	p := filepath.Join(w.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// Remove deletes a file relative to the work tree.
func (w *Work) Remove(rel string) error { return os.Remove(filepath.Join(w.Dir, rel)) }

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode().Perm())
	})
}
