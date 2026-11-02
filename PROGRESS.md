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
- [ ] **M2** — queue engine (serial, batch+bisect), store with crash recovery, property test.
- [ ] **M3** — speculation, lanes, simulator, README results table.
- [ ] **M4** — localgit forge, runner, `greenline demo`, kill -9 integration test.
- [ ] **M5** — REAPI gRPC, GitHub forge (fakes), status page, Dockerfile, docs.

## Measured so far (this container, 4 vCPU)

- `examples/mono` cold build: 15 actions executed in ~1.06 s; warm: 15/15 cached in 1 ms (local disk) / 7 ms (over HTTP to `greenline cache` on localhost). Action cost is synthetic (`sleep 0.1–0.3` in each command stands in for compile time).

## Known gaps

- Bazel compatibility is implemented against the documented HTTP protocol but **not verified with a real Bazel** here (no Bazel binary available).
