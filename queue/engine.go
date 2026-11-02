package queue

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/xartus2004/greenline/affected"
	"github.com/xartus2004/greenline/flaky"
)

// Engine is the merge queue. It is not safe for concurrent use: the caller
// (simulator or server) serialises all calls, typically from one goroutine.
//
// Every method that changes state returns an error only when the store or
// forge failed; the caller should then treat the engine as crashed, discard
// it, and build a new one with Recover.
type Engine struct {
	cfg   Config
	ci    CI
	forge Forge
	store Store
	clock Clock
	log   *slog.Logger
	Flaky *flaky.Tracker

	main    string
	prs     map[PRID]*PR
	lanes   []*lane
	waiting []PRID // lanes mode: PRs overlapping several lanes
	runs    map[RunID]*run
	nextRun RunID
	nextGID int
	nextLID int
	nextSeq int64

	stats Stats
}

// Stats are engine counters.
type Stats struct {
	RunsStarted    int            `json:"runs_started"`
	RunsCancelled  int            `json:"runs_cancelled"`
	Retries        int            `json:"flaky_retries"`
	InfraRetries   int            `json:"infra_retries"`
	Merges         int            `json:"merges"` // commits pushed to main
	MergedPRs      int            `json:"merged_prs"`
	Ejected        map[string]int `json:"ejected"`
	Flakes         int            `json:"flakes"`
	QuarantinedIgn int            `json:"quarantined_ignored"`
	Bisections     int            `json:"bisections"`
	StaleMerges    int            `json:"stale_merges"`
	StaleResults   int            `json:"stale_results"` // discarded after main moved under a lane
	ExternalMoves  int            `json:"external_moves"`
}

type lane struct {
	id     int
	groups []*group
}

type group struct {
	id      int
	prs     []PRID
	sealed  bool
	run     *run // latest live or completed run; nil if untested
	suspect bool // failed while not at head (SkipSuspects)
	infra   int
	retryAt time.Time
	bisect  bool // a bisection half: keeps its identity
}

type runStatus int

const (
	running runStatus = iota
	passed
	failed
	cancelled
)

type run struct {
	id       RunID
	g        *group
	l        *lane
	includes []int // group ids, in order, ending with g.id
	spec     Run
	status   runStatus
	result   Result
	deadline time.Time
	// stale runs were built on a main that has since moved for reasons
	// outside their lane. They keep running (their actions still fill the
	// cache) but their result is discarded and the group is re-verified on
	// the current main, which is then mostly cache hits.
	stale bool
	// firstFailure holds the gating failures of the attempt this run
	// retries; a pass on retry marks them flaky.
	firstFailure []string
}

func (r *run) has(gid int) bool {
	for _, x := range r.includes {
		if x == gid {
			return true
		}
	}
	return false
}

// Deps bundles the engine's collaborators.
type Deps struct {
	CI    CI
	Forge Forge
	Store Store
	Clock Clock
	Log   *slog.Logger
	Flaky *flaky.Tracker
}

