// Package flaky tracks flaky tests. A test that fails and then passes on
// identical inputs is flaky; once a test has flaked Threshold times it is
// quarantined: its failures no longer eject PRs, but they are still
// reported.
package flaky

import (
	"sort"
	"sync"
)

// Stat is per-test flake history.
type Stat struct {
	Test        string `json:"test"`
	Flakes      int    `json:"flakes"`       // fail→pass on identical inputs
	Failures    int    `json:"failures"`     // failures confirmed by retry
	Quarantined bool   `json:"quarantined"`  // ignored for gating
	Ignored     int    `json:"ignored_fail"` // failures ignored while quarantined
}

// Tracker records flakes. Safe for concurrent use.
type Tracker struct {
	// Threshold is the number of flakes that quarantines a test
	// (default 2; negative disables quarantine).
	Threshold int

	mu    sync.Mutex
	stats map[string]*Stat
}

// New returns a tracker with the given threshold.
func New(threshold int) *Tracker { return &Tracker{Threshold: threshold, stats: map[string]*Stat{}} }

func (t *Tracker) get(test string) *Stat {
	if t.stats == nil {
		t.stats = map[string]*Stat{}
	}
	s, ok := t.stats[test]
	if !ok {
		s = &Stat{Test: test}
		t.stats[test] = s
	}
	return s
}

func (t *Tracker) threshold() int {
	if t.Threshold == 0 {
		return 2
	}
	return t.Threshold
}

// RecordFlake notes that test failed then passed on identical inputs, and
// reports whether it is now quarantined.
func (t *Tracker) RecordFlake(test string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(test)
	s.Flakes++
	if th := t.threshold(); th > 0 && s.Flakes >= th {
		s.Quarantined = true
	}
	return s.Quarantined
}

// RecordFailure notes a failure confirmed on retry.
func (t *Tracker) RecordFailure(test string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(test).Failures++
}

// Quarantined reports whether test is quarantined.
func (t *Tracker) Quarantined(test string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.stats[test]
	return ok && s.Quarantined
}

// Gating splits failed tests into those that gate (block merging) and those
// ignored because they are quarantined; ignored ones are counted.
func (t *Tracker) Gating(failed []string) (gating, ignored []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range failed {
		if s, ok := t.stats[f]; ok && s.Quarantined {
			s.Ignored++
			ignored = append(ignored, f)
		} else {
			gating = append(gating, f)
		}
	}
	return
}

// Stats returns a snapshot sorted by test name.
func (t *Tracker) Stats() []Stat {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Stat, 0, len(t.stats))
	for _, s := range t.stats {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Test < out[j].Test })
	return out
}
