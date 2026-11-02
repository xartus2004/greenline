# PROGRESS

Resume with: "read PROGRESS.md and continue".

## Step 0 — environment (2026-11-02, cloud dev container)

| Probe | Result |
|---|---|
| `go version` | go1.24.7 installed; go.mod now needs 1.26, which the toolchain auto-switch downloads (go1.26.0) |
| `git --version` | 2.43.0 |
| `bazel` / `bazelisk` | **not installed**; releases.bazel.build and github.com release downloads are blocked by the network policy (HTTP 403), so real-Bazel tests cannot run here. They are gated behind the `bazel` build tag. |
| CPUs / RAM | 4 vCPU, 15 GiB |
| Network | outbound HTTPS through a proxy; proxy.golang.org works (Go modules OK); most other hosts blocked |
| staticcheck | installed via `go install honnef.co/go/tools/cmd/staticcheck@latest` |

## Milestones

- [x] **M1** — cas (disk LRU, atomic writes, integrity check, fuzzed digest parsing), actioncache, httpcache (Bazel HTTP protocol server + client), build tool (BUILD.yaml, sandboxed actions, process-group kill, timeouts), affected-target analysis, `examples/mono` (15 targets), `greenline build` / `greenline cache`.
- [x] **M2** — queue engine (one mechanism covers serial, batch+bisect, Zuul-style speculation with culprit skipping, lanes), write-ahead event log (`queue.MemStore`, `store.SQLite`), crash recovery that reconciles in-doubt merges with the forge, flaky retry + quarantine, infra retry with backoff, timeouts, update/remove/external push. Table-driven lifecycle tests in `queue/engine_test.go`; property test in `sim/sim_test.go` (7 configs × 1,000 seeds, `-long` = 20,000) with random crash points.
- [x] **M3** — speculation + lanes (built in M2's engine), `greenline sim` grid with a no-queue baseline and cache on/off, README results table, docs/results.md (full grid). `go test ./sim -long` (7×20,000 seeds) passes in 46 s.
- [x] **M4** — `forge/localgit` (deterministic merge commits via merge-tree/commit-tree, CAS `update-ref`), `runner` (real CI through the cache, process-group cancel, `VerifyMain`), `server` (engine goroutine, JSON API, HTML status page, Prometheus, HTTP cache), `greenline serve` and `greenline demo` (20 PRs; main verified green from scratch), kill -9 integration test (`-tags integration`, passes with 9–11 SIGKILLs per run).
- [x] **M5** — `reapi` gRPC cache (tested over bufconn **and with Google's official `remote-apis-sdks` Go client**), `forge/github` (REST forge, check runs, HMAC webhooks, fuzzed signature check) **wired into `serve -github`** through a local mirror (end-to-end test against a fake GitHub API backed by a real bare repo), HTML status page, Prometheus `/metrics`, `docs/architecture.md`, Dockerfile (**built here**; the full demo passes inside the image).
- [x] **After M5** — event-log compaction (`Engine.Compact`, SQLite transaction; the server compacts once the log passes 5,000 events; property test compacts at random points).
- [ ] **Only remaining item:** `reapi/bazel_test.go` (`-tags bazel`) against a real Bazel binary. It is not run here because Bazel downloads are blocked.

## Measured so far (this container, 4 vCPU)

- `examples/mono` cold build: 15 actions executed in ~1.06 s; warm: 15/15 cached in 1 ms (local disk) / 7 ms (over HTTP to `greenline cache` on localhost). Action cost is synthetic (`sleep 0.1–0.3` in each command stands in for compile time).

## Design notes

- Safety rests on two checks only: a group merges only if its run's parent commit equals the current main, and `Forge.Advance` is a CAS fast-forward by exactly one commit. The run-invalidation rules are for efficiency: a mutation test that removed one of them still produced zero violations (the parent check caught the stale passes). A mutation making flaky retries always pass was caught on the first seed.
- Lanes are re-formed after each merge/ejection from the PRs that are not yet committed to a run. Without this, a lane's target set is the union of its whole backlog and under load everything collapses into one lane (measured: avg 1.2 lanes, 45 active PRs → after the fix 3.1 lanes, 8.7 active).
- Runs in other lanes are not cancelled when main moves; they finish (filling the cache) and the group is re-verified on the new main, which is mostly cache hits.

## Headline numbers (simulated; `go run ./cmd/greenline sim -p 0.10 -rate 12`)

p = 0.10, 12 PRs/h, cache on: serial p95 TTM 8.7h → speculative 36m, lanes+speculative 32m (≈16× lower); 0 main breakages for every queue strategy vs 2 for no queue. Speculative CI/merged PR: 9.8 min with cache vs 22.5 without (serial 6.7). Cache hit ratio of target actions under speculation: 47%.

## Next

1. Run `go test -tags bazel ./reapi -v` on a machine with Bazel, and fix whatever it finds.
2. Try `serve -github` on a real repository (a token with contents:write and checks:write, plus a webhook for pull_request and push).
3. Optional: in the simulator, model in-flight action dedup (remote execution) to quantify how much more speculation could save.

## Environment notes for reproducing here

- Go modules need Go 1.26 (raised by grpc / remote-apis-sdks); the toolchain auto-downloads.
- Docker: start `dockerd` manually. Base-image pulls work, but `apt-get` is blocked, so the runtime image uses `buildpack-deps:bookworm-scm`, which already has git. The Go proxy needs the session's TLS CA: `docker build --secret id=extra_ca,src=/root/.ccr/ca-bundle.crt -t greenline .`

## Known gaps

- Bazel compatibility (HTTP and gRPC) is implemented against the documented protocols but **not verified with a real Bazel** here (no Bazel binary available; npm/bazelisk route also not permitted).
- See README "Limitations".
