package queue

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// harness: scripted CI, in-memory forge with deterministic commit ids.
type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type forge struct {
	main    string
	parent  map[string]string
	history []string
}

func newForge() *forge {
	return &forge{main: "root", parent: map[string]string{}, history: []string{"root"}}
}

func (f *forge) apply(parent string, g []Item) string {
	var parts []string
	for _, it := range g {
		parts = append(parts, fmt.Sprintf("%d.%d", it.PR, it.Version))
	}
	id := parent + "+" + strings.Join(parts, ",")
	f.parent[id] = parent
	return id
}
func (f *forge) Head() (string, error) { return f.main, nil }
func (f *forge) Advance(old, next string) error {
	if old != f.main {
		return ErrStale
	}
	if f.parent[next] != old {
		return fmt.Errorf("non-fast-forward")
	}
	f.main = next
	f.history = append(f.history, next)
	return nil
}
func (f *forge) Contains(c string) (bool, error) {
	for _, h := range f.history {
		if h == c {
			return true, nil
		}
	}
	return false, nil
}

type ci struct {
	f        *forge
	started  []Run
	live     map[RunID]Run
	canceled []RunID
}

func (c *ci) Start(r Run) {
	c.started = append(c.started, r)
	c.live[r.ID] = r
}
func (c *ci) Cancel(id RunID) { c.canceled = append(c.canceled, id); delete(c.live, id) }

type h struct {
	t     *testing.T
	e     *Engine
	ci    *ci
	f     *forge
	clk   *clock
	store *MemStore
	cfg   Config
}

func newH(t *testing.T, cfg Config) *h {
	f := newForge()
	x := &h{t: t, f: f, ci: &ci{f: f, live: map[RunID]Run{}}, clk: &clock{time.Unix(0, 0)}, store: &MemStore{}, cfg: cfg}
	x.boot()
	return x
}

func (x *h) boot() {
	e, err := New(x.cfg, Deps{CI: x.ci, Forge: x.f, Store: x.store, Clock: x.clk})
	if err != nil {
		x.t.Fatal(err)
	}
	x.e = e
}

func (x *h) must(err error) {
	x.t.Helper()
	if err != nil {
		x.t.Fatal(err)
	}
}