// New builds an engine and recovers its state from d.Store (an empty store
// is a fresh queue). Recovery reconciles merges whose outcome was not
// recorded with the forge, re-queues everything that was in flight and
// starts testing.
func New(cfg Config, d Deps) (*Engine, error) {
	e := &Engine{
		cfg: cfg.withDefaults(), ci: d.CI, forge: d.Forge, store: d.Store, clock: d.Clock,
		log: d.Log, Flaky: d.Flaky,
		prs: map[PRID]*PR{}, runs: map[RunID]*run{},
	}
	e.stats.Ejected = map[string]int{}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.Flaky == nil {
		e.Flaky = flaky.New(2)
	}
	if err := e.recover(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) recover() error {
	evs, err := e.store.Load()
	if err != nil {
		return err
	}
	var order []PRID
	intents := []Event{}
	for _, ev := range evs {
		switch ev.Type {
		case EvEnqueue:
			e.prs[ev.PR] = &PR{ID: ev.PR, Version: ev.Version, Head: ev.Head, Targets: ev.Targets, State: Queued, EnqueuedAt: ev.At, Seq: e.seq()}
			order = moveToBack(order, ev.PR)
		case EvUpdate:
			if p := e.prs[ev.PR]; p != nil {
				p.Version, p.Head, p.Targets, p.Seq = ev.Version, ev.Head, ev.Targets, e.seq()
				order = moveToBack(order, ev.PR)
			}
		case EvRemove:
			e.finish(e.prs[ev.PR], Cancelled, ReasonRemoved, ev.At)
		case EvEject:
			if p := e.prs[ev.PR]; p != nil {
				p.FailedTest = ev.Tests
				e.finish(p, Ejected, ev.Reason, ev.At)
			}
		case EvMergeIntent:
			intents = append(intents, ev)
		case EvMerged:
			for _, it := range ev.Items {
				if p := e.prs[it.PR]; p != nil {
					p.Commit = ev.Commit
					e.finish(p, Merged, "", ev.At)
				}
			}
			intents = dropIntent(intents, ev.Commit)
		case EvFlake:
			for _, t := range ev.Tests {
				e.Flaky.RecordFlake(t)
			}
		}
	}
	// Reconcile merges that may or may not have happened before a crash.
	for _, in := range intents {
		ok, err := e.forge.Contains(in.Commit)
		if err != nil {
			return err
		}
		if !ok {
			continue // never landed: those PRs are simply re-tested
		}
		live := false
		for _, it := range in.Items {
			if p := e.prs[it.PR]; p != nil && !p.State.Terminal() {
				live = true
			}
		}
		if !live {
			continue
		}
		if err := e.append(Event{Type: EvMerged, Commit: in.Commit, Parent: in.Parent, Items: in.Items}); err != nil {
			return err
		}
		for _, it := range in.Items {
			if p := e.prs[it.PR]; p != nil && !p.State.Terminal() {
				p.Commit = in.Commit
				e.finish(p, Merged, "", e.now())
			}
		}
		e.log.Info("recovered merge", "commit", in.Commit)
	}
	if e.main, err = e.forge.Head(); err != nil {
		return err
	}
	for _, id := range order {
		if p := e.prs[id]; p != nil && !p.State.Terminal() {
			e.place(p)
		}
	}
	return e.scheduleAll()
}

func dropIntent(in []Event, commit string) []Event {
	out := in[:0]
	for _, ev := range in {
		if ev.Commit != commit {
			out = append(out, ev)
		}
	}
	return out
}

func moveToBack(order []PRID, id PRID) []PRID {
	out := make([]PRID, 0, len(order))
	for _, x := range order {
		if x != id {
			out = append(out, x)
		}
	}
	return append(out, id)
}

func (e *Engine) now() time.Time { return e.clock.Now() }

func (e *Engine) seq() int64 { e.nextSeq++; return e.nextSeq }

func (e *Engine) append(ev Event) error {
	if ev.At.IsZero() {
		ev.At = e.now()
	}
	return e.store.Append(ev)
}

func (e *Engine) finish(p *PR, s State, reason string, at time.Time) {
	if p == nil {
		return
	}
	p.State, p.Reason, p.DoneAt = s, reason, at
	switch s {
	case Merged:
		e.stats.MergedPRs++
	case Ejected:
		e.stats.Ejected[reason]++
	}
}

// ---------------------------------------------------------------- inputs

// Enqueue adds a new PR. Targets should be the PR's affected build targets
// (only used for lanes).
func (e *Engine) Enqueue(id PRID, head string, targets []string) error {
	if p, ok := e.prs[id]; ok && !p.State.Terminal() {
		return fmt.Errorf("queue: PR %d already queued", id)
	}
	targets = sortedCopy(targets)
	version := 1
	if p, ok := e.prs[id]; ok {
		version = p.Version + 1 // re-enqueue after a terminal state
	}
	if err := e.append(Event{Type: EvEnqueue, PR: id, Version: version, Head: head, Targets: targets}); err != nil {
		return err
	}
	p := &PR{ID: id, Version: version, Head: head, Targets: targets, State: Queued, EnqueuedAt: e.now(), Seq: e.seq()}
	e.prs[id] = p
	e.place(p)
	return e.scheduleAll()
}

// Update records a new head for a queued PR. Any run that included the old
// version is invalidated and the PR is re-tested (it moves to the back of
// the queue, like a fresh submission).
func (e *Engine) Update(id PRID, head string, targets []string) error {
	p, ok := e.prs[id]
	if !ok || p.State.Terminal() {
		return fmt.Errorf("queue: PR %d not queued", id)
	}
	targets = sortedCopy(targets)
	if err := e.append(Event{Type: EvUpdate, PR: id, Version: p.Version + 1, Head: head, Targets: targets}); err != nil {
		return err
	}
	e.unplace(p)
	p.Version++
	p.Head, p.Targets, p.State, p.Seq = head, targets, Queued, e.seq()
	e.place(p)
	return e.scheduleAll()
}

// Remove takes a PR out of the queue.
func (e *Engine) Remove(id PRID) error {
	p, ok := e.prs[id]
	if !ok || p.State.Terminal() {
		return fmt.Errorf("queue: PR %d not queued", id)
	}
	if err := e.append(Event{Type: EvRemove, PR: id}); err != nil {
		return err
	}
	e.unplace(p)
	e.finish(p, Cancelled, ReasonRemoved, e.now())
	return e.scheduleAll()
}

// OnMainMoved tells the engine main may have changed outside the queue
// (e.g. a direct push). Everything built on the old main is re-tested.
func (e *Engine) OnMainMoved() error {
	head, err := e.forge.Head()
	if err != nil {
		return err
	}
	if head == e.main {
		return nil
	}
	e.stats.ExternalMoves++
	e.main = head
	for _, l := range e.lanes {
		e.staleLane(l)
	}
	return e.scheduleAll()
}

// OnResult delivers a CI result. Results for cancelled or unknown runs are
// ignored.
func (e *Engine) OnResult(res Result) error {
	r, ok := e.runs[res.RunID]
	if !ok || r.status != running {
		return nil
	}
	delete(e.runs, res.RunID)
	r.result = res
	g, l := r.g, r.l
	if r.stale {
		r.status = cancelled
		g.run = nil
		g.suspect = false
		e.setTesting(g, false)
		e.stats.StaleResults++
		return e.scheduleAll()
	}
	switch res.Outcome {
	case Infra:
		r.status = cancelled
		g.run = nil
		g.infra++
		e.setTesting(g, false)
		if g.infra > e.cfg.MaxInfraRetries {
			if err := e.ejectGroup(l, g, ReasonInfra, nil); err != nil {
				return err
			}
		} else {
			e.stats.InfraRetries++
			g.retryAt = e.now().Add(e.cfg.InfraBackoff << (g.infra - 1))
		}
		return e.scheduleAll()
	case Fail:
		gating, ignored := e.Flaky.Gating(res.FailedTests)
		e.stats.QuarantinedIgn += len(ignored)
		if len(gating) == 0 && len(res.FailedTests) > 0 {
			// Only quarantined tests failed: that is a pass for gating.
			return e.onPass(r)
		}
		if len(gating) == 0 {
			gating = []string{"<unknown>"}
		}
		if r.spec.Attempt < e.cfg.FlakyRetries {
			// Retry on identical inputs before believing the failure.
			e.stats.Retries++
			r.status = cancelled
			g.run = nil
			nr := e.startRunSpec(l, g, r.includes, r.spec, r.spec.Attempt+1)
			nr.firstFailure = gating
			return nil
		}
		for _, t := range gating {
			e.Flaky.RecordFailure(t)
		}
		r.status = failed
		r.result.FailedTests = gating
		return e.onFail(l, g)
	default:
		return e.onPass(r)
	}
}

func (e *Engine) onPass(r *run) error {
	r.status = passed
	if len(r.firstFailure) > 0 {
		// Fail then pass on identical inputs: those tests are flaky.
		if err := e.append(Event{Type: EvFlake, Tests: r.firstFailure, Commit: r.result.Commit}); err != nil {
			return err
		}
		for _, t := range r.firstFailure {
			e.stats.Flakes++
			e.Flaky.RecordFlake(t)
		}
	}
	if err := e.drain(r.l); err != nil {
		return err
	}
	return e.scheduleAll()
}

func (e *Engine) onFail(l *lane, g *group) error {
	if l.groups[0] == g || !e.cfg.SkipSuspects {
		// At the head the failure is definitive; behind the head without
		// SkipSuspects we wait until the group reaches the head.
		if err := e.drain(l); err != nil {
			return err
		}
		return e.scheduleAll()
	}
	// Zuul-style: assume g is the culprit and rebuild everything behind it
	// without it. If something ahead of g later fails instead, g's evidence
	// becomes invalid and g is re-tested (see invalidate).
	g.suspect = true
	behind := false
	for _, x := range l.groups {
		if x == g {
			behind = true
			continue
		}
		if behind && x.run != nil && x.run.has(g.id) {
			e.resetGroup(x)
		}
	}
	return e.scheduleAll()
}

// Tick handles timeouts and backoff expiry. Call it at or after
// NextWakeup.
func (e *Engine) Tick() error {
	now := e.now()
	var timedOut []*run
	for _, r := range e.runs {
		if !r.deadline.IsZero() && !now.Before(r.deadline) {
			timedOut = append(timedOut, r)
		}
	}
	sort.Slice(timedOut, func(i, j int) bool { return timedOut[i].id < timedOut[j].id })
	for _, r := range timedOut {
		e.ci.Cancel(r.id)
		e.stats.RunsCancelled++
		if err := e.OnResult(Result{RunID: r.id, Outcome: Fail, FailedTests: []string{"<timeout>"}, Err: "timeout"}); err != nil {
			return err
		}
	}
	return e.scheduleAll()
}

// NextWakeup returns the earliest time Tick has work to do.
func (e *Engine) NextWakeup() (time.Time, bool) {
	var t time.Time
	consider := func(x time.Time) {
		if !x.IsZero() && (t.IsZero() || x.Before(t)) {
			t = x
		}
	}
	for _, r := range e.runs {
		consider(r.deadline)
	}
	for _, l := range e.lanes {
		for _, g := range l.groups {
			if g.run == nil && g.retryAt.After(e.now()) {
				consider(g.retryAt)
			}
		}
	}
	return t, !t.IsZero()
}

// --------------------------------------------------------------- placement

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func (e *Engine) laneTargets(l *lane) []string {
	set := map[string]bool{}
	for _, g := range l.groups {
		for _, id := range g.prs {
			for _, t := range e.prs[id].Targets {
				set[t] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) newLane() *lane {
	e.nextLID++
	l := &lane{id: e.nextLID}
	e.lanes = append(e.lanes, l)
	return l
}

func (e *Engine) newGroup(prs []PRID, sealed bool) *group {
	e.nextGID++
	return &group{id: e.nextGID, prs: prs, sealed: sealed}
}

// place puts an active PR into a lane (or the waiting list).
func (e *Engine) place(p *PR) {
	if !e.cfg.Lanes {
		if len(e.lanes) == 0 {
			e.newLane()
		}
		e.appendTo(e.lanes[0], p.ID)
		return
	}
	for _, w := range e.waiting {
		if !affected.Disjoint(e.prs[w].Targets, p.Targets) {
			e.waiting = append(e.waiting, p.ID) // keep order among overlapping PRs
			return
		}
	}
	var hit []*lane
	for _, l := range e.lanes {
		if !affected.Disjoint(e.laneTargets(l), p.Targets) {
			hit = append(hit, l)
		}
	}
	switch len(hit) {
	case 0:
		e.appendTo(e.newLane(), p.ID)
	case 1:
		e.appendTo(hit[0], p.ID)
	default:
		e.waiting = append(e.waiting, p.ID)
	}
}

func (e *Engine) appendTo(l *lane, id PRID) {
	if n := len(l.groups); n > 0 {
		last := l.groups[n-1]
		if !last.sealed && len(last.prs) < e.cfg.BatchSize {
			last.prs = append(last.prs, id)
			return
		}
	}
	l.groups = append(l.groups, e.newGroup([]PRID{id}, false))
}

// unplace removes an active PR from wherever it is, invalidating runs that
// included its group.
func (e *Engine) unplace(p *PR) {
	for i, w := range e.waiting {
		if w == p.ID {
			e.waiting = append(e.waiting[:i], e.waiting[i+1:]...)
			return
		}
	}
	for _, l := range e.lanes {
		for gi, g := range l.groups {
			for pi, id := range g.prs {
				if id != p.ID {
					continue
				}
				rest := append(append([]PRID(nil), g.prs[:pi]...), g.prs[pi+1:]...)
				e.removeGroupAt(l, gi)
				if len(rest) > 0 {
					ng := e.newGroup(rest, g.sealed)
					l.groups = append(l.groups[:gi], append([]*group{ng}, l.groups[gi:]...)...)
				}
				e.cleanupLanes()
				return
			}
		}
	}
}

// removeGroupAt drops a group and invalidates every run that included it.
func (e *Engine) removeGroupAt(l *lane, gi int) {
	g := l.groups[gi]
	e.resetGroup(g)
	l.groups = append(l.groups[:gi], l.groups[gi+1:]...)
	for _, x := range l.groups {
		if x.run != nil && x.run.has(g.id) {
			e.resetGroup(x)
		}
	}
}

func (e *Engine) cleanupLanes() {
	if e.cfg.Lanes {
		e.relayout()
		return
	}
	out := e.lanes[:0]
	for _, l := range e.lanes {
		if len(l.groups) > 0 || !e.cfg.Lanes {
			out = append(out, l)
		}
	}
	e.lanes = out
}

// relayout re-forms lanes after something left the queue. Groups that are
// committed to a lane (they have a run, are bisection halves, or are
// backing off) keep their place as a prefix of that lane; every other PR is
// re-placed in queue order. Without this a lane's target set would be the
// union of its whole backlog and, under load, everything would collapse
// into one lane. With it a PR waits only for earlier PRs it actually
// overlaps, which approximates a dependency DAG.
func (e *Engine) relayout() {
	now := e.now()
	var loose []PRID
	for _, l := range e.lanes {
		k := 0
		for k < len(l.groups) {
			g := l.groups[k]
			if g.run == nil && !g.bisect && !g.retryAt.After(now) {
				break
			}
			k++
		}
		for _, g := range l.groups[k:] {
			e.resetGroup(g)
			loose = append(loose, g.prs...)
		}
		l.groups = l.groups[:k]
	}
	loose = append(loose, e.waiting...)
	e.waiting = nil
	out := e.lanes[:0]
	for _, l := range e.lanes {
		if len(l.groups) > 0 {
			out = append(out, l)
		}
	}
	e.lanes = out
	sort.Slice(loose, func(i, j int) bool { return e.prs[loose[i]].Seq < e.prs[loose[j]].Seq })
	for _, id := range loose {
		e.place(e.prs[id])
	}
}

// ------------------------------------------------------------- runs

func (e *Engine) setTesting(g *group, on bool) {
	for _, id := range g.prs {
		if p := e.prs[id]; p != nil && !p.State.Terminal() {
			if on {
				p.State = Testing
			} else {
				p.State = Queued
			}
		}
	}
}

// resetGroup cancels/forgets the group's run and suspicion.
func (e *Engine) resetGroup(g *group) {
	if g.run != nil {
		if g.run.status == running {
			e.ci.Cancel(g.run.id)
			delete(e.runs, g.run.id)
			e.stats.RunsCancelled++
		}
		g.run.status = cancelled
		g.run = nil
	}
	g.suspect = false
	e.setTesting(g, false)
}

}

// staleLane invalidates every run of l after main moved for reasons outside
// l. Finished results are dropped (the group is re-tested); running ones are
// left to finish so their work reaches the cache, then re-tested.
func (e *Engine) staleLane(l *lane) {
	for _, g := range l.groups {
		if g.run != nil && g.run.status == running {
			g.run.stale = true
			continue
		}
		e.resetGroup(g)
	}
}

func (e *Engine) scheduleAll() error {
	for _, l := range e.lanes {
		e.schedule(l)
	}
	return nil
}

// schedule starts runs for untested groups inside the lane's window.
func (e *Engine) schedule(l *lane) {
	now := e.now()
	var ahead []int // non-suspect group ids ahead of the current one
	window := 0
	for _, g := range l.groups {
		if g.suspect {
			continue
		}
		if window >= e.cfg.Depth {
			break
		}
		window++
		includes := append(append([]int(nil), ahead...), g.id)
		ahead = append(ahead, g.id)
		if g.run != nil || now.Before(g.retryAt) {
			continue
		}
		e.startRun(l, g, includes)
	}
}

func (e *Engine) startRun(l *lane, g *group, includes []int) *run {
	byID := map[int]*group{}
	for _, x := range l.groups {
		byID[x.id] = x
	}
	spec := Run{Base: e.main}
	tset := map[string]bool{}
	for _, gid := range includes {
		var items []Item
		for _, id := range byID[gid].prs {
			p := e.prs[id]
			items = append(items, Item{PR: id, Version: p.Version, Head: p.Head})
			for _, t := range p.Targets {
				tset[t] = true
			}
		}
		spec.Groups = append(spec.Groups, items)
	}
	for t := range tset {
		spec.Targets = append(spec.Targets, t)
	}
	sort.Strings(spec.Targets)
	return e.startRunSpec(l, g, includes, spec, 0)
}

func (e *Engine) startRunSpec(l *lane, g *group, includes []int, spec Run, attempt int) *run {
	e.nextRun++
	spec.ID = e.nextRun
	spec.Attempt = attempt
	r := &run{id: spec.ID, g: g, l: l, includes: includes, spec: spec, status: running}
	if e.cfg.RunTimeout > 0 {
		r.deadline = e.now().Add(e.cfg.RunTimeout)
	}
	g.sealed = true
	g.run = r
	e.runs[r.id] = r
	e.setTesting(g, true)
	for _, id := range g.prs {
		e.prs[id].Runs++
	}
	e.stats.RunsStarted++
	e.ci.Start(spec)
	return r
}

// ------------------------------------------------------- merging & failing

// drain resolves the head of lane l for as long as it can: merging passed
// heads and handling definitive failures.
func (e *Engine) drain(l *lane) error {
	for len(l.groups) > 0 {
		head := l.groups[0]
		if head.run == nil {
			return nil
		}
		switch head.run.status {
		case passed:
			if err := e.merge(l, head); err != nil {
				return err
			}
			if !e.laneAlive(l) {
				return nil
			}
		case failed:
			if err := e.definitiveFailure(l, head); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func (e *Engine) laneAlive(l *lane) bool {
	for _, x := range e.lanes {
		if x == l {
			return true
		}
	}
	return false
}

func (e *Engine) items(g *group) []Item {
	var items []Item
	for _, id := range g.prs {
		p := e.prs[id]
		items = append(items, Item{PR: id, Version: p.Version, Head: p.Head})
	}
	return items
}

func (e *Engine) merge(l *lane, g *group) error {
	res := g.run.result
	if res.Parent != e.main || res.Commit == "" {
		// The run did not test main+g exactly. Never merge it.
		e.stats.StaleMerges++
		e.log.Warn("stale pass, retesting", "group", g.id, "parent", res.Parent, "main", e.main)
		e.resetGroup(g)
		return nil
	}
	items := e.items(g)
	if err := e.append(Event{Type: EvMergeIntent, Commit: res.Commit, Parent: res.Parent, Items: items}); err != nil {
		return err
	}
	err := e.forge.Advance(e.main, res.Commit)
	if errors.Is(err, ErrStale) {
		e.stats.StaleMerges++
		return e.OnMainMoved()
	}
	if err != nil {
		return fmt.Errorf("queue: advance main: %w", err)
	}
	if err := e.append(Event{Type: EvMerged, Commit: res.Commit, Parent: res.Parent, Items: items}); err != nil {
		return err
	}
	e.main = res.Commit
	e.stats.Merges++
	now := e.now()
	for _, id := range g.prs {
		p := e.prs[id]
		p.Commit = res.Commit
		e.finish(p, Merged, "", now)
	}
	e.log.Debug("merged", "commit", res.Commit, "prs", g.prs)
	// Pop the head. Runs in this lane that did not include it were built
	// on a tree that no longer matches main.
	l.groups = l.groups[1:]
	for _, x := range l.groups {
		if x.run != nil && !x.run.has(g.id) {
			e.resetGroup(x)
		}
	}
	// Main moved under every other lane.
	for _, o := range e.lanes {
		if o != l {
			e.staleLane(o)
		}
	}
	e.cleanupLanes()
	return nil
}

func (e *Engine) definitiveFailure(l *lane, g *group) error {
	tests := g.run.result.FailedTests
	reason := ReasonTestFailed
	if len(tests) == 1 && tests[0] == "<timeout>" {
		reason = ReasonTimeout
	}
	if len(g.prs) == 1 {
		return e.ejectGroup(l, g, reason, tests)
	}
	// Bisect: split the batch in two and test the halves in order.
	e.stats.Bisections++
	mid := len(g.prs) / 2
	left := e.newGroup(append([]PRID(nil), g.prs[:mid]...), true)
	right := e.newGroup(append([]PRID(nil), g.prs[mid:]...), true)
	left.bisect, right.bisect = true, true
	e.removeGroupAt(l, 0)
	l.groups = append([]*group{left, right}, l.groups...)
	return nil
}

func (e *Engine) ejectGroup(l *lane, g *group, reason string, tests []string) error {
	for _, id := range g.prs {
		if err := e.append(Event{Type: EvEject, PR: id, Reason: reason, Tests: tests}); err != nil {
			return err
		}
	}
	now := e.now()
	for gi, x := range l.groups {
		if x == g {
			e.removeGroupAt(l, gi)
			break
		}
	}
	for _, id := range g.prs {
		p := e.prs[id]
		p.FailedTest = tests
		e.finish(p, Ejected, reason, now)
	}
	e.cleanupLanes()
	return nil
}

// --------------------------------------------------------------- queries

// Main returns main's commit as the engine last saw it.
func (e *Engine) Main() string { return e.main }

// PR returns a copy of a PR's state.
func (e *Engine) PR(id PRID) (PR, bool) {
	p, ok := e.prs[id]
	if !ok {
		return PR{}, false
	}
	return *p, true
}

// PRs returns copies of all PRs sorted by id.
func (e *Engine) PRs() []PR {
	out := make([]PR, 0, len(e.prs))
	for _, p := range e.prs {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stats returns a copy of the counters.
func (e *Engine) Stats() Stats {
	s := e.stats
	s.Ejected = map[string]int{}
	for k, v := range e.stats.Ejected {
		s.Ejected[k] = v
	}
	return s
}

// Active reports the number of non-terminal PRs.
func (e *Engine) Active() int {
	n := 0
	for _, p := range e.prs {
		if !p.State.Terminal() {
			n++
		}
	}
	return n
}

// RunningRuns returns the number of runs in flight.
func (e *Engine) RunningRuns() int { return len(e.runs) }

// LaneView is a snapshot of one lane for status pages.
type LaneView struct {
	ID     int         `json:"id"`
	Groups []GroupView `json:"groups"`
}

// GroupView is a snapshot of one group.
type GroupView struct {
	PRs     []PRID `json:"prs"`
	Status  string `json:"status"`
	Suspect bool   `json:"suspect,omitempty"`
}

// Lanes returns a snapshot of the queue structure.
func (e *Engine) Lanes() []LaneView {
	var out []LaneView
	for _, l := range e.lanes {
		lv := LaneView{ID: l.id}
		for _, g := range l.groups {
			st := "queued"
			if g.run != nil {
				st = [...]string{"testing", "passed", "failed", "cancelled"}[g.run.status]
			}
			lv.Groups = append(lv.Groups, GroupView{PRs: append([]PRID(nil), g.prs...), Status: st, Suspect: g.suspect})
		}
		out = append(out, lv)
	}
	return out
}

// Waiting returns PRs blocked on overlapping lanes.
func (e *Engine) Waiting() []PRID { return append([]PRID(nil), e.waiting...) }
