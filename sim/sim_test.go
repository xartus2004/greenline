package sim

import (
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xartus2004/greenline/queue"
)

var long = flag.Bool("long", false, "run many more property-test seeds")

func small(seed int64, cfg queue.Config) Params {
	p := Defaults()
	p.Seed = seed
	p.PRs = 15
	p.ArrivalPerHour = 30
	p.Targets = 12
	p.Queue = cfg
	return p
}

// TestInvariantProperty: across random PR streams, failures, flakes, infra
// errors, updates, removals, external pushes and crash points, the queue
// never merges an untested commit, never breaks main, and every PR ends in
// a terminal state.
func TestInvariantProperty(t *testing.T) {
	seeds := 1000
	if *long {
		seeds = 20000
	}
	if testing.Short() {
		seeds = 100
	}
	strategies := map[string]queue.Config{
		"serial":           {},
		"batch":            {BatchSize: 4},
		"speculative":      {Depth: 4, SkipSuspects: true},
		"speculative-wait": {Depth: 3},
		"lanes":            {Lanes: true},
		"lanes+spec+batch": {Lanes: true, Depth: 3, BatchSize: 2, SkipSuspects: true},
		"timeouts":         {Depth: 2, RunTimeout: 20 * time.Minute, SkipSuspects: true},
	}
	for name, cfg := range strategies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for seed := int64(1); seed <= int64(seeds); seed++ {
				p := small(seed, cfg)
				r := randomize(p, seed)
				res, err := Run(r)
				if err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
				if len(res.Violations) > 0 {
					t.Fatalf("seed %d (%+v): violations:\n%s", seed, r, strings.Join(res.Violations, "\n"))
				}
				if res.Merged+res.Ejected+res.Cancelled != r.PRs {
					t.Fatalf("seed %d: %d+%d+%d != %d", seed, res.Merged, res.Ejected, res.Cancelled, r.PRs)
				}
			}
		})
	}
}

// randomize varies the scenario per seed.
func randomize(p Params, seed int64) Params {
	x := uint64(seed) * 0x9E3779B97F4A7C15
	f := func(n uint64) float64 { x ^= x >> 13; x *= 0xff51afd7ed558ccd; return float64(x%n) / float64(n) }
	p.FailProb = 0.4 * f(1000)
	p.ConflictProb = 0.3 * f(1000)
	p.FlakeRate = 0.2 * f(1000)
	p.InfraRate = 0.1 * f(1000)
	p.UpdateProb = 0.2 * f(1000)
	p.RemoveProb = 0.1 * f(1000)
	p.ExternalPushes = int(f(3) * 3)
	p.TouchMax = 1 + int(f(3)*3)
	if f(10) < 0.7 {
		p.CrashMean = 1 + int(f(40)*40)
	}
	p.Cache = f(2) < 0.5
	if f(10) < 0.5 {
		p.CompactProb = 0.05 + 0.3*f(100)
	}
	return p
}

func TestDeterministic(t *testing.T) {
	p := Defaults()
	p.Seed, p.PRs = 7, 60
	p.Queue = queue.Config{Depth: 3, SkipSuspects: true}
	a, err := Run(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Run(p)
	if fmt.Sprintf("%v %v %v %v", a.Merged, a.P95, a.CIMinutes, a.Stats) != fmt.Sprintf("%v %v %v %v", b.Merged, b.P95, b.CIMinutes, b.Stats) {
		t.Fatal("simulation not deterministic per seed")
	}
}
