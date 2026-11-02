package sim

import (
	"container/heap"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/xartus2004/greenline/flaky"
	"github.com/xartus2004/greenline/queue"
)

// Params describe one simulated scenario.
type Params struct {
	Seed           int64
	PRs            int
	ArrivalPerHour float64
	FailProb       float64 // p: probability a PR is broken on its own
	ConflictProb   float64 // probability a PR breaks when combined with an overlapping open PR
	FlakeRate      float64 // probability a run fails on a flaky test
	FlakyTests     int     // number of distinct flaky tests
	InfraRate      float64 // probability a run hits an infra error
	UpdateProb     float64 // probability a PR is updated while queued
	RemoveProb     float64 // probability a PR is withdrawn while queued
	ExternalPushes int     // direct pushes to main during the run

	Targets  int // size of the synthetic repo
	TouchMax int // max targets a PR touches directly

	CIOverhead time.Duration // fixed per-run cost (checkout, setup)
	TargetCost time.Duration // mean per-target cost on a cache miss
	HitCost    time.Duration // per-target cost on a cache hit
	Cache      bool          // model a shared action cache

	// CrashMean > 0 injects engine crashes: after a uniformly random number
	// of store appends in [1, CrashMean] the process "dies" and recovers.
	CrashMean int

	Queue queue.Config
}

// Defaults returns the baseline scenario used for the README table.
func Defaults() Params {
	return Params{
		PRs: 400, ArrivalPerHour: 12, FailProb: 0.10, ConflictProb: 0.02,
		FlakeRate: 0.02, FlakyTests: 3, InfraRate: 0.005,
		Targets: 60, TouchMax: 2,
		CIOverhead: 2 * time.Minute, TargetCost: 90 * time.Second, HitCost: 2 * time.Second,
		Cache: true,
	}
}

// Result is what one simulation measured.
type Result struct {
	Params        Params
	Merged        int
	Ejected       int
	Cancelled     int
	EjectedGood   int // ejected although not broken on their own (conflict partners, flakes, infra)
	Hours         float64
	Throughput    float64 // merged PRs per hour of simulated time
	P50, P95      time.Duration
	CIMinutes     float64
	WastedMinutes float64 // minutes of cancelled runs
	CIPerMerged   float64
	Runs          int
	CacheHitRatio float64
	Crashes       int
	TTM           []time.Duration `json:"-"` // per merged PR, sorted
	Violations    []string        // invariant violations (must be empty)
	MainBreakages int
	Stats         queue.Stats
}

type evKind int

const (
	evArrive evKind = iota
	evCIDone
	evUpdate
	evRemove
	evExternal
	evWake
)

type event struct {
	at   time.Time
	seq  int64
	kind evKind
	pr   queue.PRID
	run  queue.RunID
	n    int
}

type evHeap []*event

