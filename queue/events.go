package queue

import (
	"errors"
	"sync"
	"time"

	"github.com/xartus2004/greenline/flaky"
)

// Event types written ahead of every externally visible state change.
const (
	EvEnqueue     = "enqueue"
	EvUpdate      = "update"
	EvRemove      = "remove"
	EvEject       = "eject"
	EvMergeIntent = "merge_intent"
	EvMerged      = "merged"
	EvFlake       = "flake"
	// Compaction writes these in place of the history they summarise.
	EvSnapshotPR    = "snapshot_pr"
	EvSnapshotFlaky = "snapshot_flaky"
)

// Event is one durable state transition. The engine's state is a pure
// function of its event log plus the forge, which is what makes crash
// recovery safe: on restart the log is replayed, any merge whose outcome was
// not recorded is reconciled against the forge, and everything else that was
// in flight is simply re-tested.
type Event struct {
	Seq     int64     `json:"seq"`
	Type    string    `json:"type"`
	At      time.Time `json:"at"`
	PR      PRID      `json:"pr,omitempty"`
	Version int       `json:"version,omitempty"`
	Head    string    `json:"head,omitempty"`
	Targets []string  `json:"targets,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Tests   []string  `json:"tests,omitempty"`
	Commit  string    `json:"commit,omitempty"`
	Parent  string    `json:"parent,omitempty"`
	Items   []Item    `json:"items,omitempty"`

	PRState    *PR         `json:"pr_state,omitempty"`    // EvSnapshotPR
	FlakyState *flaky.Stat `json:"flaky_state,omitempty"` // EvSnapshotFlaky
}

// Store is the engine's write-ahead log. Append must be durable before it
// returns.
type Store interface {
	Append(ev Event) error
	Load() ([]Event, error)
}

// Compacter is implemented by stores that can atomically replace their
// whole log with a snapshot.
type Compacter interface {
	Compact(snapshot []Event) error
}

// ErrCrashed is returned by CrashStore after its crash point.
var ErrCrashed = errors.New("queue: simulated crash")

// MemStore is an in-memory Store. If CrashAfter > 0 the store accepts that
// many more appends and then fails every call with ErrCrashed, which tests
// use to cut the engine off at an arbitrary point.
type MemStore struct {
	mu         sync.Mutex
	events     []Event
	CrashAfter int
	crashed    bool
}

// Append implements Store.
func (m *MemStore) Append(ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.crashed {
		return ErrCrashed
	}
	if m.CrashAfter > 0 {
		m.CrashAfter--
		if m.CrashAfter == 0 {
			m.crashed = true
			return ErrCrashed
		}
	}
	ev.Seq = int64(len(m.events) + 1)
	m.events = append(m.events, ev)
	return nil
}

// Compact implements Compacter.
func (m *MemStore) Compact(snap []Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.crashed {
		return ErrCrashed
	}
	m.events = nil
	for i, ev := range snap {
		ev.Seq = int64(i + 1)
		m.events = append(m.events, ev)
	}
	return nil
}

// Len returns the number of stored events.
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// Load implements Store.
func (m *MemStore) Load() ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...), nil
}

// Restart clears the crashed state (the process "restarted"), keeping the
// durable events.
func (m *MemStore) Restart() {
	m.mu.Lock()
	m.crashed, m.CrashAfter = false, 0
	m.mu.Unlock()
}

// Crashed reports whether the crash point was hit.
func (m *MemStore) Crashed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.crashed
}
