# greenline

A merge queue and a content-addressed build cache, in Go, built so that the
queue's speculative builds share their work through the cache.

Coding agents have multiplied the number of pull requests, and the slow part is
now landing them, not writing them. A merge queue keeps `main` green by testing
each change on top of everything ahead of it before merging. Done serially,
throughput is one merge per CI run. The known ways to go faster (batching,
Zuul-style speculation, independent lanes) all spend extra CI on builds that
overlap heavily. A shared action cache makes that overlap cheap. greenline puts
both in one system and measures the trade-offs in a deterministic simulator that
runs the real queue engine.

The cache speaks Bazel's HTTP remote-cache protocol (`/ac/<sha256>`,
`/cas/<sha256>`). greenline also ships a small hermetic build tool
(`BUILD.yaml`), so the whole thing works without Bazel.

> **Status:** M1–M5 are done and tested, with one exception: a run against a
> real Bazel binary. That test is written (`-tags bazel`) but has not been run,
> because this environment can't download Bazel. The REAPI gRPC cache *is*
> tested against Google's official REAPI Go client SDK (`remote-apis-sdks`).
> The Docker image builds, and the full demo passes inside it. GitHub mode
> (`serve -github`) is tested end to end against a fake GitHub API backed by a
> real git repository. See [PROGRESS.md](PROGRESS.md) and
> [docs/architecture.md](docs/architecture.md).

## The invariant

**Every commit the queue puts on `main` has passed CI on exactly that tree.**

Two checks enforce it, and nothing else is trusted:

1. A group of PRs merges only when it is at the head of its lane, its CI run
   passed, and **the run's parent commit equals the current `main`**.
2. `Forge.Advance(old, next)` is a compare-and-swap that only fast-forwards
   `main` by **exactly one commit** whose parent is `old`. A batch becomes one
   commit, so no untested intermediate commit can land.

Everything else (cancelling and invalidating runs when the queue changes) only
affects speed. A mutation test that deleted one of the invalidation rules still
produced zero violations, because check 1 caught the stale passes. A mutation
that made flaky retries always pass failed on the first seed.

**Property test** (`sim/sim_test.go`): random PR streams, random
independent failures (0–40%), pairwise conflicts (PRs that pass alone but fail
together), flaky tests, infra errors, PR updates and removals, direct pushes to
`main`, and **random crash points** (the engine is killed after a random number
of log writes and recovers from its log). After every merge the checker asserts
that the commit had a passing CI run on exactly that commit and that the
ground-truth tree is not broken. At the end it asserts that every PR is in a
terminal state and that `main`'s contents match the PR states.
`go test ./sim` runs 7 queue configurations × 1,000 seeds, and
`go test ./sim -long` runs 7 × 20,000. Both pass with zero violations (the
`-long` run took 46 s on the 4-vCPU dev container).

## Architecture

```
                          ┌──────────────────────── queue.Engine (pure logic) ───────────────────────┐
 PR events ──► Enqueue/   │  lanes ─► groups ─► speculative window (Depth)                             │
 (forge)       Update/    │   • lane = PRs with overlapping affected targets                          │
               Remove     │   • group = batch merged as ONE commit                                     │
                          │   • run for group i tests main+g0+…+gi (suspects skipped)                  │
                          │  merge iff head ∧ passed ∧ run.parent == main  ──► Forge.Advance (CAS FF)  │
                          │  every transition written ahead to the Store (event log)                    │
                          └──────┬──────────────────────────────┬──────────────────────────┬──────────┘
                                 │ Start/Cancel(Run)             │ Append/Load              │ Head/Advance/Contains
                                 ▼                               ▼                          ▼
                     CI: runner (git worktree +        store.SQLite (WAL,          forge: localgit / GitHub
                     greenline build) or sim CI        synchronous=FULL)           (or the sim world)
                                 │
                                 ▼ action key = sha256(cmd, env allowlist, input digests)
                 ┌──────────── cache ────────────┐
                 │ actioncache: key → result      │◄── HTTP: GET/HEAD/PUT /ac/<sha256>, /cas/<sha256>
                 │ cas: sha256 → blob (disk LRU)  │    (bazel --remote_cache=http://…)
                 └────────────────────────────────┘
```

