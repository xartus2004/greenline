package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/xartus2004/greenline/queue"
	"github.com/xartus2004/greenline/sim"
)

func init() { register("sim", cmdSim) }

func floats(s string) ([]float64, error) {
	var out []float64
	for _, f := range strings.Split(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func cmdSim(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ExitOnError)
	d := sim.Defaults()
	fails := fs.String("p", "0.02,0.10,0.25", "PR failure probabilities")
	rates := fs.String("rate", "6,12", "PR arrival rates per hour")
	strategies := fs.String("strategies", "none,"+strings.Join(queue.Strategies, ","), "strategies (none = no merge queue baseline)")
	cache := fs.String("cache", "on", "cache model: on, off or both")
	seeds := fs.Int("seeds", 5, "seeds per cell")
	seed := fs.Int64("seed", 1, "first seed")
	prs := fs.Int("prs", d.PRs, "PRs per run")
	crash := fs.Int("crash", 0, "inject crashes every ~N store appends (0 = off)")
	quick := fs.Bool("quick", false, "small fast run (CI smoke test)")
	fs.Parse(args)
	ps, err := floats(*fails)
	if err != nil {
		return err
	}
	rs, err := floats(*rates)
	if err != nil {
		return err
	}
	var caches []bool
	switch *cache {
	case "on":
		caches = []bool{true}
	case "off":
		caches = []bool{false}
	case "both":
		caches = []bool{true, false}
	default:
		return fmt.Errorf("-cache must be on, off or both")
	}
	d.Seed, d.PRs, d.CrashMean = *seed, *prs, *crash
	if *quick {
		d.PRs, *seeds, ps, rs = 60, 2, []float64{0.1}, []float64{12}
	}
	rows, err := sim.Grid(d, strings.Split(*strategies, ","), ps, rs, caches, *seeds)
	if err != nil {
		return err
	}
	fmt.Print(sim.Markdown(rows))
	for _, r := range rows {
		if r.Violations > 0 || (r.Strategy != "none" && r.Breakages > 0) {
			return errors.New("invariant violated: see table")
		}
	}
	return nil
}
