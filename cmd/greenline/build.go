package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"time"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/build"
	"github.com/xartus2004/greenline/cas"
	"github.com/xartus2004/greenline/httpcache"
)

func init() {
	register("build", cmdBuild)
	register("cache", cmdCache)
}

// openCache returns a CAS + action cache from either a remote URL or a
// local directory.
func openCache(remote, dir string, maxBytes int64) (cas.Store, actioncache.Raw, error) {
	if remote != "" {
		c := httpcache.NewClient(remote)
		return c, c, nil
	}
	st, err := cas.OpenDisk(filepath.Join(dir, "cas"), maxBytes)
	if err != nil {
		return nil, nil, err
	}
	ac, err := actioncache.OpenDisk(filepath.Join(dir, "ac"))
	return st, ac, err
}

func defaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "greenline")
	}
	return ".greenline-cache"
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	root := fs.String("root", ".", "workspace root")
	remote := fs.String("remote", "", "remote HTTP cache URL (e.g. http://localhost:9092)")
	dir := fs.String("cache", defaultCacheDir(), "local cache dir (ignored with -remote)")
	jobs := fs.Int("j", 4, "parallel actions")
	noCache := fs.Bool("no-cache", false, "disable cache lookups")
	out := fs.String("out", "", "materialize outputs into this dir")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)
	st, raw, err := openCache(*remote, *dir, 10<<30)
	if err != nil {
		return err
	}
	ws, err := build.Load(*root)
	if err != nil {
		return err
	}
	labels := fs.Args()
	if len(labels) == 0 {
		labels = ws.Labels()
	}
	b := &build.Builder{WS: ws, CAS: st, Cache: actioncache.Typed{Raw: raw, CAS: st}, Jobs: *jobs, NoCache: *noCache, KeepGoing: true, Log: logger(*verbose)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep, err := b.Build(ctx, labels)
	if rep != nil {
		printReport(rep)
	}
	if err != nil {
		return err
	}
	if *out != "" {
		if err := b.Materialize(ctx, rep, rep.Order, *out); err != nil {
			return err
		}
	}
	if !rep.OK() {
		return errors.New("build failed")
	}
	return nil
}

func printReport(rep *build.Report) {
	labels := append([]string(nil), rep.Order...)
	sort.Strings(labels)
	for _, l := range labels {
		r := rep.Results[l]
		status := "ran   "
		switch {
		case r.Skipped:
			status = "skip  "
		case r.Cached:
			status = "cached"
		case !r.OK():
			status = "FAIL  "
		}
		fmt.Printf("%s %-36s %6dms\n", status, l, r.Duration.Milliseconds())
		if !r.OK() && !r.Skipped {
			fmt.Printf("  exit=%d %s\n%s%s", r.ExitCode, r.Err, r.Stdout, r.Stderr)
		}
	}
	hits, ran := rep.Hits()
	fmt.Printf("%d actions: %d cached, %d executed, %d failed in %v\n", len(rep.Results), hits, ran, len(rep.Failed()), rep.Duration.Round(time.Millisecond))
}

func cmdCache(args []string) error {
	fs := flag.NewFlagSet("cache", flag.ExitOnError)
	addr := fs.String("addr", ":9092", "listen address")
	dir := fs.String("dir", defaultCacheDir(), "cache directory")
	maxBytes := fs.Int64("max-bytes", 10<<30, "CAS capacity in bytes")
	fs.Parse(args)
	st, raw, err := openCache("", *dir, *maxBytes)
	if err != nil {
		return err
	}
	log := logger(false)
	log.Info("cache listening", "addr", *addr, "dir", *dir)
	return http.ListenAndServe(*addr, &httpcache.Server{CAS: st, AC: raw, Log: log})
}