| package | what it does |
|---|---|
| `cas` | Content-addressed blob store. Digest = sha256 + size. Disk backend with size-bounded LRU eviction, atomic temp+rename writes, a hash check on every read (a corrupt blob is deleted and reported as a miss), a memory backend and an S3 stub. Digest parsing is fuzzed. |
| `actioncache` | Maps action key → result (output digests, exit code, stdout and stderr digests). Caches only successful actions. An entry whose outputs were evicted from the CAS counts as a miss. |
| `httpcache` | Server and client for Bazel's HTTP remote-cache protocol: optional URL prefix, chunked uploads, upload hash verification, path-traversal rejection (fuzzed). |
| `build` | Small build tool. `BUILD.yaml` targets have `srcs`, `deps`, `cmd`, `outs` and `test`. Each action runs in a fresh temp dir that holds only its declared inputs, with a scrubbed environment. A timeout or cancellation kills the whole process group. |
| `affected` | Maps changed files to affected targets (owners plus the reverse-dependency closure, plus targets whose definitions changed). |
| `queue` | The merge-queue engine: strategies, flaky retry and quarantine, infra retry with backoff, timeouts, updates, removals, external pushes, write-ahead log and recovery. |
| `flaky` | Flake tracking. Fail followed by pass on identical inputs counts as a flake. A test is quarantined after N flakes (default 2). |
| `store` | SQLite event log (`modernc.org/sqlite`, no cgo). |
| `sim` | Discrete-event simulator with ground truth, a fake CI with a per-target cache model, and the invariant checker. |
| `forge/localgit` | A bare git repo used as the forge. Candidate commits are built without a worktree (`git merge-tree --write-tree`, then `git commit-tree`) with a fixed identity and date, so they are deterministic. `Advance` checks that the new commit's only parent is the old `main`, then runs `git update-ref main new old`. |
| `runner` | Real CI. It builds the speculative commit, exports its tree, computes the affected targets from the git diff, and builds and tests them through the cache. Cancelling a run kills the build's process groups. `VerifyMain` rebuilds every `main` commit from scratch. |
| `server` | One goroutine owns the engine and serialises API calls, CI results, timers and `main` polling. Also serves a JSON API, an HTML status page, Prometheus `/metrics`, and the HTTP cache under `/cache/`. |
| `metrics` | Prometheus collector: queue depth, lanes, runs started and cancelled, merges, ejections by reason, flakes, CAS hits, misses, bytes, evictions and corrupt blobs, build actions executed vs cached, and a time-to-merge histogram. |
| `demo` | The 20-PR scenario and the kill -9 integration test. |
| `reapi` | REAPI v2 gRPC cache: Capabilities, ContentAddressableStorage (FindMissingBlobs, BatchUpdateBlobs, BatchReadBlobs), ActionCache, and ByteStream Read, Write and QueryWriteStatus. It shares one action-cache store with `httpcache`, because both hold serialized `ActionResult` protos. A `GetActionResult` whose outputs were evicted returns NotFound. Remote execution is not implemented. |
| `forge/github` | `Mirrored` (used by `serve -github`): candidate commits are built in a local bare mirror and pushed to `refs/heads/greenline/<sha>`, then `main` is moved through the API. Git credentials go through `GIT_CONFIG_*` env vars, never argv. Underneath that, REST `Forge`: `Advance` checks for a single parent, then does a fast-forward-only ref update, which acts as a CAS. Also check runs, and a webhook handler with constant-time `X-Hub-Signature-256` verification (fuzzed) for label-driven enqueue, synchronize, unlabel, close and push-to-main. Tested against an `httptest` fake. The token comes only from `GITHUB_TOKEN` and never appears in errors. |

### Strategies (one mechanism)

A lane is an ordered list of groups. The first `Depth` groups are tested at
once. The run for group *i* tests `main + g0 + … + gi`.