// liveRun returns the live run whose last group contains pr.
func (x *h) liveRun(pr PRID) Run {
	x.t.Helper()
	var ids []RunID
	for id := range x.ci.live {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	for _, id := range ids {
		r := x.ci.live[id]
		for _, it := range r.Groups[len(r.Groups)-1] {
			if it.PR == pr {
				return r
			}
		}
	}
	x.t.Fatalf("no live run for PR %d (live=%v)", pr, x.ci.live)
	return Run{}
}

func (x *h) result(r Run, o Outcome, tests ...string) {
	x.t.Helper()
	delete(x.ci.live, r.ID)
	c := r.Base
	var parent string
	for _, g := range r.Groups {
		parent = c
		c = x.f.apply(c, g)
	}
	x.must(x.e.OnResult(Result{RunID: r.ID, Outcome: o, Commit: c, Parent: parent, FailedTests: tests}))
}

func (x *h) pass(pr PRID) { x.t.Helper(); x.result(x.liveRun(pr), Pass) }
func (x *h) fail(pr PRID, t ...string) {
	x.t.Helper()
	x.result(x.liveRun(pr), Fail, append(t, "t")[:max(1, len(t))]...)
}
func (x *h) state(pr PRID) State          { p, _ := x.e.PR(pr); return p.State }
func (x *h) reason(pr PRID) string        { p, _ := x.e.PR(pr); return p.Reason }
func (x *h) enqueue(pr PRID, t ...string) { x.must(x.e.Enqueue(pr, fmt.Sprintf("h%d", pr), t)) }

func (x *h) expect(want map[PRID]State) {
	x.t.Helper()
	for pr, s := range want {
		if got := x.state(pr); got != s {
			x.t.Errorf("PR %d: state %s, want %s (reason %q)", pr, got, s, x.reason(pr))
		}
	}
}

func TestSerialHappyPathAndEject(t *testing.T) {
	x := newH(t, Config{FlakyRetries: -1})
	x.enqueue(1)
	x.enqueue(2)
	x.enqueue(3)
	if len(x.ci.live) != 1 {
		t.Fatalf("serial should test one at a time, live=%d", len(x.ci.live))
	}
	x.pass(1)
	x.fail(2)
	x.pass(3)
	x.expect(map[PRID]State{1: Merged, 2: Ejected, 3: Merged})
	if x.reason(2) != ReasonTestFailed {
		t.Fatal(x.reason(2))
	}
	if len(x.f.history) != 3 {
		t.Fatalf("history %v", x.f.history)
	}
}

func TestBatchBisect(t *testing.T) {
	x := newH(t, Config{BatchSize: 4, FlakyRetries: -1})
	for i := PRID(1); i <= 4; i++ {
		x.enqueue(i)
	}
	// First enqueue started a run with only PR 1 (window opened at once);
	// the next three form a batch.
	x.pass(1)
	r := x.liveRun(4)
	if n := len(r.Groups[len(r.Groups)-1]); n != 3 {
		t.Fatalf("batch size %d, want 3", n)
	}
	x.result(r, Fail, "t")
	// Bisect: [2] then [3,4].
	x.pass(2)
	r = x.liveRun(4)
	x.result(r, Fail, "t")
	x.pass(3)
	x.fail(4)
	x.expect(map[PRID]State{1: Merged, 2: Merged, 3: Merged, 4: Ejected})
	if s := x.e.Stats(); s.Bisections != 2 {
		t.Fatalf("bisections %d", s.Bisections)
	}
}

func TestSpeculativeChainAndCulpritSkip(t *testing.T) {
	x := newH(t, Config{Depth: 3, SkipSuspects: true, FlakyRetries: -1})
	x.enqueue(1)
	x.enqueue(2)
	x.enqueue(3)
	if len(x.ci.live) != 3 {
		t.Fatalf("want 3 speculative runs, got %d", len(x.ci.live))
	}
	r3 := x.liveRun(3)
	if len(r3.Groups) != 3 {
		t.Fatalf("run for 3 should test 1+2+3, got %v", r3.Groups)
	}
	// 2 fails while 1 still testing: 3 is rebuilt as 1+3.
	x.fail(2)
	r3 = x.liveRun(3)
	if len(r3.Groups) != 2 || r3.Groups[0][0].PR != 1 || r3.Groups[1][0].PR != 3 {
		t.Fatalf("3 not rebuilt without 2: %v", r3.Groups)
	}
	// 3 passing early must not merge before 1.
	x.pass(3)
	if x.state(3) == Merged {
		t.Fatal("merged out of order")
	}
	x.pass(1)
	x.expect(map[PRID]State{1: Merged, 2: Ejected, 3: Merged})
}

func TestSuspectClearedWhenAheadFails(t *testing.T) {
	x := newH(t, Config{Depth: 3, SkipSuspects: true, FlakyRetries: -1})
	x.enqueue(1)
	x.enqueue(2)
	x.enqueue(3)
	x.fail(2) // suspect: 3 rebuilt as 1+3
	x.fail(1) // 1 is the real culprit: 2's evidence (1+2) is void
	x.expect(map[PRID]State{1: Ejected})
	if x.state(2) == Ejected {
		t.Fatal("2 ejected on evidence that included the real culprit")
	}
	x.pass(2)
	x.pass(3)
	x.expect(map[PRID]State{2: Merged, 3: Merged})
}

func TestFlakyRetryAndQuarantine(t *testing.T) {
	x := newH(t, Config{FlakyRetries: 1})
	for i := PRID(1); i <= 3; i++ {
		x.enqueue(i)
		r := x.liveRun(i)
		x.result(r, Fail, "flaky_test")
		if i <= 2 {
			r2 := x.liveRun(i)
			if r2.Base != r.Base || fmt.Sprint(r2.Groups) != fmt.Sprint(r.Groups) || r2.Attempt != 1 {
				t.Fatal("retry not on identical inputs")
			}
			x.result(r2, Pass)
		}
	}
	// Two flakes quarantined the test, so PR 3's failure on it alone
	// passes without a retry.
	x.expect(map[PRID]State{1: Merged, 2: Merged, 3: Merged})
	if !x.e.Flaky.Quarantined("flaky_test") {
		t.Fatal("not quarantined")
	}
	// A real failure is still caught after retry.
	x.enqueue(4)
	x.fail(4, "real")
	x.fail(4, "real")
	x.expect(map[PRID]State{4: Ejected})
}

func TestInfraRetryBackoffThenEject(t *testing.T) {
	x := newH(t, Config{MaxInfraRetries: 2, InfraBackoff: time.Minute})
	x.enqueue(1)
	for i := 0; i < 2; i++ {
		x.result(x.liveRun(1), Infra)
		if x.state(1) == Ejected {
			t.Fatal("ejected on infra error")
		}
		if len(x.ci.live) != 0 {
			t.Fatal("retried without backoff")
		}
		w, ok := x.e.NextWakeup()
		if !ok {
			t.Fatal("no wakeup scheduled")
		}
		x.clk.t = w
		x.must(x.e.Tick())
	}
	x.result(x.liveRun(1), Infra)
	x.expect(map[PRID]State{1: Ejected})
	if x.reason(1) != ReasonInfra {
		t.Fatal(x.reason(1))
	}
}

func TestTimeout(t *testing.T) {
	x := newH(t, Config{RunTimeout: 10 * time.Minute, FlakyRetries: -1})
	x.enqueue(1)
	x.clk.t = x.clk.t.Add(11 * time.Minute)
	x.must(x.e.Tick())
	x.expect(map[PRID]State{1: Ejected})
	if x.reason(1) != ReasonTimeout {
		t.Fatal(x.reason(1))
	}
	if len(x.ci.canceled) != 1 {
		t.Fatal("timed-out run not cancelled")
	}
}

func TestUpdateWhileTestingRetests(t *testing.T) {
	x := newH(t, Config{Depth: 2, FlakyRetries: -1})
	x.enqueue(1)
	x.enqueue(2)
	old := x.liveRun(1)
	x.must(x.e.Update(1, "h1-v2", nil))
	if _, live := x.ci.live[old.ID]; live {
		t.Fatal("run of old version still live")
	}
	// The stale result must be ignored even if CI reports it.
	x.must(x.e.OnResult(Result{RunID: old.ID, Outcome: Pass, Commit: "x", Parent: "root"}))
	if x.state(1) == Merged {
		t.Fatal("merged stale version")
	}
	x.pass(2)
	x.pass(1)
	p, _ := x.e.PR(1)
	if p.State != Merged || p.Version != 2 {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(x.f.main, "1.2") {
		t.Fatalf("main %s lacks v2", x.f.main)
	}
}

func TestRemoveWhileQueued(t *testing.T) {
	x := newH(t, Config{Depth: 2, FlakyRetries: -1})
	x.enqueue(1)
	x.enqueue(2)
	x.enqueue(3)
	x.must(x.e.Remove(1))
	x.pass(2)
	x.pass(3)
	x.expect(map[PRID]State{1: Cancelled, 2: Merged, 3: Merged})
	if err := x.e.Remove(1); err == nil {
		t.Fatal("double remove accepted")
	}
}

func TestExternalPushRetests(t *testing.T) {
	x := newH(t, Config{FlakyRetries: -1})
	x.enqueue(1)
	r := x.liveRun(1)
	x.f.parent["ext"] = x.f.main
	x.f.main = "ext"
	x.f.history = append(x.f.history, "ext")
	// Even without OnMainMoved, a pass on the old base must not merge.
	x.result(r, Pass)
	if x.state(1) == Merged {
		t.Fatal("merged a pass built on a stale base")
	}
	x.pass(1)
	x.expect(map[PRID]State{1: Merged})
	if x.f.parent[x.f.main] != "ext" {
		t.Fatal("not built on the new main")
	}
}

func TestLanesRunInParallel(t *testing.T) {
	x := newH(t, Config{Lanes: true, FlakyRetries: -1})
	x.enqueue(1, "//a")
	x.enqueue(2, "//b")
	x.enqueue(3, "//a", "//b") // overlaps both: waits
	if len(x.ci.live) != 2 {
		t.Fatalf("want 2 parallel lanes, got %d live", len(x.ci.live))
	}
	if w := x.e.Waiting(); len(w) != 1 || w[0] != 3 {
		t.Fatalf("waiting %v", w)
	}
	x.pass(1)
	// 2's run is now stale (main moved); its result is discarded and it is
	// re-verified on the new main.
	x.pass(2)
	if x.state(2) == Merged {
		t.Fatal("merged a run built before main moved")
	}
	x.pass(2)
	x.pass(3)
	x.expect(map[PRID]State{1: Merged, 2: Merged, 3: Merged})
}

func TestCrashRecoveryReconcilesMerge(t *testing.T) {
	x := newH(t, Config{FlakyRetries: -1})
	x.enqueue(1)
	x.enqueue(2)
	r := x.liveRun(1)
	// Crash right after merge_intent is written and Advance succeeds,
	// before "merged" is recorded: allow exactly 1 more append.
	x.store.CrashAfter = 2
	delete(x.ci.live, r.ID)
	c := x.f.apply(r.Base, r.Groups[0])
	err := x.e.OnResult(Result{RunID: r.ID, Outcome: Pass, Commit: c, Parent: r.Base})
	if !errors.Is(err, ErrCrashed) {
		t.Fatalf("expected crash, got %v", err)
	}
	if x.f.main != c {
		t.Fatal("advance should have happened before the crash")
	}
	x.store.Restart()
	x.ci.live = map[RunID]Run{}
	x.boot()
	x.expect(map[PRID]State{1: Merged})
	x.pass(2)
	x.expect(map[PRID]State{2: Merged})
}

func TestCrashBeforeAdvanceRetests(t *testing.T) {
	x := newH(t, Config{FlakyRetries: -1})
	x.enqueue(1)
	r := x.liveRun(1)
	x.store.CrashAfter = 1 // merge_intent itself fails
	delete(x.ci.live, r.ID)
	c := x.f.apply(r.Base, r.Groups[0])
	if err := x.e.OnResult(Result{RunID: r.ID, Outcome: Pass, Commit: c, Parent: r.Base}); !errors.Is(err, ErrCrashed) {
		t.Fatal(err)
	}
	x.store.Restart()
	x.ci.live = map[RunID]Run{}
	x.boot()
	if x.state(1) != Testing {
		t.Fatalf("PR 1 should be re-tested after recovery, state %s", x.state(1))
	}
	x.pass(1)
	x.expect(map[PRID]State{1: Merged})
}

func TestDuplicateEnqueueRejected(t *testing.T) {
	x := newH(t, Config{})
	x.enqueue(1)
	if err := x.e.Enqueue(1, "h", nil); err == nil {
		t.Fatal("duplicate enqueue accepted")
	}
}

func TestCompactionPreservesState(t *testing.T) {
	x := newH(t, Config{Depth: 2, FlakyRetries: 1})
	x.enqueue(1)
	x.enqueue(2)
	x.enqueue(3)
	x.enqueue(4)
	r := x.liveRun(1)
	x.result(r, Fail, "flaky")
	x.pass(1) // flake recorded
	x.fail(2)
	x.fail(2) // 2 ejected
	before := x.store.Len()
	want := fmt.Sprint(stripRuns(x.e.PRs()))
	if err := x.e.Compact(); err != nil {
		t.Fatal(err)
	}
	if x.store.Len() >= before {
		t.Fatalf("log not compacted: %d -> %d", before, x.store.Len())
	}
	x.ci.live = map[RunID]Run{}
	x.boot()
	if got := fmt.Sprint(stripRuns(x.e.PRs())); got != want {
		t.Fatalf("after compaction:\n got %s\nwant %s", got, want)
	}
	if st := x.e.Flaky.Stats(); len(st) != 2 || st[0].Test != "flaky" || st[0].Flakes != 1 || st[1].Failures != 1 {
		t.Fatalf("flake stats lost: %+v", st)
	}
	// The queue keeps working, in the same order, and enqueue order is
	// preserved for new PRs.
	x.enqueue(5)
	x.pass(3)
	x.pass(4)
	x.pass(5)
	x.expect(map[PRID]State{1: Merged, 2: Ejected, 3: Merged, 4: Merged, 5: Merged})
	if p, _ := x.e.PR(5); p.Seq <= 4 {
		t.Fatalf("seq not continued after compaction: %d", p.Seq)
	}
}

// stripRuns normalises fields that legitimately differ after recovery.
func stripRuns(prs []PR) []PR {
	for i := range prs {
		prs[i].Runs = 0
		if !prs[i].State.Terminal() {
			prs[i].State = Queued
		}
	}
	return prs
}
