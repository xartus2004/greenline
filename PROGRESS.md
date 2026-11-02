# PROGRESS

Resume with: "read PROGRESS.md and continue".

## Step 0 — environment (2026-11-02, cloud dev container)

| Probe | Result |
|---|---|
| `go version` | go1.24.7 installed; toolchain auto-switch available (go1.26.0 downloaded) |
| `git --version` | 2.43.0 |
| `bazel` / `bazelisk` | **not installed**; releases.bazel.build and github.com release downloads are blocked by the network policy (HTTP 403), so real-Bazel tests cannot run here. They are gated behind the `bazel` build tag. |
| CPUs / RAM | 4 vCPU, 15 GiB |
| Network | outbound HTTPS through a proxy; proxy.golang.org works (Go modules OK); most other hosts blocked |
| staticcheck | installed via `go install honnef.co/go/tools/cmd/staticcheck@latest` |

## Milestones

- [x] **M1** — cas (disk LRU, atomic writes, integrity check, fuzzed digest parsing), actioncache, httpcache (Bazel HTTP protocol server + client), build tool (BUILD.yaml, sandboxed actions, process-group kill, timeouts), affected-target analysis, `examples/mono` (15 targets), `greenline build` / `greenline cache`.
- [x] **M2** — queue engine (one mechanism covers serial, batch+bisect, Zuul-style speculation with culprit skipping, lanes), write-ahead event log (`queue.MemStore`, `store.SQLite`), crash recovery that reconciles in-doubt merges with the forge, flaky retry + quarantine, infra retry with backoff, timeouts, update/remove/external push. Table-driven lifecycle tests in `queue/engine_test.go`; property test in `sim/sim_test.go` (7 configs × 1,000 seeds, `-long` = 20,000) with random crash points.
- [ ] **M3** — speculation, lanes, simulator, README results table.
- [ ] **M4** — localgit forge, runner, `greenline demo`, kill -9 integration test.
- [ ] **M5** — REAPI gRPC, GitHub forge (fakes), status page, Dockerfile, docs.

## Measured so far (this container, 4 vCPU)

- `examples/mono` cold build: 15 actions executed in ~1.06 s; warm: 15/15 cached in 1 ms (local disk) / 7 ms (over HTTP to `greenline cache` on localhost). Action cost is synthetic (`sleep 0.1–0.3` in each command stands in for compile time).

## Design notes

- Safety rests on two checks only: a group merges only if its run's parent commit equals the current main, and `Forge.Advance` is a CAS fast-forward by exactly one commit. The run-invalidation rules are for efficiency: a mutation test that removed one of them still produced zero violations (the parent check caught the stale passes). A mutation making flaky retries always pass was caught on the first seed.
- Lanes are re-formed after each merge/ejection from the PRs that are not yet committed to a run. Without this, a lane's target set is the union of its whole backlog and under load everything collapses into one lane (measured: avg 1.2 lanes, 45 active PRs → after the fix 3.1 lanes, 8.7 active).
- Runs in other lanes are not cancelled when main moves; they finish (filling the cache) and the group is re-verified on the new main, which is mostly cache hits.

## Known gaps

- Bazel compatibility is implemented against the documented HTTP protocol but **not verified with a real Bazel** here (no Bazel binary available).
