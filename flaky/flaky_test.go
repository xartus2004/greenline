package flaky

import "testing"

func TestQuarantine(t *testing.T) {
	tr := New(2)
	if tr.RecordFlake("t1") {
		t.Fatal("quarantined after 1 flake")
	}
	if !tr.RecordFlake("t1") || !tr.Quarantined("t1") {
		t.Fatal("not quarantined after 2 flakes")
	}
	g, ig := tr.Gating([]string{"t1", "t2"})
	if len(g) != 1 || g[0] != "t2" || len(ig) != 1 {
		t.Fatalf("gating %v ignored %v", g, ig)
	}
	if st := tr.Stats(); st[0].Ignored != 1 {
		t.Fatalf("%+v", st)
	}
	off := New(-1)
	for i := 0; i < 10; i++ {
		off.RecordFlake("x")
	}
	if off.Quarantined("x") {
		t.Fatal("quarantine should be disabled")
	}
}
