package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xartus2004/greenline/demo"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
	"github.com/xartus2004/greenline/server"
)

func init() { register("demo", cmdDemo) }

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dir := fs.String("dir", "", "working directory (default: a new temp dir)")
	src := fs.String("src", "examples/mono", "example workspace")
	jobs := fs.Int("j", 4, "parallel build actions per run")
	verbose := fs.Bool("v", false, "debug logging")
	strategy := strategyFlag(fs, "lanes+speculative")
	fs.Parse(args)
	cfg, err := strategy()
	if err != nil {
		return err
	}
	if *dir == "" {
		if *dir, err = os.MkdirTemp("", "greenline-demo-*"); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	start := time.Now()
	fmt.Printf("greenline demo in %s (strategy depth=%d batch=%d lanes=%v)\n\n", *dir, cfg.Depth, cfg.BatchSize, cfg.Lanes)

	repo, err := localgit.Init(ctx, filepath.Join(*dir, "repo.git"), *src)
	if err != nil {
		return err
	}
	work := &localgit.Work{Dir: filepath.Join(*dir, "work"), Remote: repo.Dir}
	if err := work.Clone(ctx); err != nil {
		return err
	}
	prs := demo.Scenario()
	byID := map[queue.PRID]demo.PR{}
	for _, p := range prs {
		byID[queue.PRID(p.ID)] = p
		if err := demo.Push(ctx, work, p, p.Files, false); err != nil {
			return err
		}
	}
	var mu sync.Mutex
	srv, err := server.New(server.Config{StateDir: filepath.Join(*dir, "state"), Repo: repo, Queue: cfg, Jobs: *jobs, Log: logger(*verbose),
		OnTransition: func(t server.Transition) {
			mu.Lock()
			defer mu.Unlock()
			p := t.PR
			line := fmt.Sprintf("%7.1fs  #%-2d %-9s", t.At.Sub(start).Seconds(), p.ID, p.State)
			if t.From == "" {
				fmt.Printf("%7.1fs  #%-2d enqueued  v%d %s\n", t.At.Sub(start).Seconds(), p.ID, p.Version, byID[p.ID].Title)
				if !p.State.Terminal() {
					return
				}
			}
			switch p.State {
			case queue.Merged:
				line += " in " + short(p.Commit)
			case queue.Ejected:
				line += " " + p.Reason + " " + strings.Join(p.FailedTest, " ")
			case queue.Queued, queue.Testing:
				return // too chatty
			}
			fmt.Println(line)
		}})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(runCtx) }()

	var wg sync.WaitGroup
	for _, p := range prs {
		if err := srv.Enqueue(ctx, queue.PRID(p.ID), fmt.Sprintf("pr/%d", p.ID)); err != nil {
			return err
		}
		if p.Fix != nil {
			p := p
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Second)
				mu.Lock()
				err := demo.Push(ctx, work, p, p.Fix, true)
				mu.Unlock()
				if err == nil {
					err = srv.Update(ctx, queue.PRID(p.ID), fmt.Sprintf("pr/%d", p.ID))
				}
				if err != nil {
					fmt.Printf("         #%-2d fix not applied: %v\n", p.ID, err)
				} else {
					mu.Lock()
					fmt.Printf("%7.1fs  #%-2d pushed a fix while queued\n", time.Since(start).Seconds(), p.ID)
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	deadline := time.After(15 * time.Minute)
	for {
		st := srv.Status()
		active := 0
		for _, p := range st.PRs {
			if !p.State.Terminal() {
				active++
			}
		}
		if active == 0 && len(st.PRs) == len(prs) {
			break
		}
		select {
		case err := <-errc:
			return fmt.Errorf("server stopped: %v", err)
		case <-deadline:
			return fmt.Errorf("timed out with %d PRs active", active)
		case <-time.After(200 * time.Millisecond):
		}
	}
	elapsed := time.Since(start)
	st := srv.Status()
	cancel()
	<-errc

	fmt.Printf("\n%-4s %-9s %-52s %s\n", "PR", "state", "title", "detail")
	ps := append([]queue.PR(nil), st.PRs...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	merged := 0
	for _, p := range ps {
		d := byID[p.ID]
		detail := fmt.Sprintf("[%s] runs=%d", d.Kind, p.Runs)
		if p.State == queue.Ejected {
			detail += " " + p.Reason + " " + strings.Join(p.FailedTest, " ")
		} else {
			merged++
		}
		fmt.Printf("#%-3d %-9s %-52s %s\n", p.ID, p.State, d.Title, detail)
	}
	s := st.Stats
	fmt.Printf("\n%d/%d merged in %d commits in %.1fs; %d runs started, %d cancelled, %d stale results discarded, %d flaky retries, %d flakes recorded, %d bisections\n",
		merged, len(ps), s.Merges, elapsed.Seconds(), s.RunsStarted, s.RunsCancelled, s.StaleResults, s.Retries, s.Flakes, s.Bisections)
	ran, cached := srv.ActionCounts()
	fmt.Printf("build actions: %d executed, %d served from cache (%.0f%% hit); CAS holds %d blobs, %d bytes\n",
		ran, cached, 100*float64(cached)/float64(max(1, ran+cached)), st.Cache.Items, st.Cache.Bytes)

	fmt.Println("\nverifying: rebuilding every commit on main from scratch (no cache)...")
	checked, red, err := srv.Runner().VerifyMain(ctx, 6)
	if err != nil {
		return err
	}
	if len(red) > 0 {
		for c, f := range red {
			fmt.Printf("  RED %s: %v\n", short(c), f)
		}
		return fmt.Errorf("%d of %d main commits are red", len(red), checked)
	}
	fmt.Printf("main is green at all %d commits (flaky targets retried up to 6 times)\n", checked)
	return nil
}

func short(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return c
}
