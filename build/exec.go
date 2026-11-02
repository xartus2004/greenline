package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
)

// KeyVersion is mixed into every action key; bump it to invalidate caches
// when action semantics change.
const KeyVersion = "greenline-build-v1"

// DefaultEnvAllow lists host environment variables passed into actions.
var DefaultEnvAllow = []string{"PATH"}

// Builder executes targets against a cache.
type Builder struct {
	WS    *Workspace
	CAS   cas.Store
	Cache actioncache.Cache
	// Jobs bounds concurrently running actions (default 4).
	Jobs int
	// EnvAllow lists host env vars whose values are passed to actions and
	// included in the action key.
	EnvAllow []string
	// NoCache disables cache lookups (results are still uploaded unless
	// NoUpload).
	NoCache, NoUpload bool
	// KeepGoing continues building independent targets after a failure.
	KeepGoing bool
	// Timeout bounds a single action (0 = none).
	Timeout time.Duration
	Log     *slog.Logger
}

// ActionResult is the outcome of one target.
type ActionResult struct {
	Label    string                   `json:"label"`
	Key      string                   `json:"key"`
	Test     bool                     `json:"test"`
	Cached   bool                     `json:"cached"`
	Skipped  bool                     `json:"skipped,omitempty"` // a dep failed
	ExitCode int                      `json:"exit_code"`
	Duration time.Duration            `json:"duration"`
	Stdout   string                   `json:"stdout,omitempty"`
	Stderr   string                   `json:"stderr,omitempty"`
	Err      string                   `json:"err,omitempty"`
	Outputs  []actioncache.OutputFile `json:"outputs,omitempty"`
}

// OK reports whether the action succeeded.
func (r *ActionResult) OK() bool { return !r.Skipped && r.ExitCode == 0 && r.Err == "" }

// Report summarises a build.
type Report struct {
	Results  map[string]*ActionResult `json:"results"`
	Order    []string                 `json:"order"`
	Duration time.Duration            `json:"duration"`
}

// Hits counts cache hits; Executed counts actions actually run.
func (r *Report) Hits() (hits, executed int) {
	for _, a := range r.Results {
		if a.Cached {
			hits++
		} else if !a.Skipped {
			executed++
		}
	}
	return
}

