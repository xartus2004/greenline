# Simulation results (full grid)

Simulated, not measured. Reproduce with:

```bash
go run ./cmd/greenline sim -cache both
```

400 PRs per run × 5 seeds per cell (seeds 1–5). Each PR touches 1–2 of 60 targets in a random layered DAG; CI = 2 min fixed overhead + 45–135 s per affected target on a miss, 2 s on a hit; 2% of PRs conflict with a recent overlapping PR; 2% of runs hit a flaky test; 0.5% hit an infra error. `none` is the no-merge-queue baseline (CI on the branch, merge without re-testing; a breaking merge is counted and reverted).

**p = 0.02, 6 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 5.8 | 5m | 9m | 5.7 | 0% | 0% | 0 | 2 |
| serial | 5.8 | 7m | 32m | 5.9 | 0% | 2% | 25 | 0 |
| batch | 5.8 | 7m | 41m | 5.6 | 0% | 5% | 24 | 0 |
| speculative | 5.8 | 5m | 17m | 7.1 | 1% | 34% | 23 | 0 |
| lanes | 5.8 | 6m | 19m | 7.1 | 0% | 37% | 23 | 0 |
| lanes+speculative | 5.8 | 6m | 15m | 7.5 | 0% | 44% | 23 | 0 |

**p = 0.02, 6 PRs/hour, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 5.8 | 5m | 9m | 5.7 | 0% | 0% | 0 | 2 |
| serial | 5.8 | 8m | 33m | 6.0 | 0% | 0% | 24 | 0 |
| batch | 5.8 | 7m | 56m | 5.8 | 0% | 0% | 23 | 0 |
| speculative | 5.8 | 8m | 39m | 10.6 | 3% | 0% | 24 | 0 |
| lanes | 5.8 | 9m | 4.3h | 13.6 | 0% | 0% | 23 | 0 |
| lanes+speculative | 5.8 | 8m | 3.3h | 20.8 | 2% | 0% | 23 | 0 |

**p = 0.10, 6 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 5.4 | 5m | 9m | 6.2 | 0% | 0% | 0 | 2 |
| serial | 5.4 | 8m | 36m | 6.7 | 0% | 6% | 20 | 0 |
| batch | 5.4 | 8m | 1.5h | 7.0 | 0% | 16% | 20 | 0 |
| speculative | 5.4 | 6m | 18m | 8.3 | 5% | 37% | 20 | 0 |
| lanes | 5.4 | 6m | 22m | 8.2 | 0% | 36% | 20 | 0 |
| lanes+speculative | 5.4 | 6m | 16m | 8.9 | 1% | 44% | 20 | 0 |

**p = 0.10, 6 PRs/hour, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 5.4 | 5m | 9m | 6.2 | 0% | 0% | 0 | 2 |
| serial | 5.4 | 8m | 41m | 7.0 | 0% | 0% | 21 | 0 |
| batch | 5.3 | 44m | 8.0h | 9.0 | 0% | 0% | 20 | 0 |
| speculative | 5.4 | 8m | 57m | 14.3 | 9% | 0% | 20 | 0 |
| lanes | 5.4 | 8m | 2.6h | 15.1 | 0% | 0% | 20 | 0 |
| lanes+speculative | 5.4 | 7m | 2.5h | 23.5 | 3% | 0% | 20 | 0 |

**p = 0.25, 6 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 4.5 | 5m | 9m | 7.5 | 0% | 0% | 0 | 2 |
| serial | 4.5 | 9m | 45m | 8.7 | 0% | 12% | 17 | 0 |
| batch | 4.1 | 3.5h | 11h | 13.0 | 0% | 35% | 17 | 0 |
| speculative | 4.5 | 6m | 20m | 11.4 | 10% | 42% | 17 | 0 |
| lanes | 4.5 | 6m | 23m | 11.0 | 0% | 35% | 17 | 0 |
| lanes+speculative | 4.5 | 5m | 18m | 11.8 | 2% | 43% | 17 | 0 |

**p = 0.25, 6 PRs/hour, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 4.5 | 5m | 9m | 7.5 | 0% | 0% | 0 | 2 |
| serial | 4.5 | 11m | 1.1h | 9.4 | 0% | 0% | 17 | 0 |
| batch | 3.0 | 18h | 39h | 20.0 | 0% | 0% | 17 | 0 |
| speculative | 4.5 | 12m | 1.9h | 25.0 | 18% | 0% | 17 | 0 |
| lanes | 4.5 | 7m | 3.4h | 18.4 | 0% | 0% | 17 | 0 |
| lanes+speculative | 4.5 | 7m | 3.2h | 31.2 | 5% | 0% | 18 | 0 |

