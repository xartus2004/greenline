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
	"strings"
	"syscall"
	"time"

	"github.com/xartus2004/greenline/forge/github"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
	"github.com/xartus2004/greenline/server"
	"google.golang.org/grpc"
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
	repo := fs.String("repo", "", "bare git repository acting as the forge (or, with -github, the local mirror)")
	gh := fs.String("github", "", "owner/repo: use GitHub as the forge (token from GITHUB_TOKEN, webhook secret from GREENLINE_WEBHOOK_SECRET)")
	ghAPI := fs.String("github-api", "https://api.github.com", "GitHub API base URL")
	ghRemote := fs.String("github-remote", "", "git URL of the GitHub repo (default https://github.com/<owner>/<repo>.git)")
	label := fs.String("label", "merge-queue", "PR label that puts a PR in the queue (GitHub mode)")
	initFrom := fs.String("init-from", "", "create -repo from this directory if it does not exist")
	jobs := fs.Int("j", 4, "parallel build actions per run")
	grpcAddr := fs.String("grpc", "", "also serve the cache over REAPI v2 gRPC on this address")
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
	os.MkdirAll(*state, 0o755)
	scfg := server.Config{StateDir: *state, Queue: cfg, Jobs: *jobs, Log: log}
	var api *github.Client
	if *gh != "" {
		owner, name, ok := strings.Cut(*gh, "/")
		if !ok {
			return errors.New("-github wants owner/repo")
		}
		tok := os.Getenv("GITHUB_TOKEN")
		if tok == "" {
			return errors.New("GITHUB_TOKEN is not set")
		}
		secret := os.Getenv("GREENLINE_WEBHOOK_SECRET")
		if secret == "" {
			return errors.New("GREENLINE_WEBHOOK_SECRET is not set")
		}
		api = github.New(*ghAPI, owner, name, tok)
		remote := *ghRemote
		if remote == "" {
			remote = "https://github.com/" + owner + "/" + name + ".git"
		}
		m, err := github.OpenMirror(ctx, api, remote, *repo)
		if err != nil {
			return err
		}
		scfg.Repo, scfg.Forge, scfg.Fetcher = m.Mirror, m, m
		scfg.WebhookSecret, scfg.WebhookLabel = []byte(secret), *label
		scfg.PollMain = 30 * time.Second // stay well inside API rate limits
	} else {
		r, err := localgit.Init(ctx, *repo, *initFrom)
		if err != nil {
			return err
		}
		scfg.Repo = r
	}
	scfg.OnTransition = func(t server.Transition) {
		log.Info("pr", "id", t.PR.ID, "from", t.From, "to", t.PR.State, "reason", t.PR.Reason, "commit", t.PR.Commit)
		if api != nil {
			p := t.PR
			go func() {
				if err := api.ReportPR(context.Background(), p); err != nil {
					log.Warn("check run", "pr", p.ID, "err", err)
				}
			}()
		}
	}
	srv, err := server.New(scfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	// Tell scripts (and the crash test) where we listen.
	os.WriteFile(filepath.Join(*state, "addr"), []byte(ln.Addr().String()), 0o644)
	if *grpcAddr != "" {
		gl, err := net.Listen("tcp", *grpcAddr)
		if err != nil {
			return err
		}
		gs := grpc.NewServer(grpc.MaxRecvMsgSize(16 << 20))
		srv.RegisterREAPI(gs)
		go gs.Serve(gl)
		defer gs.Stop()
		log.Info("REAPI gRPC cache", "addr", gl.Addr().String())
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	log.Info("serving", "addr", ln.Addr().String(), "repo", *repo, "state", *state)
	err = srv.Run(ctx)
	sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	hs.Shutdown(sctx)
	return err
}
