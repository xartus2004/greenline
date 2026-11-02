package sim

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/xartus2004/greenline/queue"
)

// Row aggregates one (strategy, scenario) cell over several seeds.
type Row struct {
	Strategy      string
	FailProb      float64
	Rate          float64
	Cache         bool
	Seeds         int
	Merged        int
	Throughput    float64 // merged/hour, mean over seeds
	P50, P95      time.Duration
	CIPerMerged   float64
	WastedPct     float64 // cancelled CI minutes / all CI minutes
	CacheHit      float64
	EjectedGood   int
	Breakages     int
	Violations    int
}

// Grid runs every strategy over the given failure rates and arrival rates.
func Grid(base Params, strategies []string, fails, rates []float64, caches []bool, seeds int) ([]Row, error) {
	var rows []Row
	for _, rate := range rates {
		for _, p := range fails {
			for _, c := range caches {
				for _, name := range strategies {
					var cfg queue.Config
					if name != "none" {
						var err error
						if cfg, err = queue.Strategy(name); err != nil {
							return nil, err
						}
					}
					row := Row{Strategy: name, FailProb: p, Rate: rate, Cache: c, Seeds: seeds}
					var ttm []time.Duration
					var ci, wasted, thr, hit float64
					for s := 0; s < seeds; s++ {
						prm := base
						prm.Seed = base.Seed + int64(s)
						prm.FailProb, prm.ArrivalPerHour, prm.Cache, prm.Queue = p, rate, c, cfg
						run := Run
						if name == "none" {
							run = RunNoQueue
						}
						res, err := run(prm)
						if err != nil {
							return nil, fmt.Errorf("%s p=%v rate=%v seed=%d: %w", name, p, rate, prm.Seed, err)
						}
						row.Merged += res.Merged
						row.EjectedGood += res.EjectedGood
						row.Breakages += res.MainBreakages
						row.Violations += len(res.Violations)
						ttm = append(ttm, res.TTM...)
						ci += res.CIMinutes
						wasted += res.WastedMinutes
						thr += res.Throughput
						hit += res.CacheHitRatio
					}
					sort.Slice(ttm, func(i, j int) bool { return ttm[i] < ttm[j] })
					if n := len(ttm); n > 0 {
						row.P50 = ttm[n/2]
						row.P95 = ttm[min(n-1, int(math.Ceil(float64(n)*0.95))-1)]
					}
					row.Throughput = thr / float64(seeds)
					row.CacheHit = hit / float64(seeds)
					if row.Merged > 0 {
						row.CIPerMerged = ci / float64(row.Merged)
					}
					if ci > 0 {
						row.WastedPct = 100 * wasted / ci
					}
					rows = append(rows, row)
				}
			}
		}
	}
	return rows, nil
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= 10*time.Hour:
		return fmt.Sprintf("%.0fh", d.Hours())
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
}

// Markdown renders rows grouped by scenario.
func Markdown(rows []Row) string {
	var b strings.Builder
	var last string
	for _, r := range rows {
		key := fmt.Sprintf("**p = %.2f, %.0f PRs/hour, cache %s**", r.FailProb, r.Rate, map[bool]string{true: "on", false: "off"}[r.Cache])
		if key != last {
			if last != "" {
				b.WriteString("\n")
			}
			b.WriteString(key + "\n\n")
			b.WriteString("| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |\n")
			b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
			last = key
		}
		fmt.Fprintf(&b, "| %s | %.1f | %s | %s | %.1f | %.0f%% | %.0f%% | %d | %d |\n",
			r.Strategy, r.Throughput, fmtDur(r.P50), fmtDur(r.P95), r.CIPerMerged, r.WastedPct, 100*r.CacheHit, r.EjectedGood, r.Breakages)
	}
	return b.String()
}