| preset | config | behaviour |
|---|---|---|
| `serial` | Batch 1, Depth 1 | Test one PR on `main`, then merge it. Throughput is 1 / CI time. |
| `batch` | Batch 4, Depth 1 | Test up to 4 PRs as one commit. If the batch fails, split it in half and test the halves in order (bisection). |
| `speculative` | Batch 1, Depth 4, skip suspects | Zuul-style. Test `#1`, `#1+#2`, `#1+#2+#3` and so on in parallel. If `#2` fails while `#1` is still running, rebuild `#3` as `#1+#3` right away. If `#1` then fails too, `#2`'s failure evidence (which included `#1`) is discarded and `#2` is retested. |
| `lanes` | Lanes, Depth 1 | PRs with disjoint affected targets are tested in parallel. After each merge or ejection, the PRs not yet committed to a run are re-placed in queue order, so a PR waits only for earlier PRs it actually overlaps. |
| `lanes+speculative` | both | Speculation inside each lane. |

When `main` moves under a lane (another lane merged, or someone pushed
directly), that lane's running builds are **not cancelled**. They finish, so
their actions land in the cache. Their results are then discarded, and the
group is re-verified on the new `main`. Because the targets are disjoint, that
re-verification is almost entirely cache hits.

### Crash recovery

The engine writes every state change ahead to the log: `enqueue`, `update`,
`remove`, `eject`, `flake`, `merge_intent`, `merged`. Merging means writing
`merge_intent`, then calling `Advance`, then writing `merged`. On restart the
engine replays the log. For any `merge_intent` without a matching `merged`, it
asks the forge whether that commit is on `main` and records the answer. Every PR
that was being tested goes back to the queue. **Test results are never
persisted, so a lost result means a re-test, never a merge.** Unit tests cover
a crash between `Advance` and `merged` and a crash before `Advance`. The
property test crashes at random write points, including during recovery
itself.

## End-to-end demo (measured)

```bash
go run ./cmd/greenline demo
```

This creates a bare git repo from `examples/mono`, pushes 20 PR branches and
runs the real queue (lanes+speculative) with real shell builds through the
cache. It prints a timeline and then **rebuilds every commit on `main` from
scratch with no cache reads** to check that each one is green. The 20 PRs
include:

