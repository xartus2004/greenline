// Package server runs the merge queue for real: one goroutine owns the
// engine and serialises every input (API calls, CI results, timers, main
// polling); a localgit forge, the runner and the cache do the work; state is
// write-ahead logged to SQLite so the process can be killed at any moment.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/build"
	"github.com/xartus2004/greenline/cas"
	"github.com/xartus2004/greenline/flaky"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/httpcache"
	"github.com/xartus2004/greenline/metrics"
	"github.com/xartus2004/greenline/queue"
	"github.com/xartus2004/greenline/reapi"
	"github.com/xartus2004/greenline/runner"
	"github.com/xartus2004/greenline/store"
	"google.golang.org/grpc"
)

// Config configures a server.
type Config struct {
	StateDir      string // queue.db and cache/ live here
	Repo          *localgit.Repo
	Queue         queue.Config
	Jobs          int
	CacheBytes    int64
	ActionTimeout time.Duration
	PollMain      time.Duration // how often to check main for external pushes (default 2s)
	// CompactAfter compacts the event log once it holds more than this
	// many events (default 5000; negative disables).
	CompactAfter int
	Log          *slog.Logger
	// OnTransition is called (from the engine goroutine) whenever a PR
	// changes state.
	OnTransition func(Transition)
}

// Transition is a PR state change.
type Transition struct {
	At   time.Time
	PR   queue.PR
	From queue.State
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Server is a running queue.
type Server struct {
	cfg     Config
	log     *slog.Logger
	store   *store.SQLite
	cas     *cas.Disk
	ac      *actioncache.Disk
	runner  *runner.Runner
	eng     *queue.Engine
	results chan queue.Result
	cmds    chan func()
	status  atomic.Pointer[Status]
	prev    map[queue.PRID]queue.State

	mu            sync.Mutex
	ttmPending    []float64
	actionsRun    atomic.Int64
	actionsCached atomic.Int64

	cacheSrv *httpcache.Server
}

// Status is a snapshot for the API and status page.
type Status struct {
	Main    string           `json:"main"`
	PRs     []queue.PR       `json:"prs"`
	Lanes   []queue.LaneView `json:"lanes"`
	Waiting []queue.PRID     `json:"waiting"`
	Stats   queue.Stats      `json:"stats"`
	Flaky   []flaky.Stat     `json:"flaky"`
	Cache   cas.Stats        `json:"cache"`
	Running int              `json:"running_runs"`
	At      time.Time        `json:"at"`
}

// New opens state and recovers the queue.
func New(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.CacheBytes <= 0 {
		cfg.CacheBytes = 5 << 30
	}
	if cfg.CompactAfter == 0 {
		cfg.CompactAfter = 5000
	}
	if cfg.PollMain <= 0 {
		cfg.PollMain = 2 * time.Second
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "queue.db"))
	if err != nil {
		return nil, err
	}
	c, err := cas.OpenDisk(filepath.Join(cfg.StateDir, "cache", "cas"), cfg.CacheBytes)
	if err != nil {
		return nil, err
	}
	ac, err := actioncache.OpenDisk(filepath.Join(cfg.StateDir, "cache", "ac"))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: cfg.Log, store: st, cas: c, ac: ac,
		results: make(chan queue.Result, 1024), cmds: make(chan func()), prev: map[queue.PRID]queue.State{}}
	s.runner = &runner.Runner{Repo: cfg.Repo, CAS: c, AC: ac, Results: s.results, Jobs: cfg.Jobs,
		ActionTimeout: cfg.ActionTimeout, Log: cfg.Log, OnReport: s.onReport}
	s.cacheSrv = &httpcache.Server{CAS: c, AC: ac, Log: cfg.Log}
	eng, err := queue.New(cfg.Queue, queue.Deps{CI: s.runner, Forge: cfg.Repo, Store: st, Clock: realClock{}, Log: cfg.Log})
	if err != nil {
		s.runner.Close()
		st.Close()
		return nil, fmt.Errorf("recover queue: %w", err)
	}
	s.eng = eng
	for _, p := range eng.PRs() {
		s.prev[p.ID] = p.State
	}
	s.snapshot()
	return s, nil
}

func (s *Server) onReport(_ queue.Run, rep *build.Report) {
	h, e := rep.Hits()
	s.actionsCached.Add(int64(h))
	s.actionsRun.Add(int64(e))
}

