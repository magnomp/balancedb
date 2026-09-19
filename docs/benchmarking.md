# Benchmarking BalanceDB

How many operations per second one cell can hold, and how to find out on your
hardware. The [README](../README.md) is the overview; spec §11 (scaling model) and
§13 (observability) are the authorities on what the numbers mean.

## What "transactions per second" means here

A registration goes through two independent stages, and either can be the limit:

1. **Insertion** — clients commit rows to `operations` in their own transactions.
   This scales with connections: many inserters commit in parallel.
2. **Decision** — the single elected leader selects committed `PENDING` work in ID
   order and decides it in batches of `batch_size` per commit (spec §8.5). This is
   serial by design, so it is the cell's ceiling.

The number that answers "what can this hold" is the **sustained decision rate**:
the rate at which the leader turns registrations into confirmed balances. Insert
faster than that and the backlog grows without bound (spec N1: no decision
deadline). `make bench` measures both stages at once so you can see which one you
are hitting.

## Running it

```sh
export TEST_DATABASE_URL=postgres://balancedb:balancedb@127.0.0.1:5432/balancedb?sslmode=disable
make bench                                            # default sweep: 1, 4, 16, 64 inserters
make bench ARGS="-workers 8,32 -duration 30s"         # your own levels and window
make bench ARGS="-group-size 5"                       # atomic groups of 5 legs
make bench ARGS="-backdate-days 30"                   # exercise the snapshot cascade
make bench ARGS="-batch-size 500 -loop-interval-ms 100"   # try other config values
make bench ARGS="-json bench.json"                    # machine-readable copy
```

