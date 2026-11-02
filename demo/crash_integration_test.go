//go:build integration

package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xartus2004/greenline/actioncache"
	"github.com/xartus2004/greenline/cas"
	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
	"github.com/xartus2004/greenline/runner"
	"github.com/xartus2004/greenline/store"
)

type status struct {
	Main string     `json:"main"`
	PRs  []queue.PR `json:"prs"`
}

// TestKill9Recovery runs `greenline serve` as a subprocess, SIGKILLs it at
// random points while the queue is busy, restarts it, and finally checks
// that every commit on main is green when rebuilt from scratch, that main
// is a linear chain of queue merges each preceded by a logged merge intent,
// and that every PR ended in a terminal state consistent with main.
func TestKill9Recovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin := filepath.Join(dir, "greenline")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/greenline").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo, err := localgit.Init(ctx, filepath.Join(dir, "repo.git"), "../examples/mono")
	if err != nil {
		t.Fatal(err)
	}
	work := &localgit.Work{Dir: filepath.Join(dir, "work"), Remote: repo.Dir}
	if err := work.Clone(ctx); err != nil {
		t.Fatal(err)
	}
	prs := Scenario()
	for _, p := range prs {
		if err := Push(ctx, work, p, p.Files, false); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(dir, "state")
	seed := time.Now().UnixNano()
	if s := os.Getenv("GREENLINE_CRASH_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	rng := rand.New(rand.NewSource(seed))
	t.Logf("crash seed %d (GREENLINE_CRASH_SEED to reproduce)", seed)

	start := func() (*exec.Cmd, string) {
		os.Remove(filepath.Join(state, "addr"))
		cmd := exec.Command(bin, "serve", "-addr", "127.0.0.1:0", "-state", state, "-repo", repo.Dir, "-strategy", "lanes+speculative")
		logf, _ := os.OpenFile(filepath.Join(dir, "serve.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		cmd.Stdout, cmd.Stderr = logf, logf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 200; i++ {
			if b, err := os.ReadFile(filepath.Join(state, "addr")); err == nil && len(b) > 0 {
				return cmd, "http://" + string(b)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("server did not start")
		return nil, ""
	}
	get := func(base string) (*status, error) {
		resp, err := http.Get(base + "/api/status")
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var st status
		return &st, json.NewDecoder(resp.Body).Decode(&st)
	}
	submitMissing := func(base string) {
		st, err := get(base)
		if err != nil {
			return
		}
		known := map[queue.PRID]bool{}
		for _, p := range st.PRs {
			known[p.ID] = true
		}
		for _, p := range prs {
			if known[queue.PRID(p.ID)] {
				continue
			}
			body, _ := json.Marshal(map[string]any{"id": p.ID, "branch": fmt.Sprintf("pr/%d", p.ID)})
			resp, err := http.Post(base+"/api/prs", "application/json", bytes.NewReader(body))
			if err != nil {
				return // killed mid-submit; retried after restart
			}
			resp.Body.Close()
		}
	}

	kills := 0
	deadline := time.Now().Add(8 * time.Minute)
	for {
		cmd, base := start()
		go submitMissing(base)
		done := false
		limit := time.Now().Add(time.Duration(150+rng.Intn(1000)) * time.Millisecond)
		if kills >= 15 {
			limit = deadline // enough crashes: let it finish
		}
		for time.Now().Before(limit) {
			st, err := get(base)
			if err == nil && len(st.PRs) == len(prs) {
				done = true
				for _, p := range st.PRs {
					if !p.State.Terminal() {
						done = false
					}
				}
			}
			if done {
				break
			}
			if !done && len(st.PRs) < len(prs) {
				submitMissing(base)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if done {
			cmd.Process.Signal(syscall.SIGTERM)
			cmd.Wait()
			break
		}
		cmd.Process.Kill() // SIGKILL
		cmd.Wait()
		kills++
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(dir, "serve.log"))
			t.Fatalf("did not finish after %d kills\n%s", kills, tail(string(log), 4000))
		}
	}
	t.Logf("queue finished after %d SIGKILLs", kills)
	if kills == 0 {
		t.Fatal("never killed the server; test proved nothing")
	}

	// Final state from the durable log (a fresh, uncrashed process).
	db, err := store.Open(filepath.Join(state, "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := db.Load()
	db.Close()
	intents := map[string]bool{}
	for _, ev := range evs {
		if ev.Type == queue.EvMergeIntent {
			intents[ev.Commit] = true
		}
	}
	hist, err := repo.MainHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inMain := map[int]int{}
	for i, c := range hist[1:] {
		if !intents[c] {
			t.Errorf("main commit %s has no merge_intent in the log", c)
		}
		parents, _ := repo.Git(ctx, nil, "rev-list", "--parents", "-n1", c)
		if f := strings.Fields(parents); len(f) != 2 || f[1] != hist[i] {
			t.Errorf("main commit %s is not a single-parent child of %s", c, hist[i])
		}
		msg, _ := repo.Git(ctx, nil, "log", "-1", "--format=%B", c)
		for _, line := range strings.Split(msg, "\n") {
			var id, v int
			if n, _ := fmt.Sscanf(line, "PR #%d v%d", &id, &v); n == 2 {
				inMain[id]++
			}
		}
	}
	// Replay the log into a fresh engine view of PR states.
	final := map[int]queue.State{}
	for _, ev := range evs {
		switch ev.Type {
		case queue.EvEnqueue, queue.EvUpdate:
			final[int(ev.PR)] = queue.Queued
		case queue.EvEject:
			final[int(ev.PR)] = queue.Ejected
			t.Logf("PR %d ejected: %s %v", ev.PR, ev.Reason, ev.Tests)
		case queue.EvRemove:
			final[int(ev.PR)] = queue.Cancelled
		case queue.EvMerged:
			for _, it := range ev.Items {
				final[int(it.PR)] = queue.Merged
			}
		}
	}
	for _, p := range prs {
		s := final[p.ID]
		if s == queue.Merged && inMain[p.ID] != 1 {
			t.Errorf("PR %d merged but appears %d times on main", p.ID, inMain[p.ID])
		}
		if s != queue.Merged && inMain[p.ID] != 0 {
			t.Errorf("PR %d is %s but is on main", p.ID, s)
		}
		if !s.Terminal() {
			t.Errorf("PR %d ended %s", p.ID, s)
		}
		if p.Kind == "broken" && s == queue.Merged {
			t.Errorf("broken PR %d merged", p.ID)
		}
	}
	// Rebuild every main commit from scratch.
	r := &runner.Runner{Repo: repo, CAS: cas.NewMemory(), AC: actioncache.NewMemory(), Jobs: 4}
	checked, red, err := r.VerifyMain(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(red) > 0 {
		t.Fatalf("red main commits: %v", red)
	}
	t.Logf("main green at all %d commits; %d PRs merged", checked, len(hist)-1)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
