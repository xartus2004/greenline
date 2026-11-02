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