The benchmark creates a throwaway `bench_<hex>` schema, migrates it, runs the
leader in-process (wired exactly as the `processor` role is), and drops the schema
on exit (`-keep` keeps it; `-schema name` uses and never drops a schema of yours).
It talks to Postgres through the same insertion core the HTTP api uses, with no
HTTP in the way, so the figure is the cell's ceiling. For an HTTP-inclusive
picture, run `make loadgen` against an api node and watch the `/metrics` endpoint
(see [operations.md](operations.md#watching-it-locally)).

To benchmark a processor you are running elsewhere (another machine, the Docker
image), point both at one schema and pass `-external-processor -schema <name>`;
the ρ column is then unavailable because it comes from the in-process registry.

Each worker count is one **level**: a warmup, a wait for the leader to drain, a
measured insertion window with that many closed-loop inserters (each one does
`BEGIN`, insert, `COMMIT`, repeat), then a wait for the leader to drain again.
Amounts are positive against unbounded accounts, so every operation is accepted.

## Reading the report

```
workers | insert ops/s | insert p50/p99 | leader ops/s | backlog@end |  drain s | sustained ops/s | e2e p50/p99 ms |   rho
      1 |        216.9 |    4.3 /   8.8 |        216.6 |           4 |     0.02 |           216.5 |     11 /    23 |  1.00
```

| Column | Meaning |
|---|---|
| `insert ops/s` | Operations the inserters committed per second during the window (`registrations/s` = this ÷ group size). |
| `insert p50/p99` | Client-observed `BEGIN`→`COMMIT` latency in ms. |
| `leader ops/s` | Operations the leader decided per second *while inserts were running*. |
| `backlog@end` | `PENDING` rows when the inserters stopped. Growing backlog = inserting faster than the leader decides. |
| `drain s` | How long the leader took to clear that backlog. `!` marks a drain that hit `-drain-timeout`. |
| `sustained ops/s` | Window ops ÷ (window + drain): the leader's real throughput on that workload. **This is the headline number.** |
| `e2e p50/p99 ms` | `registered_at` → `confirmed_at` on the DB clock, for the window's operations. |
| `rho` | Loop utilization ρ (spec §13): leader busy time ÷ (window + drain). `1.00` = saturated. |

Rules of thumb:

- **ρ well below 1 and backlog ≈ 0**: you are measuring the inserters, not the
  cell. The leader has `1/ρ ×` headroom; add workers.
- **ρ ≈ 1 and backlog growing**: you found the ceiling. `sustained ops/s` at this
  level is what the cell holds; more workers only lengthen the drain.
- **`insert p99` in the hundreds of ms while `p50` is single digits**: inserters
  are stalling behind the leader's batch commit (see the finding below), not
  behind each other.
- Compare `leader ops/s` across levels: it should be flat once ρ hits 1. If it
  drops at high worker counts, inserters are stealing DB capacity from the leader.

`-json` writes the same rows with the run's config (`batch_size`, `loop_interval_ms`,
Postgres version) so numbers can be tracked across commits or machines.

## What bounds the ceiling

Per decided operation the leader runs several statements in sequence — account
read, account-version CAS (Guard 3), status flip (Guard 2), snapshot upsert and
cascade — inside one batch transaction, plus the lease fence (Guard 1) per batch.
Two costs dominate:

- **Round trips.** Each statement is one round trip to Postgres. With the database
  on the same host at ~0.1 ms that is a few hundred microseconds per operation;
  across a container boundary or a network hop it is milliseconds. Batching
  (`batch_size`) amortises the commit fsync across the batch but not the per-row
  round trips, so on a slow link the leader is latency-bound, not fsync-bound.
- **Snapshot cascade.** An operation whose `effective_at` is in the past touches one
  snapshot row per later day that already has a snapshot (spec §8.4).
  `-backdate-days N` shows this cost; `effective_at = now` is the cheapest path.

Groups cost per leg plus one `transactions` row; `-group-size` shows the per-leg
amortisation.

## Baseline

Numbers from the development box this was written on — not a target, a point of
reference: 16-vCPU WSL2 host, Postgres 18.6 in a Docker container on a Docker
volume, everything on one machine, default config (`batch_size=200`,
`loop_interval_ms=1000`). Reproduce with `make bench` (singles) and
`make bench ARGS="-group-size 5"`.

Singles (`make bench -workers 1,4,16,64 -duration 15s`), measured 2026-09-19 after
the select-first ledger fix and the round-trip pipelining below:

| workers | insert ops/s | insert p50/p99 ms | leader ops/s | backlog@end | drain s | sustained ops/s | e2e p50/p99 ms | ρ |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 293 | 3.3 / 5.5 | 293 | 1 | 0.02 | **293** | 4 / 8 | 0.97 |
| 4 | 985 | 3.9 / 7.6 | 802 | 2,746 | 2.58 | **840** | 1,464 / 2,526 | 1.00 |
| 16 | 1,275 | 12.0 / 18.9 | 716 | 8,381 | 9.00 | **797** | 5,672 / 8,855 | 1.00 |
| 64 | 1,281 | 49.2 / 61.1 | 645 | 9,569 | 9.46 | **786** | 7,056 / 9,409 | 1.00 |

For comparison, the same sweep before those two changes (kept here as the
pre-fix reference point, not re-measured since):

| workers | insert ops/s | insert p50/p99 ms | leader ops/s | backlog@end | drain s | sustained ops/s | e2e p50/p99 ms | ρ |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 217 | 4.3 / 8.8 | 217 | 4 | 0.02 | 217 | 11 / 23 | 1.00 |
| 4 | 270 | 4.7 / 399 | 254 | 239 | 0.39 | 263 | 383 / 933 | 1.00 |
| 16 | 597 | 9.8 / 548 | 274 | 4,839 | 15.2 | 297 | 8,430 / 14,613 | 1.00 |
| 64 | 889 | 45.9 / 736 | 193 | 10,444 | 33.2 | 277 | 18,610 / 32,828 | 1.00 |

Groups of 5 (`make bench ARGS="-group-size 5 -workers 1,16"`) — **not re-measured**
after the fixes below; the numbers here predate them and are kept only as the
last known groups baseline:

| workers | insert ops/s | insert p50/p99 ms | leader ops/s | backlog@end | drain s | sustained ops/s | e2e p50/p99 ms | ρ |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 411 | 11.4 / 28.4 | 410 | 10 | 0.02 | **410** (82 groups/s) | 27 / 54 | 1.00 |
| 16 | 1,201 | 17.3 / 534 | 408 | 11,895 | 24.4 | **457** (91 groups/s) | 13,433 / 24,267 | 1.00 |

Takeaways from this box:

- The leader saturates (ρ ≈ 1.00) already with a single inserter: the ceiling here
  is the leader's per-row round trips into the container, not fsync. Solid hardware
  with the database on a local socket may move this further; the spec's "thousands
  of decisions/second" assumes that. This has not been measured on other hardware
  or topologies (e.g. Postgres and the processor in separate pods on a cluster,
  which reintroduces a real network hop) — only this box.
- Ceiling here after the fixes below: **~840 ops/s peak sustained as singles**
  (workers=4), up from ~300 ops/s before. Groups of 5 have not been re-measured;
  see above.
- Past the ceiling, more inserters still cost more than they gain: `leader ops/s`
  declines from 802 (4 workers) to 645 (64 workers) as inserters compete with the
  leader for the database, while backlog and end-to-end latency keep growing.
- Insert latency p99 no longer tracks batch duration: it now stays under ~20 ms
  through 16 workers and ~61 ms at 64 workers, versus hundreds of milliseconds
  before the fix below.

## Fixed: inserts waited for the leader's batch, and the leader took more round trips than necessary

Two related round-trip issues were found and fixed on 2026-09-19, together
producing the before/after numbers above (singles: ~300 → ~840 ops/s peak
sustained; insert p99 dropped roughly an order of magnitude at every
concurrency level tested).

**1. Ledger account resolution blocked on the leader's open transaction.**
`internal/ledger.upsertAccountID` ran `INSERT … ON CONFLICT DO NOTHING RETURNING
id` first and only fell back to a `SELECT` when nothing was returned. When the
conflicting `accounts` row had been updated by a transaction that was still
open — which is exactly what the leader's batch does through the Guard-3 CAS —
Postgres made the `INSERT … ON CONFLICT` wait for that transaction to finish
before it could tell whether the conflict stood. So every insert touching an
account the current batch had already confirmed against blocked until the
batch committed, and insert p99 tracked batch duration. Fix: `SELECT` first
(non-blocking under READ COMMITTED); `INSERT … ON CONFLICT` only runs on a
miss (new account), still with `ON CONFLICT` since two inserters can race to
create the same new account.

**2. The leader's single-operation decision made more round trips than its
data dependencies required.** For a plain single operation, `processSingleTx`
ran the lease fence (Guard 1), the account read, Guard 2, Guard 3, the two
snapshot statements, and the outcome notify as seven sequential round trips on
accept (four on reject). Guard 1 and the account read don't depend on each
other's result — which operation this is and which account it targets are
already known in Go before either statement runs — and once the accept/reject
decision is made, Guard 2, Guard 3, the snapshot writes and the notify don't
need each other's return values either, only their own rowcounts, checked
after. Fix: pipeline both groups with `pgx.Batch` (`internal/processor/single.go`
`fenceAndReadAccount`, `accept`, `reject`; `internal/snapshot.QueueApply`/
`ScanApply`), cutting accept to two round trips and reject to two. The account
read → validate boundary is a genuine sequential dependency and can't be
removed this way. The edit, delete and group decision paths were not touched —
the same opportunity exists there but is out of scope of this change.

Neither change alters guard or validation semantics — a rowcount miss on any
guard still rolls back the whole (possibly batched) transaction exactly as
before, whichever statement is read first. Verified with the existing unit,
`itest`, and `simtest` suites (including `TestSimVersionRaceGuard3` and
`TestSimBatchCrashEqualsSequential`, which specifically exercise the guard-miss
and batch-rollback paths), all green with no changes to the simulation
reference model.
