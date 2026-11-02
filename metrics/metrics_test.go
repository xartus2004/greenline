package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCollector(t *testing.T) {
	c := New(func() Snapshot {
		return Snapshot{QueueDepth: 3, Ejected: map[string]int{"test_failed": 2}, TimeToMergeSec: []float64{30}}
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP greenline_queue_depth Active (non-terminal) PRs.
# TYPE greenline_queue_depth gauge
greenline_queue_depth 3
# HELP greenline_ejected_prs_total PRs ejected, by reason.
# TYPE greenline_ejected_prs_total counter
greenline_ejected_prs_total{reason="test_failed"} 2
`), "greenline_queue_depth", "greenline_ejected_prs_total"); err != nil {
		t.Fatal(err)
	}
}