- 3 PRs that are broken on their own (#7, #8, #19);
- a pair that each pass alone but break `worker_test` together (#9, #10:
  both add a `deploy` job);
- a textual git conflict (#11 against #1);
- a deliberately flaky test (#12, fails about 35% of runs);
- a broken PR that gets a fix pushed while it is queued (#15);
- 12 good PRs, some touching shared libraries.

From one run ([full transcript](docs/demo.md)):

```
#7   EJECTED   api: drop /status (breaks api_test)          [broken]   test_failed //app:app_test //services/api:api_test
#9   MERGED    worker: add deploy job (a)                   [pair]
#10  EJECTED   worker: add deploy job (b)                   [pair]     test_failed //services/worker:worker_test
#11  EJECTED   docs: reword tagline (conflicts with #1)     [conflict] unmergeable <merge-conflict>
#15  MERGED    worker: duplicate job (fixed while queued)   [fixed]    runs=2
14/20 merged in 14 commits in 11.1s; 56 runs started, 11 cancelled, 15 stale results discarded
build actions: 138 executed, 293 served from cache (68% hit)
main is green at all 15 commits (flaky targets retried up to 6 times)
```

### kill -9 test

```bash
go test -tags integration ./demo -run Kill9 -v
```

The test runs `greenline serve` as a subprocess on the same 20 PRs. Every
150–1150 ms it **SIGKILLs** the server, then restarts it from its SQLite log,
and keeps going until the queue drains (up to 15 kills). It then checks that:

- every `main` commit is a single-parent child of the previous one, and each
  has a `merge_intent` in the log;
- every PR ended in a terminal state, and merged PRs appear on `main` exactly
  once while all others are absent;
- no broken PR merged;
- every `main` commit is green when rebuilt from scratch.

It passed on 5 consecutive runs with 9–11 SIGKILLs each. An earlier version
of the test caught a real bug: a re-delivered enqueue for an already-ejected
PR re-queued it. Enqueue is now a no-op when the PR's head is unchanged.

## Results (simulated)

Simulated, not measured. Reproduce:

```bash
go run ./cmd/greenline sim -p 0.02,0.10,0.25 -rate 12
```

The simulation is deterministic per seed. Each cell is 5 seeds × 400 PRs. The
synthetic repo has 60 targets in a random layered DAG. Each PR touches 1–2
targets, skewed towards leaves. CI costs a fixed 2 min plus 45–135 s per
affected target on a cache miss, or 2 s on a hit. 2% of PRs break only when
combined with a recent overlapping PR. 2% of runs hit a flaky test, and 0.5%
hit an infra error. *TTM* is time to merge, from enqueue to landing. `none` is
the no-queue baseline: CI runs on the branch, and a passing PR merges onto
whatever `main` is by then without a re-test. Breaking merges are counted and
reverted. The full grid (6 and 12 PRs/h, cache on and off) is in
[docs/results.md](docs/results.md).

**p = 0.10 (10% of PRs broken), 12 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 10.7 | 5m | 9m | 6.2 | 0% | 0% | 0 | 2 |
| serial | 8.9 | 3.5h | 8.7h | 6.7 | 0% | 6% | 20 | 0 |
| batch | 7.5 | 7.9h | 16h | 8.0 | 0% | 30% | 20 | 0 |
| speculative | 10.7 | 7m | 36m | 9.8 | 8% | 47% | 21 | 0 |
| lanes | 10.6 | 8m | 1.3h | 11.2 | 0% | 50% | 20 | 0 |
| lanes+speculative | 10.7 | 7m | 32m | 12.0 | 2% | 58% | 20 | 0 |

**Same scenario, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI |
|---|---:|---:|---:|---:|---:|
| serial | 8.5 | 4.3h | 11h | 7.0 | 0% |
| speculative | 9.6 | 1.7h | 6.4h | 22.5 | 12% |
| lanes | 7.9 | 5.8h | 15h | 20.1 | 0% |
| lanes+speculative | 9.3 | 3.1h | 7.9h | 43.0 | 4% |

**p = 0.02 and p = 0.25, 12 PRs/hour, cache on (excerpt)**

| strategy | p = 0.02: p95 TTM | CI min / PR | p = 0.25: p95 TTM | CI min / PR |
|---|---:|---:|---:|---:|
| serial | 7.3h | 5.9 | 12h | 8.7 |
| batch | 3.6h | 5.0 | 38h | 14.4 |
| speculative | 32m | 8.1 | 46m | 14.2 |
| lanes | 58m | 9.1 | 1.2h | 14.2 |
| lanes+speculative | 28m | 9.5 | 36m | 16.5 |

What the numbers say:

- **Serial is overloaded at 12 PRs/h.** Its capacity in this model is about 9
  merges/h at p = 0.10, so its queue grows for the whole 33-hour run. Its p95
  depends on run length, so the comparison that matters is that the other
  strategies keep up and serial does not. At 6 PRs/h, serial's p95 is 36 min
  ([full grid](docs/results.md)).
- **Speculation plus the cache:** at p = 0.10, p95 TTM falls from 8.7h
  (serial) to 32–36 min, with zero main breakages. That costs 46% more CI per
  merged PR (9.8 vs 6.7 min) for `speculative`, and 79% more for
  `lanes+speculative`.
- **The cache is what makes speculation affordable.** Without it, speculative
  CI per merged PR goes from 9.8 to 22.5 min (2.3×), lanes+speculative from
  12.0 to 43.0 min (3.6×), and latency loses most of its gain because the extra
  builds compete for time. Lanes without a cache are *worse* than serial: every
  merge forces a full-cost re-verification in the other lanes. With the cache,
  that re-verification is mostly hits (50% hit ratio for `lanes`).
- **Batching only pays when failures are rare.** At p = 0.02, `batch` uses the
  least CI (5.0 min/PR) and halves serial's p95. At p = 0.25, about 68% of
  4-PR batches contain a broken PR, bisection dominates, and batching is the
  worst strategy (p95 38h).
- **Speculation wastes CI as p grows**: 3% of CI minutes are cancelled at
  p = 0.02, 8% at 0.10 and 19% at 0.25. Lanes waste almost nothing, because
  stale runs are allowed to finish into the cache instead of being cancelled.
- **No queue is fast and breaks main.** It has the best latency, but conflicting
  PRs that each passed on their own branch broke `main` 2 times in 2,000 PRs at
  this low conflict rate (2%). Every queue strategy had 0 breakages, at the cost
  of ejecting the second PR of each conflicting pair (the "ejected, not broken
  alone" column).

## Cache and build tool (measured)

Measured on the development container: 4 vCPU Intel Xeon @ 2.80GHz, local
disk, Go 1.26.

```bash
go test ./cas -run x -bench . -benchtime 2s
```

| benchmark | throughput | latency |
|---|---:|---:|
| put 4 KiB (verify + temp file + rename) | 45 MB/s | 91 µs |
| get 4 KiB (read + sha256 verify) | 168 MB/s | 24 µs |
| get 4 KiB, 4 parallel readers | 470 MB/s | 8.7 µs |
| put 1 MiB | 117 MB/s | 9.0 ms |
| get 1 MiB | 246 MB/s | 4.3 ms |
| get 1 MiB, 4 parallel readers | 584 MB/s | 1.8 ms |

`examples/mono` is a 15-target workspace: 5 libraries and services, a code
generator, an app bundle, 7 tests and a docs target, with a dependency chain
four deep. Each command sleeps 0.1–0.3 s as a stand-in for compile time, so
these numbers show cache behaviour, not real compiler cost:

```bash
go run ./cmd/greenline build -root examples/mono -cache /tmp/c   # cold
go run ./cmd/greenline build -root examples/mono -cache /tmp/c   # warm
go run ./cmd/greenline cache -addr :9092 &                       # remote
go run ./cmd/greenline build -root examples/mono -remote http://localhost:9092
```

| build | executed | cached | wall time |
|---|---:|---:|---:|
| cold, local disk cache | 15 | 0 | 1.06 s |
| warm, local disk cache | 0 | 15 | 1 ms |
| cold, remote HTTP cache (localhost) | 15 | 0 | 1.08 s |
| warm, remote HTTP cache (localhost) | 0 | 15 | 7 ms |

Changing one leaf file (`services/worker/jobs.txt`) rebuilds exactly its
reverse dependencies: `worker`, `worker_test`, `app` and `app_test`. That is
asserted in `build/build_test.go`. A failing test is never cached, so its
failure reproduces on the next build instead of becoming a false hit.

## Reproduce everything

```bash
go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go test -race ./...                      # unit + 7x1000-seed property test (~25 s)
go test ./sim -run TestInvariantProperty -long   # 7x20000 seeds (~45 s)
go run ./cmd/greenline sim -cache both   # full results grid (docs/results.md)
go test ./cas -run x -bench .            # CAS benchmarks
go run ./cmd/greenline demo              # 20-PR end-to-end demo + from-scratch verification
go test -tags integration ./demo -v      # kill -9 crash-recovery test (~40 s)
go run ./cmd/greenline serve -repo /tmp/r.git -init-from examples/mono -addr :8080 -grpc :9093
#   POST /api/prs {"id":1,"branch":"pr/1"} · GET / (status page) · GET /metrics · /cache/
#   bazel build --remote_cache=http://localhost:8080/cache   (or grpc://localhost:9093)
go test -tags bazel ./reapi -v            # needs bazel on PATH (not run here)
docker build -t greenline . && docker run --rm greenline demo -src /src/examples/mono
GITHUB_TOKEN=… GREENLINE_WEBHOOK_SECRET=… go run ./cmd/greenline serve \
    -github owner/repo -repo /var/lib/greenline/mirror.git -addr :8080
#   point a GitHub webhook (pull_request, push) at /webhook; label PRs "merge-queue"
```

## Limitations

- **The simulation is a model.** The DAG, the cost model (targets build
  sequentially inside one run), and the failure and conflict processes are
  synthetic. The relative ordering of strategies is the useful output, not the
  absolute minutes.
- In the cache model, a cache entry becomes visible only when the action that
  produced it finishes. Two runs that start at the same time both miss, as they
  would without remote execution's in-flight deduplication.
- **Compatibility with a real Bazel client is untested.** Bazel could not be
  downloaded in the development environment (the network policy blocks
  releases.bazel.build and GitHub release assets).
  - `reapi/bazel_test.go` (`-tags bazel`) builds a two-genrule workspace twice
    against each front end and asserts that the second build is fully
    remote-cached. It has not been run.
  - What *is* tested: `reapi/sdk_test.go` drives the gRPC server with Google's
    official REAPI Go client SDK (`remote-apis-sdks`). That covers
    GetCapabilities, FindMissingBlobs, BatchUpdateBlobs, BatchReadBlobs,
    ByteStream Write and Read of a 6 MiB blob, and the ActionCache. A gRPC
    interceptor asserts that each of those RPCs was called.
  - The HTTP front end stores AC values opaquely. The gRPC front end decodes
    `ActionResult` to check that outputs are present. Instance names are
    ignored, and compressed blobs are rejected.
- **GitHub mode has only been run against a fake** GitHub API, backed by a real
  bare repo that enforces fast-forward-only updates. It has not been run
  against github.com. Candidate commits are pushed to `refs/heads/greenline/*`,
  so the token needs `contents: write` and `checks: write`. Branch protection
  that requires status checks on `main` would reject the queue's
  fast-forward unless the queue's checks are the required ones.
- Quarantine treats failures of quarantined tests as passes. If a real break
  shows up only in a quarantined test, it would get through. This trade-off is
  standard, and quarantined failures are counted (`quarantined_ignored`).
- Lanes are only as good as affected-target analysis. A missing dependency
  edge would put two interacting PRs in different lanes. The invariant still
  holds in that case, because every merge is re-verified on the exact `main`,
  but the extra parallelism would be lost to re-tests.
- The event log is compacted by snapshot: once it passes 5,000 events,
  `serve` replaces it with one record per PR plus flake stats. Merge history
  is in git, not in the log.
- The real runner builds the affected targets of the whole speculative chain
  relative to its base, so a run for `#1+#2+#3` also rebuilds `#1`'s targets.
  This is deliberate: each run fully tests the commit it reports, so the
  invariant does not depend on a different run having passed. The cache makes
  those rebuilds hits.
- With one flaky retry, a test that fails 35% of the time still ejects a good
  PR about 12% of the time until it has flaked enough times to be quarantined.
  The demo sometimes shows this with #12.
- `build` sandboxes by giving each action a directory and a scrubbed
  environment. It does not use namespaces, so an action that reads absolute
  paths can escape its declared inputs.

## Compared with

- **GitHub merge queue**: tests each entry on a `merge_group` branch that
  contains the entries ahead of it, several at once, which is similar to
  `Depth`. With a merge limit above 1, several PRs land together, and the
  commits between them on `main` were not each tested. greenline merges a batch
  as a single commit, so every commit on `main` was tested. GitHub has no
  affected-target lanes and no shared build cache.
- **Zuul (dependent pipelines)**: greenline's `speculative` mode follows Zuul's
  model. When an item fails, items behind it are rebuilt without it, and an
  item can't merge before the items ahead of it. greenline adds batch
  bisection, lanes, and a cache shared by the speculative builds.
- **Uber SubmitQueue**: builds a speculation tree weighted by predicted success
  probabilities, and uses target analysis to detect independent changes.
  greenline's speculation is a plain linear chain with culprit skipping and no
  learned model. Its lanes are the same independence idea, with cheap
  re-verification through the cache instead of skipping verification.
- **bazel-remote / BuildBuddy / EngFlow**: production remote caches (and, for
  the last two, remote execution). greenline's cache implements the HTTP
  protocol only, with a disk LRU. Its point is the interaction with the queue,
  not competing as a cache.
- **Mergify, Trunk, Graphite**: hosted merge queues with batching, parallel
  queues and flaky-test handling. greenline is an open, measurable
  implementation of the same ideas, with a property-tested invariant.
