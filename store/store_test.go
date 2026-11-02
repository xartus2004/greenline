package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xartus2004/greenline/queue"
)

func TestAppendLoadReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(100, 5).UTC()
	evs := []queue.Event{
		{Type: queue.EvEnqueue, At: at, PR: 1, Version: 1, Head: "h", Targets: []string{"//a:a"}},
		{Type: queue.EvMergeIntent, At: at, Commit: "c", Parent: "p", Items: []queue.Item{{PR: 1, Version: 1, Head: "h"}}},
	}
	for _, ev := range evs {
		if err := s.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Load()
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Seq != 1 || got[1].Items[0].Head != "h" || !got[0].At.Equal(at) || got[0].Targets[0] != "//a:a" {
		t.Fatalf("round trip: %+v", got)
	}
}

var _ queue.Store = (*SQLite)(nil)

func TestCompact(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 10; i++ {
		s.Append(queue.Event{Type: queue.EvEnqueue, PR: queue.PRID(i)})
	}
	snap := []queue.Event{{Type: queue.EvSnapshotPR, PR: 3, PRState: &queue.PR{ID: 3, State: queue.Merged, Seq: 9}}}
	if err := s.Compact(snap); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Load()
	if len(got) != 1 || got[0].PRState == nil || got[0].PRState.Seq != 9 {
		t.Fatalf("%+v", got)
	}
}
