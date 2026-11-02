// Package metrics exposes greenline's Prometheus metrics. Values are read
// from a snapshot function at scrape time, so the single-threaded engine is
// never touched from the HTTP goroutine.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Snapshot is the state exported on each scrape.
type Snapshot struct {
	QueueDepth     int
	RunningRuns    int
	Lanes          int
	MergedPRs      int
	Merges         int
	Ejected        map[string]int
	RunsStarted    int
	RunsCancelled  int
	Flakes         int
	CacheHits      int64
	CacheMisses    int64
	CASBytes       int64
	CASItems       int64
	CASEvictions   int64
	CASCorrupt     int64
	ActionsRun     int64
	ActionsCached  int64
	TimeToMergeSec []float64 // observed since last snapshot
}

// Collector adapts a snapshot function to Prometheus.
type Collector struct {
	snap func() Snapshot
	ttm  prometheus.Histogram
	desc map[string]*prometheus.Desc
}

// New returns a collector; register it with a prometheus.Registry.
func New(snap func() Snapshot) *Collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("greenline_"+name, help, labels, nil)
	}
	return &Collector{
		snap: snap,
		ttm: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "greenline_time_to_merge_seconds", Help: "Enqueue to merge latency.",
			Buckets: prometheus.ExponentialBuckets(10, 2, 14),
		}),
		desc: map[string]*prometheus.Desc{
			"depth":     d("queue_depth", "Active (non-terminal) PRs."),
			"running":   d("ci_runs_running", "CI runs in flight."),
			"lanes":     d("lanes", "Active lanes."),
			"merged":    d("merged_prs_total", "PRs merged."),
			"merges":    d("merge_commits_total", "Commits pushed to main by the queue."),
			"ejected":   d("ejected_prs_total", "PRs ejected, by reason.", "reason"),
			"started":   d("ci_runs_started_total", "CI runs started."),
			"cancelled": d("ci_runs_cancelled_total", "CI runs cancelled (speculation invalidated)."),
			"flakes":    d("flakes_total", "Flaky test detections."),
			"hits":      d("cache_hits_total", "CAS hits."),
			"misses":    d("cache_misses_total", "CAS misses."),
			"bytes":     d("cas_bytes", "Bytes stored in the CAS."),
			"items":     d("cas_items", "Blobs stored in the CAS."),
			"evictions": d("cas_evictions_total", "Blobs evicted."),
			"corrupt":   d("cas_corrupt_total", "Blobs that failed integrity checks."),
			"actions":   d("build_actions_total", "Build actions by result.", "result"),
		},
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.desc {
		ch <- d
	}
	c.ttm.Describe(ch)
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	s := c.snap()
	g := func(k string, v float64, l ...string) {
		ch <- prometheus.MustNewConstMetric(c.desc[k], prometheus.GaugeValue, v, l...)
	}
	cnt := func(k string, v float64, l ...string) {
		ch <- prometheus.MustNewConstMetric(c.desc[k], prometheus.CounterValue, v, l...)
	}
	g("depth", float64(s.QueueDepth))
	g("running", float64(s.RunningRuns))
	g("lanes", float64(s.Lanes))
	cnt("merged", float64(s.MergedPRs))
	cnt("merges", float64(s.Merges))
	for r, n := range s.Ejected {
		cnt("ejected", float64(n), r)
	}
	cnt("started", float64(s.RunsStarted))
	cnt("cancelled", float64(s.RunsCancelled))
	cnt("flakes", float64(s.Flakes))
	cnt("hits", float64(s.CacheHits))
	cnt("misses", float64(s.CacheMisses))
	g("bytes", float64(s.CASBytes))
	g("items", float64(s.CASItems))
	cnt("evictions", float64(s.CASEvictions))
	cnt("corrupt", float64(s.CASCorrupt))
	cnt("actions", float64(s.ActionsRun), "executed")
	cnt("actions", float64(s.ActionsCached), "cached")
	for _, v := range s.TimeToMergeSec {
		c.ttm.Observe(v)
	}
	c.ttm.Collect(ch)
}