// Failed returns labels of failed (not skipped) actions, sorted.
func (r *Report) Failed() []string {
	var out []string
	for l, a := range r.Results {
		if !a.OK() && !a.Skipped {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// FailedTests returns labels of failed test targets.
func (r *Report) FailedTests() []string {
	var out []string
	for _, l := range r.Failed() {
		if r.Results[l].Test {
			out = append(out, l)
		}
	}
	return out
}

// OK reports whether every action succeeded.
func (r *Report) OK() bool {
	for _, a := range r.Results {
		if !a.OK() {
			return false
		}
	}
	return true
}

// ErrInfra marks failures of the build system itself (cache down, disk
// full) as opposed to a target failing. The queue retries these instead of
// ejecting a PR.
var ErrInfra = errors.New("build: infrastructure error")

type node struct {
	done chan struct{}
	res  *ActionResult
	// files maps workspace-relative path → digest for this target's
	// outputs plus all transitive deps' outputs.
	files map[string]cas.Digest
}

// Build builds labels and their transitive deps. The returned error is
// non-nil only for infrastructure problems or cancellation; target failures
// are reported in the Report.
func (b *Builder) Build(ctx context.Context, labels []string) (*Report, error) {
	start := time.Now()
	order, err := b.WS.Closure(labels)
	if err != nil {
		return nil, err
	}
	jobs := b.Jobs
	if jobs <= 0 {
		jobs = 4
	}
	sem := make(chan struct{}, jobs)
	nodes := map[string]*node{}
	for _, l := range order {
		nodes[l] = &node{done: make(chan struct{})}
	}
	var (
		mu       sync.Mutex
		infraErr error
		failed   bool
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, l := range order {
		wg.Add(1)
		go func(l string) {
			defer wg.Done()
			n := nodes[l]
			defer close(n.done)
			t := b.WS.Targets[l]
			files := map[string]cas.Digest{}
			for _, d := range t.DepLabels {
				dn := nodes[d]
				select {
				case <-dn.done:
				case <-ctx.Done():
					n.res = &ActionResult{Label: l, Test: t.Test, Skipped: true, Err: ctx.Err().Error()}
					return
				}
				if !dn.res.OK() {
					n.res = &ActionResult{Label: l, Test: t.Test, Skipped: true}
					return
				}
				for p, dg := range dn.files {
					files[p] = dg
				}
			}
			mu.Lock()
			stop := failed && !b.KeepGoing
			mu.Unlock()
			if stop {
				n.res = &ActionResult{Label: l, Test: t.Test, Skipped: true}
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				n.res = &ActionResult{Label: l, Test: t.Test, Skipped: true, Err: ctx.Err().Error()}
				return
			}
			res, err := b.runTarget(ctx, t, files)
			<-sem
			if err != nil {
				mu.Lock()
				if infraErr == nil {
					infraErr = err
				}
				mu.Unlock()
				cancel()
				res = &ActionResult{Label: l, Test: t.Test, Err: err.Error(), ExitCode: -1}
			}
			n.res = res
			if res.OK() {
				for _, o := range res.Outputs {
					files[o.Path] = o.Digest
				}
			} else {
				mu.Lock()
				failed = true
				mu.Unlock()
			}
			n.files = files
		}(l)
	}
	wg.Wait()
	rep := &Report{Results: map[string]*ActionResult{}, Order: order, Duration: time.Since(start)}
	for l, n := range nodes {
		rep.Results[l] = n.res
	}
	if infraErr != nil {
		return rep, infraErr
	}
	// Only the caller can have cancelled ctx at this point (our own cancel
	// runs on infra errors, handled above, or in the deferred call).
	return rep, ctx.Err()
}

type keyInput struct {
	Version string            `json:"v"`
	Dir     string            `json:"dir"`
	Cmd     string            `json:"cmd"`
	Env     map[string]string `json:"env"`
	Outs    []string          `json:"outs"`
	Test    bool              `json:"test"`
	Inputs  []string          `json:"inputs"` // "path\x00hash/size", sorted
}

func (b *Builder) envAllow() []string {
	if b.EnvAllow != nil {
		return b.EnvAllow
	}
	return DefaultEnvAllow
}

// ActionKey computes a target's key from its resolved input files.
func (b *Builder) ActionKey(t *Target, inputs map[string]cas.Digest) string {
	ki := keyInput{Version: KeyVersion, Dir: t.Dir, Cmd: t.Cmd, Env: map[string]string{}, Outs: t.OutFiles, Test: t.Test}
	for _, k := range b.envAllow() {
		ki.Env[k] = os.Getenv(k)
	}
	for p, d := range inputs {
		ki.Inputs = append(ki.Inputs, p+"\x00"+d.String())
	}
	sort.Strings(ki.Inputs)
	j, _ := json.Marshal(ki)
	sum := sha256.Sum256(j)
	return hex.EncodeToString(sum[:])
}

func (b *Builder) runTarget(ctx context.Context, t *Target, depFiles map[string]cas.Digest) (*ActionResult, error) {
	inputs := map[string]cas.Digest{}
	srcBytes := map[string][]byte{}
	for p, d := range depFiles {
		inputs[p] = d
	}
	for _, f := range t.SrcFiles {
		data, err := os.ReadFile(filepath.Join(b.WS.Root, filepath.FromSlash(f)))
		if err != nil {
			return nil, fmt.Errorf("read src %s: %w", f, err)
		}
		srcBytes[f] = data
		inputs[f] = cas.Of(data)
	}
	key := b.ActionKey(t, inputs)
	res := &ActionResult{Label: t.Label, Key: key, Test: t.Test}
	start := time.Now()
	if !b.NoCache {
		if ar, err := b.Cache.Get(ctx, key); err == nil {
			res.Cached = true
			res.Outputs = ar.Outputs
			res.Duration = time.Since(start)
			if out, err := b.CAS.Get(ctx, ar.Stdout); err == nil {
				res.Stdout = string(out)
			}
			b.logf("cache hit", t.Label, key)
			return res, nil
		} else if !errors.Is(err, actioncache.ErrNotFound) {
			return nil, fmt.Errorf("%w: action cache: %v", ErrInfra, err)
		}
	}

	sandbox, err := os.MkdirTemp("", "greenline-action-*")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInfra, err)
	}
	defer os.RemoveAll(sandbox)
	for p, data := range srcBytes {
		if err := writeFile(sandbox, p, data, 0o644); err != nil {
			return nil, err
		}
	}
	for p, d := range depFiles {
		data, err := b.CAS.Get(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("%w: fetch input %s: %v", ErrInfra, p, err)
		}
		if err := writeFile(sandbox, p, data, 0o755); err != nil {
			return nil, err
		}
	}
	os.MkdirAll(filepath.Join(sandbox, ".tmp"), 0o755)
	if t.Dir != "" {
		os.MkdirAll(filepath.Join(sandbox, t.Dir), 0o755)
	}

	actx := ctx
	if b.Timeout > 0 {
		var c context.CancelFunc
		actx, c = context.WithTimeout(ctx, b.Timeout)
		defer c()
	}
	cmd := exec.CommandContext(actx, "/bin/sh", "-c", t.Cmd)
	cmd.Dir = sandbox
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the whole process group so speculative builds that are
		// invalidated leave nothing behind.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	env := []string{"HOME=" + sandbox, "TMPDIR=" + filepath.Join(sandbox, ".tmp"),
		"SRCS=" + strings.Join(t.SrcFiles, " "), "OUTS=" + strings.Join(t.OutFiles, " "),
		"DIR=" + t.Dir, "LANG=C", "LC_ALL=C"}
	if len(t.OutFiles) > 0 {
		env = append(env, "OUT="+t.OutFiles[0])
	}
	for _, k := range b.envAllow() {
		env = append(env, k+"="+os.Getenv(k))
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout, res.Stderr = truncate(stdout.String()), truncate(stderr.String())
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			if res.ExitCode == -1 {
				res.ExitCode = 128 + 9
			}
		} else {
			return nil, fmt.Errorf("%w: exec: %v", ErrInfra, runErr)
		}
		if actx.Err() == context.DeadlineExceeded {
			res.Err = "timeout"
		}
		b.logf("action failed", t.Label, key, "exit", res.ExitCode)
		return res, nil
	}
	for _, o := range t.OutFiles {
		data, err := os.ReadFile(filepath.Join(sandbox, filepath.FromSlash(o)))
		if err != nil {
			res.ExitCode = 1
			res.Err = "missing declared output " + o
			return res, nil
		}
		d, err := cas.PutBytes(ctx, b.CAS, data)
		if err != nil {
			return nil, fmt.Errorf("%w: upload %s: %v", ErrInfra, o, err)
		}
		res.Outputs = append(res.Outputs, actioncache.OutputFile{Path: o, Digest: d})
	}
	if !b.NoUpload {
		so, err1 := cas.PutBytes(ctx, b.CAS, stdout.Bytes())
		se, err2 := cas.PutBytes(ctx, b.CAS, stderr.Bytes())
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("%w: upload logs: %v", ErrInfra, err)
		}
		if err := b.Cache.Put(ctx, key, &actioncache.Result{Outputs: res.Outputs, Stdout: so, Stderr: se}); err != nil {
			return nil, fmt.Errorf("%w: action cache put: %v", ErrInfra, err)
		}
	}
	b.logf("action ran", t.Label, key, "ms", res.Duration.Milliseconds())
	return res, nil
}

func truncate(s string) string {
	const max = 16 << 10
	if len(s) > max {
		return s[:max] + "\n...[truncated]"
	}
	return s
}

func writeFile(root, rel string, data []byte, mode os.FileMode) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes sandbox", rel)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, mode)
}

func (b *Builder) logf(msg, label, key string, kv ...any) {
	if b.Log != nil {
		b.Log.Debug(msg, append([]any{"label", label, "key", key[:12]}, kv...)...)
	}
}

// Materialize copies the outputs of labels from the CAS into dir.
func (b *Builder) Materialize(ctx context.Context, rep *Report, labels []string, dir string) error {
	for _, l := range labels {
		r := rep.Results[l]
		if r == nil || !r.OK() {
			continue
		}
		for _, o := range r.Outputs {
			data, err := b.CAS.Get(ctx, o.Digest)
			if err != nil {
				return err
			}
			if err := writeFile(dir, o.Path, data, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