**p = 0.02, 12 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 11.6 | 5m | 9m | 5.7 | 0% | 0% | 0 | 2 |
| serial | 10.1 | 2.6h | 7.3h | 5.9 | 0% | 2% | 25 | 0 |
| batch | 11.2 | 54m | 3.6h | 5.0 | 0% | 14% | 23 | 0 |
| speculative | 11.6 | 6m | 32m | 8.1 | 3% | 42% | 24 | 0 |
| lanes | 11.6 | 8m | 58m | 9.1 | 0% | 53% | 23 | 0 |
| lanes+speculative | 11.6 | 7m | 28m | 9.5 | 1% | 58% | 23 | 0 |

**p = 0.02, 12 PRs/hour, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 11.6 | 5m | 9m | 5.7 | 0% | 0% | 0 | 2 |
| serial | 10.0 | 2.9h | 7.9h | 6.0 | 0% | 0% | 24 | 0 |
| batch | 10.2 | 1.4h | 11h | 5.7 | 0% | 0% | 23 | 0 |
| speculative | 11.5 | 24m | 2.2h | 15.5 | 4% | 0% | 23 | 0 |
| lanes | 8.6 | 5.9h | 15h | 17.5 | 0% | 0% | 23 | 0 |
| lanes+speculative | 10.4 | 2.0h | 7.5h | 35.2 | 2% | 0% | 23 | 0 |

**p = 0.10, 12 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 10.7 | 5m | 9m | 6.2 | 0% | 0% | 0 | 2 |
| serial | 8.9 | 3.5h | 8.7h | 6.7 | 0% | 6% | 20 | 0 |
| batch | 7.5 | 7.9h | 16h | 8.0 | 0% | 30% | 20 | 0 |
| speculative | 10.7 | 7m | 36m | 9.8 | 8% | 47% | 21 | 0 |
| lanes | 10.6 | 8m | 1.3h | 11.2 | 0% | 50% | 20 | 0 |
| lanes+speculative | 10.7 | 7m | 32m | 12.0 | 2% | 58% | 20 | 0 |

**p = 0.10, 12 PRs/hour, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 10.7 | 5m | 9m | 6.2 | 0% | 0% | 0 | 2 |
| serial | 8.5 | 4.3h | 11h | 7.0 | 0% | 0% | 21 | 0 |
| batch | 5.7 | 16h | 31h | 10.5 | 0% | 0% | 20 | 0 |
| speculative | 9.6 | 1.7h | 6.4h | 22.5 | 12% | 0% | 21 | 0 |
| lanes | 7.9 | 5.8h | 15h | 20.1 | 0% | 0% | 20 | 0 |
| lanes+speculative | 9.3 | 3.1h | 7.9h | 43.0 | 4% | 0% | 20 | 0 |

**p = 0.25, 12 PRs/hour, cache on**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 9.0 | 5m | 9m | 7.5 | 0% | 0% | 0 | 2 |
| serial | 6.8 | 5.6h | 12h | 8.7 | 0% | 12% | 17 | 0 |
| batch | 4.2 | 21h | 38h | 14.4 | 0% | 39% | 17 | 0 |
| speculative | 8.9 | 9m | 46m | 14.2 | 19% | 52% | 17 | 0 |
| lanes | 8.9 | 8m | 1.2h | 14.2 | 0% | 45% | 18 | 0 |
| lanes+speculative | 8.9 | 7m | 36m | 16.5 | 5% | 56% | 17 | 0 |

**p = 0.25, 12 PRs/hour, cache off**

| strategy | merged/h | p50 TTM | p95 TTM | CI min / merged PR | wasted CI | cache hit | ejected, not broken alone | main breakages |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| none | 9.0 | 5m | 9m | 7.5 | 0% | 0% | 0 | 2 |
| serial | 6.3 | 7.3h | 16h | 9.4 | 0% | 0% | 17 | 0 |
| batch | 2.9 | 37h | 67h | 20.5 | 0% | 0% | 17 | 0 |
| speculative | 6.4 | 6.6h | 16h | 36.9 | 21% | 0% | 17 | 0 |
| lanes | 6.7 | 5.5h | 15h | 24.6 | 0% | 0% | 17 | 0 |
| lanes+speculative | 7.5 | 3.3h | 11h | 57.3 | 7% | 0% | 17 | 0 |
