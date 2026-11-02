// Package runner executes queue CI runs for real: it materialises the
// speculative commit with the forge's deterministic merger, exports its tree,
// works out which targets the run's PRs affect, and builds and tests those
// targets through the shared cache. Cancelling a run (speculation
// invalidated) cancels the build, which kills every action's process group.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/affected"
	"github.com/xartus2004/greenline/build"
	"github.com/xartus2004/greenline/cas"
	"github.com/xartus2004/greenline/forge"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
)

// MergeConflict is the pseudo-test reported when a group does not merge.
const MergeConflict = queue.MergeConflictTest

// Runner implements queue.CI.
type Runner struct {
	Repo    *localgit.Repo
	CAS     cas.Store
	AC      actioncache.Raw
	Results chan<- queue.Result
	Jobs    int
	// ActionTimeout bounds each build action (0 = none).
	ActionTimeout time.Duration
	Log           *slog.Logger
	// OnReport, if set, receives every build report (for stats).
	OnReport func(queue.Run, *build.Report)

	mu      sync.Mutex
	cancels map[queue.RunID]context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
	done    chan struct{}
}

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

// Start implements queue.CI.
func (r *Runner) Start(run queue.Run) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.cancels == nil {
		r.cancels = map[queue.RunID]context.CancelFunc{}
		r.done = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancels[run.ID] = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		res := r.exec(ctx, run)
		r.mu.Lock()
		_, live := r.cancels[run.ID]
		delete(r.cancels, run.ID)
		r.mu.Unlock()
		live = live && ctx.Err() == nil
		cancel()
		if live {
			select {
			case r.Results <- res:
			case <-r.done:
			}
		}
	}()
}

// Cancel implements queue.CI.
func (r *Runner) Cancel(id queue.RunID) {
	r.mu.Lock()
	c, ok := r.cancels[id]
	delete(r.cancels, id)
	r.mu.Unlock()
	if ok {
		c()
	}
}

// Close cancels everything and waits for all runs to stop.
func (r *Runner) Close() {
	r.mu.Lock()
	if !r.closed && r.done != nil {
		close(r.done)
	}
	r.closed = true
	for id, c := range r.cancels {
		c()
		delete(r.cancels, id)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Runner) exec(ctx context.Context, run queue.Run) queue.Result {
	start := time.Now()
	res := queue.Result{RunID: run.ID}
	defer func() { res.CIMinutes = time.Since(start).Minutes() }()
	cur := run.Base
	for _, g := range run.Groups {
		res.Parent = cur
		c, err := r.Repo.Apply(cur, g)
		var ce *forge.ConflictError
		if errors.As(err, &ce) {
			res.Outcome, res.FailedTests, res.Err = queue.Fail, []string{MergeConflict}, fmt.Sprintf("PR #%d does not merge cleanly", ce.PR)
			return res
		}
		if err != nil {
			res.Outcome, res.Err = queue.Infra, err.Error()
			return res
		}
		cur = c
	}
	res.Commit = cur
	rep, err := r.BuildAffected(ctx, run.Base, cur)
	if r.OnReport != nil && rep != nil {
		r.OnReport(run, rep)
	}
	switch {
	case ctx.Err() != nil:
		res.Outcome, res.Err = queue.Infra, "cancelled"
	case err != nil:
		res.Outcome, res.Err = queue.Infra, err.Error()
	case !rep.OK():
		res.Outcome, res.FailedTests = queue.Fail, rep.Failed()
	default:
		res.Outcome = queue.Pass
	}
	r.log().Debug("run finished", "run", run.ID, "outcome", res.Outcome, "commit", short(cur), "failed", res.FailedTests)
	return res
}

func short(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return c
}

// Checkout exports commit into a fresh temp dir and loads its workspace.
func (r *Runner) Checkout(ctx context.Context, commit string) (*build.Workspace, func(), error) {
	dir, err := os.MkdirTemp("", "greenline-run-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err := r.Repo.Export(ctx, commit, dir); err != nil {
		cleanup()
		return nil, nil, err
	}
	ws, err := build.Load(dir)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return ws, cleanup, nil
}

// Affected returns the targets in head affected by the change base..head.
func (r *Runner) Affected(ctx context.Context, base, head string) ([]string, error) {
	bws, bclean, err := r.Checkout(ctx, base)
	if err != nil {
		return nil, err
	}
	defer bclean()
	hws, hclean, err := r.Checkout(ctx, head)
	if err != nil {
		return nil, err
	}
	defer hclean()
	files, err := affected.ChangedFiles(ctx, r.Repo.Dir, base, head)
	if err != nil {
		return nil, err
	}
	return affected.Targets(bws, hws, files), nil
}

func (r *Runner) builder(ws *build.Workspace) *build.Builder {
	return &build.Builder{WS: ws, CAS: r.CAS, Cache: actioncache.Typed{Raw: r.AC, CAS: r.CAS},
		Jobs: r.Jobs, KeepGoing: true, Timeout: r.ActionTimeout, Log: r.Log}
}

// BuildAffected builds and tests, at head, the targets affected by
// base..head.
func (r *Runner) BuildAffected(ctx context.Context, base, head string) (*build.Report, error) {
	targets, err := r.Affected(ctx, base, head)
	if err != nil {
		return nil, err
	}
	ws, clean, err := r.Checkout(ctx, head)
	if err != nil {
		return nil, err
	}
	defer clean()
	if len(targets) == 0 {
		return &build.Report{Results: map[string]*build.ActionResult{}}, nil
	}
	return r.builder(ws).Build(ctx, targets)
}

// VerifyMain rebuilds every commit on main's first-parent history from
// scratch (no cache reads) and returns the commits that are not green.
// Flaky targets are retried up to retries times before counting as red.
func (r *Runner) VerifyMain(ctx context.Context, retries int) (checked int, red map[string][]string, err error) {
	hist, err := r.Repo.MainHistory(ctx)
	if err != nil {
		return 0, nil, err
	}
	red = map[string][]string{}
	for _, c := range hist {
		var failed []string
		for attempt := 0; attempt <= retries; attempt++ {
			ws, clean, err := r.Checkout(ctx, c)
			if err != nil {
				return checked, red, err
			}
			b := r.builder(ws)
			b.NoCache, b.NoUpload = true, true
			rep, err := b.Build(ctx, ws.Labels())
			clean()
			if err != nil {
				return checked, red, err
			}
			failed = rep.Failed()
			if len(failed) == 0 {
				break
			}
		}
		checked++
		if len(failed) > 0 {
			red[c] = failed
		}
	}
	return checked, red, nil
}