// Run drives the engine until ctx is done or the engine fails (store or
// forge error), in which case the process should exit and be restarted;
// recovery happens in New.
func (s *Server) Run(ctx context.Context) error {
	defer s.store.Close()
	defer s.runner.Close()
	poll := time.NewTicker(s.cfg.PollMain)
	defer poll.Stop()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	steps := 0
	for {
		if t, ok := s.eng.NextWakeup(); ok {
			timer.Reset(max(time.Until(t), time.Millisecond))
		} else {
			timer.Reset(time.Hour)
		}
		var err error
		select {
		case <-ctx.Done():
			return nil
		case f := <-s.cmds:
			f()
		case res := <-s.results:
			err = s.eng.OnResult(res)
		case <-timer.C:
			err = s.eng.Tick()
		case <-poll.C:
			err = s.eng.OnMainMoved()
		}
		if err != nil {
			return fmt.Errorf("engine: %w", err)
		}
		if steps++; s.cfg.CompactAfter > 0 && steps%256 == 0 {
			if n, err := s.store.Count(); err == nil && n > s.cfg.CompactAfter {
				if err := s.eng.Compact(); err != nil {
					return fmt.Errorf("compact: %w", err)
				}
				s.log.Info("compacted event log", "events_before", n)
			}
		}
		s.snapshot()
	}
}

