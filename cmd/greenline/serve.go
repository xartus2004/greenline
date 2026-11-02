package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
	"github.com/xartus2004/greenline/server"
)

func init() { register("serve", cmdServe) }

func strategyFlag(fs *flag.FlagSet, def string) func() (queue.Config, error) {
	name := fs.String("strategy", def, "serial | batch | speculative | lanes | lanes+speculative")
	depth := fs.Int("depth", 0, "override speculation depth")
	batch := fs.Int("batch", 0, "override batch size")
	return func() (queue.Config, error) {
		cfg, err := queue.Strategy(*name)
		if *depth > 0 {
			cfg.Depth = *depth
		}
		if *batch > 0 {
			cfg.BatchSize = *batch
		}
		cfg.RunTimeout = 10 * time.Minute
		return cfg, err
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address (API, status page, /metrics, /cache/)")
	state := fs.String("state", "greenline-state", "state directory (queue.db, cache)")
	repo := fs.String("repo", "", "bare git repository acting as the forge (required)")
	initFrom := fs.String("init-from", "", "create -repo from this directory if it does not exist")
	jobs := fs.Int("j", 4, "parallel build actions per run")
	verbose := fs.Bool("v", false, "debug logging")
	strategy := strategyFlag(fs, "lanes+speculative")
	fs.Parse(args)
	if *repo == "" {
		return errors.New("-repo is required")
	}
	cfg, err := strategy()
	if err != nil {
		return err
	}
	log := logger(*verbose)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := localgit.Init(ctx, *repo, *initFrom)
	if err != nil {
		return err
	}
	os.MkdirAll(*state, 0o755)
	srv, err := server.New(server.Config{StateDir: *state, Repo: r, Queue: cfg, Jobs: *jobs, Log: log,
		OnTransition: func(t server.Transition) {
			log.Info("pr", "id", t.PR.ID, "from", t.From, "to", t.PR.State, "reason", t.PR.Reason, "commit", t.PR.Commit)
		}})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	// Tell scripts (and the crash test) where we listen.
	os.WriteFile(filepath.Join(*state, "addr"), []byte(ln.Addr().String()), 0o644)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	log.Info("serving", "addr", ln.Addr().String(), "repo", *repo, "state", *state)
	err = srv.Run(ctx)
	sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	hs.Shutdown(sctx)
	return err
}
