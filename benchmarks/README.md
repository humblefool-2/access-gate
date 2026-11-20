# Benchmarks

Goal: measure **how much latency the access-gate library adds** to a normal request. Results are in [RESULTS.md](RESULTS.md).

Same requests, same data, same store, two setups:

- **baseline**: the handlers served directly (`GUARD=off`), no guard and no projection, but the service's own authentication and role check
- **guarded**: the same handlers wrapped by `Guard.Handle` (what `make run` serves)

The number that matters is the **delta** (guarded minus baseline) at p50, p95 and p99, plus the allocations it adds. Run pairs back to back on one machine, several repetitions, and compare with `benchstat`. The same harness works with any other `accessgate.Authenticator`.

**Stores.** The harness runs against SQLite, the fastest realistic store, so the library's share of latency is at its largest. Slower stores (Kafka, OpenSearch, ...) are covered by a stub handler that sleeps 1, 5, 20 or 100 ms. The per-request cost should not change; the exception is concurrent writes to one entity, which the guard serialises (the stale-read check and the write must be atomic), so they cap at about 1 / store latency. Writes to different entities run in parallel.

## What is measured

| Benchmark | Where | Measures | Run |
| --- | --- | --- | --- |
| `BenchmarkRequest` | `examples/wallet-service/bench_test.go` | Per-request overhead: 3 routes x 3 principals x baseline/guarded, status asserted per case | `go test -run '^$' -bench BenchmarkRequest -benchmem -count 10 ./examples/wallet-service` |
| `BenchmarkStoreLatency` | same file | Stub store with injected delay, reads and writes, 1 vs 64 entities (`b.RunParallel`, 96 workers sharing one session) | `go test -run '^$' -bench BenchmarkStoreLatency -benchmem ./examples/wallet-service` |
| `BenchmarkProject`, `BenchmarkProjectNarrow` | `lib/bench_test.go` | `Project` by view and payload size (1 to 1000 rows), with `?fields=` and `MaxRows`; no HTTP or DB | `go test -run '^$' -bench BenchmarkProject -benchmem ./lib` |
| agent load | `agentload/` | Concurrency, stale-read contention, denied traffic (below) | `make agentload`, `make bench-compare` |
| memory growth | `memrun.sh` | Service RSS under sustained load | `make bench-memory` (`DURATION`, `SAMPLE`) |

Run from the repo root; `-run '^$'` skips unit tests. Pair `BenchmarkRequest` runs with `benchstat -col /mode`. For `POST credit`, the `read` and `read-pii` principals are refused (403) by the service's role check on both muxes, so those rows compare two refusals, not real writes.

## Agent-flow load (`agentload/`)

Agents send dependent multi-step tasks (read, think, write, verify), retry after refusals, and pause for LLM latency between steps. `agentload` reproduces that shape against a running service with no LLM (free, deterministic with `-seed`).

```bash
make run                                                    # terminal 1
make agentload ARGS="-agents 20 -duration 15s"              # terminal 2: one private wallet per agent
make agentload ARGS="-agents 20 -wallets 1 -think 50ms"     # hot wallet: contention
```

Each task is one session (`X-Session-ID`) on one wallet, picked from a weighted mix (`-mix`):

| Flow | Steps |
| --- | --- |
| `topup` | read wallet, think, credit, think, read again |
| `spend` | read wallet, debit up to available, read again |
| `hold` | read, create hold, read, release (REST only) |
| `browse` | `get_customer` and `get_wallet` in parallel, think, read again |
| `denied` | read-only token tries to credit; must get 403 |
| `retry_storm` | read, a second session credits, then resend the same credit 3 times without re-reading: each must be a 409 and move no money |
| `rewrite` | read-only token asks for `email` (403), rewrites to `customer_id,phone` and gets the masked view; a write token credits with a bad `?fields=` (403, nothing saved), then retries |

`retry_storm` and `rewrite` are opt-in (not in the default `-mix`), for example `-mix topup=3,spend=2,retry_storm=2,rewrite=2`. Both need the guard; `rewrite` also needs `STRICT_FIELDS=on`.

On a 409 the agent re-reads, thinks and retries up to `-max-retries`, then the task fails. A business refusal such as insufficient funds counts as `rejected`, not `failed`.

Flags: `-agents`, `-wallets` (0 = one per agent), `-duration`, `-ramp`, `-think` and `-sigma` (LLM latency stand-in), `-api tools|rest`, `-mix`, `-initial`, `-seed`, `-write-token`, `-read-token`, `-max-retries`, `-url`.

Output: per-operation counts and p50/p95/p99/max, per-flow outcomes and active time (think time removed), stale blocks and retries. At the end it checks that each wallet's balance equals its start plus confirmed credits minus confirmed debits (exit 1 on failure; a transport error on a write makes the outcome unknown, so it prints WARN).

Notes: tasks stop starting at `-duration`, so runs finish slightly late. Each run creates its own customers and wallets, so the DB accumulates. Compare request percentiles, not total task time, because think time inflates it.

### Baseline vs guarded

`make bench-compare ARGS="..."` (`compare.sh`) runs the same traffic against both variants `REPS` times (default 5). Each run gets a fresh service and DB, the side that runs first alternates, and the traffic is identical (same seed, REST, no `denied` flow). It prints the baseline median and the median and [min, max] of the paired delta at p50, p95 and p99.

```bash
REPS=7 make bench-compare ARGS="-agents 1 -think 0 -duration 10s"   # per-request overhead, no queueing
REPS=5 make bench-compare ARGS="-agents 32 -think 0 -wallets 32"    # concurrency
```

Use `-think 0` (closed loop) to isolate service time. If a range spans zero, the cost is below what this setup can resolve. Request counts differ between sides in a closed loop, so compare percentiles, not counts. A single pair is only a smoke test.

Manual version: `agentload -baseline -save base.json` against a `GUARD=off` service, then `agentload -vs base.json` against the guarded one; `agentload -aggregate <dir>` combines saved pairs.

## Run from the UI

`make playground` runs the main benchmarks from dropdowns: pick a **benchmark** and a **size**, press Run.

| Benchmark | Underneath | Quick / Standard / Thorough |
| --- | --- | --- |
| Request overhead | `BenchmarkRequest` + `benchstat` | 1 / 5 / 10 runs per case |
| Datastore latency | `BenchmarkStoreLatency` + `benchstat` | delays 0, 5, 100 ms / all 5 delays / all 5 delays x 3 runs |
| Compare Performance | `compare.sh` | 3 pairs of 5 s / 5 pairs of 10 s / 7 pairs of 10 s |

Output streams live and Stop is active while a run is. The browser sends only the benchmark and size; the server maps them to a fixed command list (`playground/bench.go`). Memory growth and single agent-load runs are not in the UI.

## Re-running everything

`caffeinate -i make bench-all` (`run-all.sh`) runs the playground presets, `BenchmarkStoreLatency` at `-count 10` and the 10 minute memory run (about 35 min, or one part: `presets`, `samples`, `memory`) and saves the output under `benchmarks/results/<date>/` (git-ignored). Use an idle machine and edit no files while it runs. The 32-agent paired run is separate (command above).