func (h evHeap) Len() int { return len(h) }
func (h evHeap) Less(i, j int) bool {
	if !h[i].at.Equal(h[j].at) {
		return h[i].at.Before(h[j].at)
	}
	return h[i].seq < h[j].seq
}
func (h evHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *evHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *evHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

type cacheKey struct {
	target int
	state  uint64
}

type cacheEntry struct {
	readyAt time.Time
	owner   queue.RunID
}

type activeRun struct {
	start    time.Time
	dur      time.Duration
	result   queue.Result
	inserted []cacheKey
}

type simulator struct {
	p      Params
	rng    *rand.Rand
	clock  *fakeClock
	w      *World
	store  *queue.MemStore
	flaky  *flaky.Tracker
	eng    *queue.Engine
	events evHeap
	seq    int64
	wakes  map[time.Time]bool

	active    map[queue.RunID]*activeRun
	cache     map[cacheKey]cacheEntry
	hits      int
	lookups   int
	ciMinutes float64
	wasted    float64
	runs      int
	crashes   int
	start     time.Time
	lastMerge time.Time
}

func (s *simulator) push(e *event) {
	s.seq++
	e.seq = s.seq
	heap.Push(&s.events, e)
}

// Run executes one simulation.
func Run(p Params) (*Result, error) {
	if p.Targets <= 0 {
		p.Targets = 60
	}
	if p.TouchMax <= 0 {
		p.TouchMax = 1
	}
	rng := rand.New(rand.NewSource(p.Seed))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &simulator{p: p, rng: rng, clock: &fakeClock{start}, store: &queue.MemStore{},
		flaky: flaky.New(2), wakes: map[time.Time]bool{}, active: map[queue.RunID]*activeRun{},
		cache: map[cacheKey]cacheEntry{}, start: start}
	s.w = newWorld(p, rng)
	s.generate()
	s.armCrash()
	if err := s.boot(); err != nil {
		return nil, err
	}
	if err := s.loop(); err != nil {
		return nil, err
	}
	return s.result(), nil
}

func (s *simulator) generate() {
	t := s.start
	var prev []*simPR
	for i := 1; i <= s.p.PRs; i++ {
		gap := s.rng.ExpFloat64() / s.p.ArrivalPerHour
		t = t.Add(time.Duration(gap * float64(time.Hour)))
		pr := &simPR{id: queue.PRID(i), version: 1, bad: map[int]bool{1: s.rng.Float64() < s.p.FailProb}, arrival: t}
		n := 1 + s.rng.Intn(s.p.TouchMax)
		for k := 0; k < n; k++ {
			// Skew towards leaves (high indices) like real repos: most
			// changes touch apps, few touch core libraries.
			x := int(math.Abs(s.rng.NormFloat64()) * float64(s.p.Targets) / 3)
			if x >= s.p.Targets {
				x = s.p.Targets - 1
			}
			pr.touched = append(pr.touched, s.p.Targets-1-x)
		}
		pr.affected = s.w.closure(pr.touched)
		if s.p.ConflictProb > 0 && s.rng.Float64() < s.p.ConflictProb {
			// Break when combined with a recent overlapping PR.
			for j := len(prev) - 1; j >= 0 && j >= len(prev)-10; j-- {
				q := prev[j]
				if overlaps(q.affected, pr.affected) {
					pr.partners = append(pr.partners, q.id)
					q.partners = append(q.partners, pr.id)
					s.w.partnerOf[pr.id], s.w.partnerOf[q.id] = true, true
					break
				}
			}
		}
		s.w.prs[pr.id] = pr
		prev = append(prev, pr)
		s.push(&event{at: t, kind: evArrive, pr: pr.id})
		if s.rng.Float64() < s.p.UpdateProb {
			s.push(&event{at: t.Add(time.Duration(s.rng.Intn(60)+1) * time.Minute), kind: evUpdate, pr: pr.id})
		}
		if s.rng.Float64() < s.p.RemoveProb {
			s.push(&event{at: t.Add(time.Duration(s.rng.Intn(60)+1) * time.Minute), kind: evRemove, pr: pr.id})
		}
	}
	span := t.Sub(s.start)
	for i := 0; i < s.p.ExternalPushes; i++ {
		s.push(&event{at: s.start.Add(time.Duration(s.rng.Int63n(int64(span) + 1))), kind: evExternal, n: i})
	}
}

func overlaps(a, b []int) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}

func (s *simulator) armCrash() {
	if s.p.CrashMean > 0 {
		// CrashAfter=n lets n-1 appends succeed, so every life makes progress.
		s.store.CrashAfter = 2 + s.rng.Intn(s.p.CrashMean)
	}
}

func (s *simulator) boot() error {
	for {
		e, err := queue.New(s.p.Queue, queue.Deps{CI: (*simCI)(s), Forge: s.w, Store: s.store, Clock: s.clock, Flaky: s.flaky})
		if err == nil {
			s.eng = e
			return nil
		}
		if !errors.Is(err, queue.ErrCrashed) {
			return err
		}
		s.crash() // crashed during recovery itself
	}
}

// crash drops all volatile state: the engine and every in-flight CI run.
func (s *simulator) crash() {
	s.crashes++
	now := s.clock.Now()
	for id, r := range s.active {
		s.ciMinutes += now.Sub(r.start).Minutes()
		s.wasted += now.Sub(r.start).Minutes()
		s.dropInserts(id, r, now)
	}
	s.active = map[queue.RunID]*activeRun{}
	s.store.Restart()
	s.armCrash()
	// The flake tracker is rebuilt from the log on recovery.
	s.flaky = flaky.New(2)
	s.eng = nil
}

// deliver runs f against the engine, handling crashes. If redeliver is set
// the input is retried after recovery (like a webhook redelivery).
func (s *simulator) deliver(redeliver bool, f func(*queue.Engine) error) error {
	for {
		err := f(s.eng)
		if err == nil {
			return nil
		}
		if !errors.Is(err, queue.ErrCrashed) {
			return err
		}
		s.crash()
		if err := s.boot(); err != nil {
			return err
		}
		if !redeliver {
			return nil
		}
	}
}

