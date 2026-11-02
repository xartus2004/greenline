package sim

import (
	"container/heap"
	"math/rand"
	"sort"
	"time"

	"github.com/xartus2004/greenline/queue"
)

// RunNoQueue simulates the baseline without a merge queue: each PR's CI
// runs once against the main it was branched from, and a passing PR is
// merged onto whatever main is by then without re-testing (like a protected
// branch that requires green checks but not up-to-date branches). A merge
// that breaks main is counted and instantly reverted.
func RunNoQueue(p Params) (*Result, error) {
	if p.Targets <= 0 {
		p.Targets = 60
	}
	if p.TouchMax <= 0 {
		p.TouchMax = 1
	}
	rng := rand.New(rand.NewSource(p.Seed))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &simulator{p: p, rng: rng, clock: &fakeClock{start}, start: start, wakes: map[time.Time]bool{}}
	s.w = newWorld(p, rng)
	p.UpdateProb, p.RemoveProb, p.ExternalPushes = 0, 0, 0
	s.p = p
	s.generate()
	res := &Result{Params: p}
	type pending struct {
		base *commit
		ver  int
	}
	running := map[queue.PRID]pending{}
	for s.events.Len() > 0 {
		ev := heap.Pop(&s.events).(*event)
		s.clock.t = ev.at
		pr := s.w.prs[ev.pr]
		switch ev.kind {
		case evArrive:
			// CI on branch = main at branch time + PR.
			item := []queue.Item{{PR: pr.id, Version: 1}}
			tested := s.w.apply(s.w.main, item)
			dur := s.p.CIOverhead
			for range pr.affected {
				dur += time.Duration((0.5 + rng.Float64()) * float64(s.p.TargetCost))
			}
			res.CIMinutes += dur.Minutes()
			res.Runs++
			if tested.bad {
				res.Ejected++
				continue // its own CI caught it
			}
			running[pr.id] = pending{base: s.w.main, ver: 1}
			s.push(&event{at: ev.at.Add(dur), kind: evCIDone, pr: pr.id})
		case evCIDone:
			delete(running, pr.id)
			merged := s.w.apply(s.w.main, []queue.Item{{PR: pr.id, Version: 1}})
			if merged.bad {
				res.MainBreakages++
				continue // reverted
			}
			s.w.main = merged
			s.w.history = append(s.w.history, merged)
			res.Merged++
			res.TTM = append(res.TTM, ev.at.Sub(pr.arrival))
			s.lastMerge = ev.at
		}
	}
	sort.Slice(res.TTM, func(i, j int) bool { return res.TTM[i] < res.TTM[j] })
	res.Hours = s.lastMerge.Sub(start).Hours()
	if res.Hours > 0 {
		res.Throughput = float64(res.Merged) / res.Hours
	}
	return res, nil
}
