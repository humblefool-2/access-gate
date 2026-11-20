# Benchmark results

Measured on an Apple M4 Pro (12 cores), Go 1.26.5, darwin/arm64, one laptop with no isolation, after the review fixes and the removal of the audit trail. The method and the commands are in [README.md](README.md); the raw output is not kept in the repo (`make bench-all` regenerates it under `benchmarks/results/`, which is git-ignored).

"Without" is the same handlers served directly (`GUARD=off`), with the service's own authentication and role check. "With" adds the library (projection and the stale-read check). Compare the deltas inside one table, not across machines or days.

Caveats:
- Single machine, laptop. The datastore is an in-memory stub with injected delay, not a real store.
- Throughput varies more between reps than between modes, so read the medians and the ranges.
- The playground presets were run from a script with the playground's commands, not through the UI.

## Per-request overhead (`BenchmarkRequest`, thorough preset, 10 runs, medians)

| Route | Principal | Without | With | Added | Allocs with (without) |
|---|---|---|---|---|---|
| `GET /customers/{id}` | read | 15.1 µs | 21.4 µs | +6.3 µs | 237 (100) |
| `GET /customers/{id}` | read-pii, write | 15.2 to 15.5 µs | 21.8 to 21.9 µs | +6.4 to +6.6 µs | 239 (100) |
| `GET /wallets/{id}` | read, read-pii, write | 17.5 to 17.6 µs | 22.6 to 22.8 µs | +5.0 to +5.2 µs | 193 (88) |
| `POST /wallets/{id}/credit` | write | 43.5 µs | 49.2 µs | +5.7 µs | 258 (153) |
| `POST /wallets/{id}/credit` | read, read-pii | 1.8 µs | 1.8 µs | 0 | 38 (38) |

The library adds about 5 to 7 µs and about 2x the allocations per request (the response is decoded, projected and written again). The last row is a 403 from the service's own role check on both muxes, so it says nothing about the library; quote the per-route rows, not a geomean.

## Agent load (`make bench-compare`, paired runs, medians, ms)

"Added" is the median of guarded minus baseline per pair, so it is not the difference of the other columns.

**1 agent, no think time, 7 paired runs** (ranges of the p50 delta include zero or stay within +0.019 ms; the p99 ranges span about -0.2 to +0.1 ms, so p99 is noise-limited):

| Op | p50 without | p50 added | p99 without | p99 added |
|---|---|---|---|---|
| credit | 0.148 | +0.005 | 0.275 | +0.040 |
| debit | 0.147 | +0.006 | 0.260 | +0.040 |
| get_customer | 0.109 | +0.012 | 0.207 | +0.067 |
| get_wallet | 0.075 | +0.006 | 0.153 | +0.041 |
| hold_create | 0.191 | +0.011 | 0.348 | +0.017 |
| hold_release | 0.175 | +0.006 | 0.305 | +0.044 |

Throughput about 9.7k req/s with against 10.4k without. 0 stale-read blocks, 0 retries.

**32 agents on 32 wallets, no think time, 5 paired runs:**

| Op | p50 without | p50 added | p99 without | p99 added |
|---|---|---|---|---|
| credit | 3.730 | +0.334 | 11.427 | +1.564 |
| debit | 3.719 | +0.350 | 11.410 | +1.881 |
| get_customer | 2.405 | +0.185 | 9.174 | +0.845 |
| get_wallet | 2.339 | +0.185 | 8.987 | +0.917 |
| hold_create | 3.823 | +0.467 | 11.525 | +1.714 |
| hold_release | 5.049 | +0.499 | 13.698 | +1.496 |

Throughput 9.1k to 10.0k req/s with against 10.3k to 11.0k without. Every p50 range is above zero and the p99 ranges are wide (up to +4.4 ms on one pair), so part of the gap may be machine drift. 0 stale-read blocks, 0 retries.

## Datastore latency (`BenchmarkStoreLatency`, `-count 10`, benchstat 95% intervals 0 to 7%)

A stub that sleeps stands in for the store. Guarded ops/s:

| Delay | Write, 1 entity | Write, 64 entities | Baseline (any) | Read, guarded |
|---|---|---|---|---|
| 0 | 116.9k | 140.5k | 322k | 168.7k to 170.6k |
| 1 ms | 843 | 26.8k | 88k | 90.1k |
| 5 ms | 177.6 | 5.79k | 17.4k | 17.9k to 18.2k |
| 20 ms | 47.8 | 1.52k | 4.55k | 4.54k to 4.56k |
| 100 ms | 9.90 | 342 | 927 | 922 to 925 |

- Writes to one entity run at about 1 / delay, by design: the stale-read check and the write are atomic per entity.
- Writes to 64 entities scale to about a third of baseline. With 256 stripe locks chosen by hash, entities that share a stripe wait on each other, and several workers queue on the same entity.
- Reads match baseline from 1 ms up (no significant difference at 20 and 100 ms). With no delay a guarded read costs 5.9 µs against 2.9 µs, and a write 7.1 to 8.6 µs against 3.1 µs, with 157 allocations against 52 and about 15 KiB against 8.2 KiB per operation.

## Projection cost (`BenchmarkProject`, `./lib`, `-count 5`, medians)

| Rows | public | pii | Allocs |
|---|---|---|---|
| 1 | 1.55 µs | 1.58 µs | 41 |
| 10 | 13.5 µs | 14.2 µs | 358 |
| 100 | 126.5 µs | 129 µs | 3,424 |
| 1000 | 1.39 ms | 1.39 ms | 34,040 |

| Narrow cases | Time | Allocs |
|---|---|---|
| `?fields=` 2 columns, 100 rows | 132 µs | 3,429 |
| `MaxRows=100` applied to 1000 rows | 1.15 ms | 32,235 |

- Cost is linear in rows, about 1.4 µs per row, and the same for both views.
- `MaxRows` saves only about 17% (1.15 ms against 1.39 ms uncapped), because the whole response is decoded before the cap is applied. `?fields=` narrowing does not cut allocations either.

## Memory growth (`make bench-memory`, 10 min)

20 agents, 20 wallets, API tools, think median 50 ms: 353,835 requests (590/s), 121,102 tasks, 0 stale-read blocks, 0 retries. Service RSS 15.5 MB at start, 42.9 MB at 2.5 min, 64.5 MB at 5 min, 105.7 MB at 10.5 min. That is about 0.25 KB of RSS per request, growing linearly with no plateau. The cause was not profiled; the library keeps an unbounded version and seen map per entity by decision, and the example service may keep state too. Do not run a long-lived service on one instance without watching memory.

## Playground timings

| Benchmark | Quick | Standard | Thorough |
|---|---|---|---|
| Request overhead | 3 s | 45 s | 4 min 10 s |
| Datastore latency | 13 s | 23 s | 3 min 16 s |
| Compare Performance | 32 s | 1 min 42 s | 2 min 22 s |

The labels in `playground/bench.go` use these.

## Known gaps

- The datastore is a stub that sleeps, not a real SQLite, Postgres or network store.
- One laptop, no isolation, no CPU pinning.
- No run through the playground UI is recorded.
- Memory grows with every request and entity; there is no eviction yet.