func (s *simulator) loop() error {
	limit := 200000 + 2000*s.p.PRs
	for steps := 0; ; steps++ {
		if steps > limit {
			return fmt.Errorf("sim: no progress after %d events (livelock?) active=%d lanes=%v", steps, s.eng.Active(), s.eng.Lanes())
		}
		if s.events.Len() == 0 {
			if t, ok := s.eng.NextWakeup(); ok {
				s.push(&event{at: maxTime(t, s.clock.Now()), kind: evWake})
				continue
			}
			if s.eng.Active() > 0 {
				return fmt.Errorf("sim: stuck with %d active PRs and nothing scheduled (lanes=%v waiting=%v)", s.eng.Active(), s.eng.Lanes(), s.eng.Waiting())
			}
			return nil
		}
		if debugHook != nil {
			debugHook(s.eng)
		}
		ev := heap.Pop(&s.events).(*event)
		s.clock.t = maxTime(ev.at, s.clock.t)
		var err error
		switch ev.kind {
		case evArrive:
			pr := s.w.prs[ev.pr]
			err = s.deliver(true, func(e *queue.Engine) error {
				if p, ok := e.PR(pr.id); ok && p.Version >= 1 {
					return nil // already recorded before a crash
				}
				return e.Enqueue(pr.id, fmt.Sprintf("pr%d@v%d", pr.id, pr.version), labels(pr.affected))
			})
		case evUpdate:
			pr := s.w.prs[ev.pr]
			p, ok := s.eng.PR(pr.id)
			if !ok || p.State.Terminal() {
				continue
			}
			newV := p.Version + 1
			pr.version = newV
			pr.bad[newV] = s.rng.Float64() < s.p.FailProb
			err = s.deliver(true, func(e *queue.Engine) error {
				p, _ := e.PR(pr.id)
				if p.State.Terminal() || p.Version >= newV {
					return nil // finished, or update already recorded before a crash
				}
				return e.Update(pr.id, fmt.Sprintf("pr%d@v%d", pr.id, newV), labels(pr.affected))
			})
		case evRemove:
			err = s.deliver(true, func(e *queue.Engine) error {
				if p, ok := e.PR(ev.pr); !ok || p.State.Terminal() {
					return nil
				}
				return e.Remove(ev.pr)
			})
		case evExternal:
			s.w.externalPush(ev.n)
			err = s.deliver(true, func(e *queue.Engine) error { return e.OnMainMoved() })
		case evCIDone:
			r, ok := s.active[ev.run]
			if !ok {
				continue
			}
			delete(s.active, ev.run)
			s.ciMinutes += r.dur.Minutes()
			res := r.result
			err = s.deliver(false, func(e *queue.Engine) error { return e.OnResult(res) })
		case evWake:
			delete(s.wakes, ev.at)
			err = s.deliver(false, func(e *queue.Engine) error { return e.Tick() })
		}
		if err != nil {
			return err
		}
		if t, ok := s.eng.NextWakeup(); ok && !s.wakes[t] {
			s.wakes[t] = true
			s.push(&event{at: maxTime(t, s.clock.Now()), kind: evWake})
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ------------------------------------------------------------ fake CI

type simCI simulator

func (c *simCI) Start(run queue.Run) {
	s := (*simulator)(c)
	s.runs++
	now := s.clock.Now()
	base, ok := s.w.commits[run.Base]
	if !ok {
		panic("sim: run on unknown base " + run.Base)
	}
	cur := base
	var parent *commit
	affectedSet := map[int]bool{}
	for _, g := range run.Groups {
		parent = cur
		cur = s.w.apply(cur, g)
		for _, it := range g {
			for _, t := range s.w.prs[it.PR].affected {
				affectedSet[t] = true
			}
		}
	}
	ar := &activeRun{start: now}
	res := queue.Result{RunID: run.ID, Commit: cur.id, Parent: parent.id}

	if s.rng.Float64() < s.p.InfraRate {
		ar.dur = time.Duration(s.rng.Intn(60)+1) * time.Second
		res.Outcome = queue.Infra
		ar.result = res
		s.active[run.ID] = ar
		s.push(&event{at: now.Add(ar.dur), kind: evCIDone, run: run.ID})
		return
	}
	failing := map[int]bool{}
	if cur.bad {
		for _, it := range run.Groups[len(run.Groups)-1] {
			failing[s.w.prs[it.PR].touched[0]] = true
		}
	}
	// Cost model: targets build in dependency order; each target is a cache
	// hit if an identical action (same target, same input state) finished
	// before this run reached it.
	ts := make([]int, 0, len(affectedSet))
	for t := range affectedSet {
		ts = append(ts, t)
	}
	sort.Ints(ts)
	tcur := now.Add(s.p.CIOverhead)
	for _, t := range ts {
		k := cacheKey{t, cur.state[t]}
		s.lookups++
		if e, ok := s.cache[k]; s.p.Cache && ok && !e.readyAt.After(tcur) {
			s.hits++
			tcur = tcur.Add(s.p.HitCost)
			continue
		}
		cost := time.Duration((0.5 + s.rng.Float64()) * float64(s.p.TargetCost))
		tcur = tcur.Add(cost)
		if s.p.Cache && !failing[t] {
			if e, ok := s.cache[k]; !ok || tcur.Before(e.readyAt) {
				s.cache[k] = cacheEntry{readyAt: tcur, owner: run.ID}
				ar.inserted = append(ar.inserted, k)
			}
		}
	}
	ar.dur = tcur.Sub(now)
	switch {
	case cur.bad:
		res.Outcome = queue.Fail
		res.FailedTests = append([]string(nil), cur.culprits...)
	case len(s.w.flakyTests) > 0 && s.rng.Float64() < s.p.FlakeRate:
		res.Outcome = queue.Fail
		res.FailedTests = []string{s.w.flakyTests[s.rng.Intn(len(s.w.flakyTests))]}
	default:
		res.Outcome = queue.Pass
	}
	if res.Outcome == queue.Pass || allFlaky(res.FailedTests, s.w.flakyTests) {
		// Recorded for the invariant checker: CI accepted this exact commit
		// (a failure only on known-flaky tests is accepted once those tests
		// are quarantined).
		if res.Outcome == queue.Pass {
			s.w.passedCI[cur.id] = true
		} else {
			s.w.flakyOnly[cur.id] = true
		}
	}
	ar.result = res
	s.active[run.ID] = ar
	s.push(&event{at: now.Add(ar.dur), kind: evCIDone, run: run.ID})
}

func allFlaky(failed, flakyTests []string) bool {
	if len(failed) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, f := range flakyTests {
		set[f] = true
	}
	for _, f := range failed {
		if !set[f] {
			return false
		}
	}
	return true
}

func (c *simCI) Cancel(id queue.RunID) {
	s := (*simulator)(c)
	r, ok := s.active[id]
	if !ok {
		return
	}
	now := s.clock.Now()
	delete(s.active, id)
	s.ciMinutes += now.Sub(r.start).Minutes()
	s.wasted += now.Sub(r.start).Minutes()
	s.dropInserts(id, r, now)
}

func (s *simulator) dropInserts(id queue.RunID, r *activeRun, now time.Time) {
	for _, k := range r.inserted {
		if e, ok := s.cache[k]; ok && e.owner == id && e.readyAt.After(now) {
			delete(s.cache, k)
		}
	}
}

// ------------------------------------------------------------ results

func (s *simulator) result() *Result {
	r := &Result{Params: s.p, CIMinutes: s.ciMinutes, WastedMinutes: s.wasted, Runs: s.runs, Crashes: s.crashes}
	r.Stats = s.eng.Stats()
	var ttm []time.Duration
	inMain := s.w.mainItems()
	for _, p := range s.eng.PRs() {
		truth := s.w.prs[p.ID]
		switch p.State {
		case queue.Merged:
			r.Merged++
			ttm = append(ttm, p.DoneAt.Sub(truth.arrival))
			s.lastMerge = maxTime(s.lastMerge, p.DoneAt)
			if v, ok := inMain[p.ID]; !ok || v != p.Version {
				s.w.Violations = append(s.w.Violations, fmt.Sprintf("PR %d reported merged but v%d not on main", p.ID, p.Version))
			}
		case queue.Ejected:
			r.Ejected++
			if !truth.bad[p.Version] {
				r.EjectedGood++
			}
			if _, ok := inMain[p.ID]; ok {
				s.w.Violations = append(s.w.Violations, fmt.Sprintf("PR %d ejected but on main", p.ID))
			}
		case queue.Cancelled:
			r.Cancelled++
			if _, ok := inMain[p.ID]; ok {
				s.w.Violations = append(s.w.Violations, fmt.Sprintf("PR %d cancelled but on main", p.ID))
			}
		default:
			s.w.Violations = append(s.w.Violations, fmt.Sprintf("PR %d ended non-terminal: %s", p.ID, p.State))
		}
	}
	for id := range inMain {
		if p, ok := s.eng.PR(id); !ok || p.State != queue.Merged {
			s.w.Violations = append(s.w.Violations, fmt.Sprintf("PR %d on main but state %s", id, p.State))
		}
	}
	for _, v := range s.w.Violations {
		if len(v) > 11 && v[:11] == "main broken" {
			r.MainBreakages++
		}
	}
	r.Violations = s.w.Violations
	sort.Slice(ttm, func(i, j int) bool { return ttm[i] < ttm[j] })
	r.TTM = ttm
	if n := len(ttm); n > 0 {
		r.P50 = ttm[n/2]
		r.P95 = ttm[min(n-1, int(math.Ceil(float64(n)*0.95))-1)]
	}
	end := s.lastMerge
	if end.IsZero() {
		end = s.clock.Now()
	}
	r.Hours = end.Sub(s.start).Hours()
	if r.Hours > 0 {
		r.Throughput = float64(r.Merged) / r.Hours
	}
	if r.Merged > 0 {
		r.CIPerMerged = r.CIMinutes / float64(r.Merged)
	}
	if s.lookups > 0 {
		r.CacheHitRatio = float64(s.hits) / float64(s.lookups)
	}
	return r
}

var debugHook func(*queue.Engine)