// do runs f on the engine goroutine and waits for it.
func (s *Server) do(ctx context.Context, f func(*queue.Engine) error) error {
	done := make(chan error, 1)
	select {
	case s.cmds <- func() { done <- f(s.eng) }:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) snapshot() {
	now := time.Now()
	prs := s.eng.PRs()
	for _, p := range prs {
		if from, ok := s.prev[p.ID]; !ok || from != p.State {
			s.prev[p.ID] = p.State
			if p.State == queue.Merged {
				s.mu.Lock()
				s.ttmPending = append(s.ttmPending, p.DoneAt.Sub(p.EnqueuedAt).Seconds())
				s.mu.Unlock()
			}
			if s.cfg.OnTransition != nil {
				s.cfg.OnTransition(Transition{At: now, PR: p, From: from})
			}
		}
	}
	s.status.Store(&Status{Main: s.eng.Main(), PRs: prs, Lanes: s.eng.Lanes(), Waiting: s.eng.Waiting(),
		Stats: s.eng.Stats(), Flaky: s.eng.Flaky.Stats(), Cache: s.cas.Stats(), Running: s.eng.RunningRuns(), At: now})
}

// Status returns the latest snapshot.
func (s *Server) Status() *Status { return s.status.Load() }

// RegisterREAPI serves the queue's cache over the REAPI gRPC services.
func (s *Server) RegisterREAPI(gs *grpc.Server) {
	(&reapi.Server{CAS: s.cas, AC: s.ac}).Register(gs)
}

// ActionCounts returns build actions executed and served from cache.
func (s *Server) ActionCounts() (executed, cached int64) {
	return s.actionsRun.Load(), s.actionsCached.Load()
}

// Runner exposes the runner (for verification).
func (s *Server) Runner() *runner.Runner { return s.runner }

// Enqueue adds refs/heads/<branch> as PR id.
func (s *Server) Enqueue(ctx context.Context, id queue.PRID, branch string) error {
	head, targets, err := s.inspect(ctx, branch)
	if err != nil {
		return err
	}
	return s.do(ctx, func(e *queue.Engine) error {
		p, ok := e.PR(id)
		switch {
		case ok && p.Head == head:
			return nil // re-delivery of something already handled: idempotent
		case ok && !p.State.Terminal():
			return e.Update(id, head, targets)
		}
		return e.Enqueue(id, head, targets) // new PR, or new head after a terminal state
	})
}

// Update re-reads the branch of a queued PR.
func (s *Server) Update(ctx context.Context, id queue.PRID, branch string) error {
	return s.Enqueue(ctx, id, branch)
}

// Remove withdraws a PR.
func (s *Server) Remove(ctx context.Context, id queue.PRID) error {
	return s.do(ctx, func(e *queue.Engine) error { return e.Remove(id) })
}

// inspect resolves a branch to its head and affected targets (relative to
// its merge base with main).
func (s *Server) inspect(ctx context.Context, branch string) (string, []string, error) {
	head, err := s.cfg.Repo.BranchHead(ctx, branch)
	if err != nil {
		return "", nil, err
	}
	base, err := s.cfg.Repo.Git(ctx, nil, "merge-base", "refs/heads/main", head)
	if err != nil {
		return "", nil, err
	}
	targets, err := s.runner.Affected(ctx, base, head)
	if err != nil {
		return "", nil, err
	}
	return head, targets, nil
}

func (s *Server) metricsSnapshot() metrics.Snapshot {
	st := s.Status()
	cs := s.cas.Stats()
	s.mu.Lock()
	ttm := s.ttmPending
	s.ttmPending = nil
	s.mu.Unlock()
	depth := 0
	for _, p := range st.PRs {
		if !p.State.Terminal() {
			depth++
		}
	}
	return metrics.Snapshot{QueueDepth: depth, RunningRuns: st.Running, Lanes: len(st.Lanes),
		MergedPRs: st.Stats.MergedPRs, Merges: st.Stats.Merges, Ejected: st.Stats.Ejected,
		RunsStarted: st.Stats.RunsStarted, RunsCancelled: st.Stats.RunsCancelled, Flakes: st.Stats.Flakes,
		CacheHits: cs.Hits, CacheMisses: cs.Misses, CASBytes: cs.Bytes, CASItems: cs.Items,
		CASEvictions: cs.Evictions, CASCorrupt: cs.Corrupt,
		ActionsRun: s.actionsRun.Load(), ActionsCached: s.actionsCached.Load(), TimeToMergeSec: ttm}
}

// Handler serves the API, status page, metrics and the HTTP cache.
//
//	POST   /api/prs            {"id": 1, "branch": "pr/1"}   enqueue (or update)
//	DELETE /api/prs/{id}                                      remove
//	GET    /api/status                                        JSON snapshot
//	GET    /                                                  HTML status page
//	GET    /metrics                                           Prometheus
//	*      /cache/{ac,cas}/{sha256}                            Bazel HTTP cache
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.New(s.metricsSnapshot))
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/cache/", s.cacheSrv)
	mux.HandleFunc("POST /api/prs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     int64  `json:"id"`
			Branch string `json:"branch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
			http.Error(w, "want {\"id\": n, \"branch\": \"...\"}", http.StatusBadRequest)
			return
		}
		if body.Branch == "" {
			body.Branch = fmt.Sprintf("pr/%d", body.ID)
		}
		if strings.Contains(body.Branch, "..") || strings.HasPrefix(body.Branch, "-") {
			http.Error(w, "bad branch", http.StatusBadRequest)
			return
		}
		if err := s.Enqueue(r.Context(), queue.PRID(body.ID), body.Branch); err != nil {
			s.httpErr(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("DELETE /api/prs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if err := s.Remove(r.Context(), queue.PRID(id)); err != nil {
			s.httpErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.Status())
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		st := s.Status()
		prs := append([]queue.PR(nil), st.PRs...)
		sort.Slice(prs, func(i, j int) bool { return prs[i].Seq > prs[j].Seq })
		statusPage.Execute(w, struct {
			*Status
			Sorted []queue.PR
		}{st, prs})
	})
	return mux
}

func (s *Server) httpErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case strings.Contains(err.Error(), "not queued"), strings.Contains(err.Error(), "already queued"):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var statusPage = template.Must(template.New("status").Funcs(template.FuncMap{
	"short": func(s string) string {
		if len(s) > 10 {
			return s[:10]
		}
		return s
	},
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="2">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>greenline</title>
<style>
:root{--bg:#fff;--fg:#1b1f24;--mute:#59636e;--line:#d1d9e0;--ok:#1a7f37;--bad:#cf222e;--run:#9a6700}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--mute:#9198a1;--line:#3d444d;--ok:#3fb950;--bad:#f85149;--run:#d29922}}
body{background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,sans-serif;margin:0 auto;max-width:960px;padding:16px}
h1{font-size:20px;margin:0 0 4px} .mute{color:var(--mute)} code{font-size:13px}
table{border-collapse:collapse;width:100%;margin:12px 0} td,th{border-bottom:1px solid var(--line);padding:4px 8px;text-align:left}
.MERGED{color:var(--ok)} .EJECTED{color:var(--bad)} .TESTING{color:var(--run)} .lane{margin:4px 0}
.g{display:inline-block;border:1px solid var(--line);border-radius:4px;padding:1px 6px;margin-right:4px}
</style></head><body>
<h1>greenline</h1>
<div class="mute">main <code>{{short .Main}}</code> · {{.Running}} runs in flight · {{.Stats.MergedPRs}} merged in {{.Stats.Merges}} commits · {{.Stats.RunsStarted}} runs started, {{.Stats.RunsCancelled}} cancelled · cache {{.Cache.Hits}} hits / {{.Cache.Misses}} misses</div>
<h2>Lanes</h2>
{{range .Lanes}}<div class="lane">lane {{.ID}}: {{range .Groups}}<span class="g">{{range .PRs}}#{{.}} {{end}}— {{.Status}}{{if .Suspect}} (suspect){{end}}</span>{{end}}</div>{{else}}<div class="mute">empty</div>{{end}}
{{if .Waiting}}<div class="mute">waiting on overlapping lanes: {{range .Waiting}}#{{.}} {{end}}</div>{{end}}
<h2>PRs</h2>
<table><tr><th>PR</th><th>state</th><th>reason</th><th>runs</th><th>commit</th></tr>
{{range .Sorted}}<tr><td>#{{.ID}} v{{.Version}}</td><td class="{{.State}}">{{.State}}</td><td>{{.Reason}} {{range .FailedTest}}<code>{{.}}</code> {{end}}</td><td>{{.Runs}}</td><td><code>{{short .Commit}}</code></td></tr>{{end}}
</table>
{{if .Flaky}}<h2>Flaky tests</h2><table><tr><th>test</th><th>flakes</th><th>quarantined</th></tr>
{{range .Flaky}}<tr><td><code>{{.Test}}</code></td><td>{{.Flakes}}</td><td>{{.Quarantined}}</td></tr>{{end}}</table>{{end}}
</body></html>`))
