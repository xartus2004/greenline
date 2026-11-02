// Package queue is greenline's merge-queue engine. It is pure logic: time,
// CI, the code host and durable storage are injected, so the same engine runs
// in the deterministic simulator, the property tests and the real server.
//
// # Model
//
// The queue is split into lanes (one lane unless Config.Lanes is set). Each
// lane is an ordered list of groups; a group is one or more PRs that are
// merged as a single commit (a batch). The first Config.Depth groups of a
// lane are tested speculatively: the run for group i tests the tree
// main + g0 + ... + gi (skipping groups already suspected bad), so serial is
// Depth=1,Batch=1, batching is Batch>1 and Zuul-style speculation is
// Depth>1.
//
// # The invariant
//
// A group is merged only when it is at the head of its lane, its run passed,
// and the run's parent commit is exactly the current main. Merging is a
// compare-and-swap fast-forward by one commit (Forge.Advance), so every
// commit the queue puts on main is a commit that passed CI as-is.
package queue

import (
	"errors"
	"time"
)

// PRID identifies a pull request.
type PRID int64

// State is a PR's lifecycle state.
type State string

const (
	Queued    State = "QUEUED"
	Testing   State = "TESTING"
	Merged    State = "MERGED"
	Ejected   State = "EJECTED"
	Cancelled State = "CANCELLED"
)

// Terminal reports whether s is a final state.
func (s State) Terminal() bool { return s == Merged || s == Ejected || s == Cancelled }

// Ejection reasons.
const (
	ReasonTestFailed  = "test_failed"
	ReasonTimeout     = "timeout"
	ReasonInfra       = "infra_retries_exhausted"
	ReasonRemoved     = "removed"
	ReasonSuperseded  = "superseded_by_update"
	ReasonUnmergeable = "unmergeable"
)

// MergeConflictTest is the pseudo-test CI reports when a group does not
// merge cleanly. It is never retried and ejects with ReasonUnmergeable.
const MergeConflictTest = "<merge-conflict>"

// PR is a pull request in the queue.
type PR struct {
	ID      PRID     `json:"id"`
	Version int      `json:"version"` // bumped on every update
	Head    string   `json:"head"`    // head commit of the PR branch
	Targets []string `json:"targets"` // sorted affected targets (lanes)

	State      State     `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	FailedTest []string  `json:"failed_tests,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	DoneAt     time.Time `json:"done_at,omitempty"`
	Commit     string    `json:"commit,omitempty"` // main commit it landed in
	Runs       int       `json:"runs"`             // CI runs that included it
	Seq        int64     `json:"seq"`              // queue order
}

// Item is a PR at a specific version, as included in a run.
type Item struct {
	PR      PRID   `json:"pr"`
	Version int    `json:"version"`
	Head    string `json:"head"`
}

// RunID identifies a CI run.
type RunID uint64

// Run asks CI to build and test the commit obtained by applying Groups, in
// order, to Base: each group becomes one commit whose parent is the
// previous one. CI must report the final commit and its parent.
type Run struct {
	ID     RunID    `json:"id"`
	Base   string   `json:"base"`
	Groups [][]Item `json:"groups"`
	// Targets is the union of affected targets of all items (a hint for
	// what to build; CI may build more).
	Targets []string `json:"targets,omitempty"`
	Attempt int      `json:"attempt"` // >0 for retries on identical inputs
}

// Outcome is a CI result class.
type Outcome int

const (
	Pass Outcome = iota
	Fail
	Infra // CI itself broke; retried, never ejects on its own
)

func (o Outcome) String() string { return [...]string{"pass", "fail", "infra"}[o] }

// Result is CI's report for a run.
type Result struct {
	RunID       RunID
	Outcome     Outcome
	Commit      string   // tested commit
	Parent      string   // its parent (Base + all groups but the last)
	FailedTests []string // for Fail
	Err         string
	CIMinutes   float64 // optional accounting
}

// CI starts and cancels runs. Start must not call back into the engine
// synchronously; results are delivered later via Engine.OnResult.
type CI interface {
	Start(r Run)
	Cancel(id RunID)
}

// ErrStale is returned by Forge.Advance when main is not at the expected
// commit.
var ErrStale = errors.New("queue: main moved")

// Forge is the code host as seen by the engine.
type Forge interface {
	// Head returns main's current commit.
	Head() (string, error)
	// Advance atomically moves main from old to next. It must fail with
	// ErrStale if main != old, and must refuse a next whose parent is not
	// old (so main only ever fast-forwards by exactly one tested commit).
	Advance(old, next string) error
	// Contains reports whether commit is on main's first-parent history.
	Contains(commit string) (bool, error)
}

// Clock provides time.
type Clock interface{ Now() time.Time }

// Config selects the strategy and its knobs.
type Config struct {
	BatchSize int  // PRs per group (default 1)
	Depth     int  // speculative groups tested at once per lane (default 1)
	Lanes     bool // independent lanes for disjoint affected targets
	// Bisect splits a failing batch instead of ejecting all its PRs.
	// Always on for BatchSize>1.

	// FlakyRetries reruns a failing run on identical inputs this many times
	// before believing the failure (default 1; -1 disables).
	FlakyRetries int
	// MaxInfraRetries before giving up on a group (default 5).
	MaxInfraRetries int
	// InfraBackoff is the base backoff after an infra error (default 30s).
	InfraBackoff time.Duration
	// RunTimeout fails a run that has not reported (0 = none).
	RunTimeout time.Duration
	// Zuul-style: when a non-head group fails, immediately rebuild the
	// groups behind it without it (default true via NewConfig).
	SkipSuspects bool
}

func (c Config) withDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = 1
	}
	if c.Depth <= 0 {
		c.Depth = 1
	}
	if c.FlakyRetries == 0 {
		c.FlakyRetries = 1
	} else if c.FlakyRetries < 0 {
		c.FlakyRetries = 0
	}
	if c.MaxInfraRetries <= 0 {
		c.MaxInfraRetries = 5
	}
	if c.InfraBackoff <= 0 {
		c.InfraBackoff = 30 * time.Second
	}
	return c
}

// Strategy returns a named preset.
func Strategy(name string) (Config, error) {
	switch name {
	case "serial":
		return Config{}, nil
	case "batch":
		return Config{BatchSize: 4}, nil
	case "speculative":
		return Config{Depth: 4, SkipSuspects: true}, nil
	case "lanes":
		return Config{Lanes: true}, nil
	case "lanes+speculative":
		return Config{Lanes: true, Depth: 4, SkipSuspects: true}, nil
	}
	return Config{}, errors.New("unknown strategy " + name)
}

// Strategies lists preset names.
var Strategies = []string{"serial", "batch", "speculative", "lanes", "lanes+speculative"}
