# BalanceDB

A Postgres-backed balance-maintenance engine (a ledger). Clients insert money
operations — singles or atomic groups — and a single elected leader validates and
confirms them in strict registration order, so an account's final balance never
breaks its configured min/max limits.

One Go binary runs as three roles:

```
balancedb api        # HTTP API (spec §10): insertion, synchronous waiting, queries
balancedb processor  # the single-leader decision loop (spec §7–§8)
balancedb migrate    # run migrations and exit (a CI/CD deploy gate)
```

The authorities for behavior and structure are `docs/architecture-spec.md` (what
the system does) and `docs/plan.md` (how it is built); deviations are recorded as
ADRs in `docs/decisions/`. Start with `CLAUDE.md` for orientation and the
inviolable rules.

## Running locally

```sh
export BALANCEDB_DATABASE_URL=postgres://balancedb:balancedb@127.0.0.1:5432/balancedb?sslmode=disable
go run ./cmd/balancedb processor &   # elects itself leader, drains work
go run ./cmd/balancedb api           # serves the API on :8080, docs at /docs
```

Both roles run migrations on boot (idempotent, advisory-lock guarded) unless
`BALANCEDB_MIGRATE_ON_START=false`. Deployment configuration is `BALANCEDB_*`
environment (see plan §0); behavioral knobs (`lease_ttl_ms`, `batch_size`, …) live
in the DB `config` table and are hot-reloaded each cycle.

## Observability (spec §13)

Every process — `api` and `processor` alike — serves an operational HTTP surface on
`BALANCEDB_METRICS_ADDR` (default `:9090`):

| Endpoint | Purpose |
|---|---|
| `GET /metrics` | Prometheus exposition of the metrics below (plus Go runtime + process collectors). |
| `GET /healthz` | Liveness: `200` while the process is serving. Never touches the DB. |
| `GET /readyz` | Readiness: `200` when the DB is reachable, `503` otherwise. For the processor the body also reports `leader` — **information only; a standby is healthy** (the standby must stay in the load-balancer so it can take over). |

Requests to the API are wrapped in **panic-recovery** (a handler panic becomes a
logged `500`, never a crashed process) and **request logging** (method, path,
status, latency; `5xx` logged at warn). Slow SQL is logged by a pgx query
**tracer** at `obs.DefaultSlowQueryThreshold` (200 ms) and counted in
`balancedb_slow_queries_total`.

### Metrics

All metrics are prefixed `balancedb_`. The rightmost column ties each to its
authority.

| Metric | Type | What it measures | Rationale |
|---|---|---|---|
| `queue_depth` | gauge | PENDING operations awaiting a decision. | §13 — the system's only latency promise is "~one cycle when healthy"; backlog is its health signal. |
| `oldest_pending_age_seconds` | gauge | Age of the oldest PENDING op, on the **DB clock** (`now() - registered_at`). | §13 — the other half of queue health; a growing oldest-age means the leader is behind. |
| `loop_busy_seconds_total` / `loop_wall_seconds_total` | counters | Leader time spent draining vs. total cycle wall-clock. | §13 loop utilization ρ = `rate(loop_busy_seconds_total) / rate(loop_wall_seconds_total)` — the queue-latency predictor and the cell-splitting trigger. |
| `snapshot_rows_touched` | histogram | Balance-snapshot rows written per confirmed operation (1 = newest day; more = a backdating cascade). | §13 — measures the backdating workload in production (spec §8.4 makes backdating O(days spanned)). |
| `leadership_changes_total` | counter | Lease ownership transitions (gained or lost) this instance observed. | §13 — `rate()*3600` = changes/hour; a high rate is lease flapping (tuning or infrastructure). |
| `leader` | gauge | `1` if this instance currently holds the lease, else `0`. | Companion to leadership changes: which instance is the leader right now. |
| `decisions_total{kind,outcome}` | counter | Committed terminal decisions: `kind` ∈ single\|group, `outcome` ∈ confirmed\|invalid\|committed\|rejected. | §13 decision counters by outcome. Counted **only on a committed batch**, so a guard-miss rollback never inflates them. |
| `doorbell_wakeup_lag_seconds` | histogram | Insert-commit → leader-pickup lag per operation, on the **DB clock** (`now() - registered_at` at fetch). | **ADR-0002** — the measured doorbell latency win: tens of ms on an idle system, degrading gracefully into queue latency under load. |
| `api_wait_seconds{source,result}` | histogram | Synchronous-insert wait latency (register → return). `source` ∈ immediate\|notify\|poll\|timeout, `result` ∈ decided\|pending. | **ADR-0002** / §13 NOTIFY→outcome lag — API wait health. The `notify` source is the low-latency fast path; `poll` is the durability fallback firing. |
| `slow_queries_total` | counter | SQL queries over the slow-query threshold (also logged). | §13 DB-write health — surfaces queries approaching the cell's DB ceiling. |
| `pool_*` | gauges + counters | pgxpool stats read live on each scrape: `acquired_conns`, `idle_conns`, `constructing_conns`, `total_conns`, `max_conns`, `acquire_count_total`, `acquire_duration_seconds_total`, `empty_acquire_count_total`, `canceled_acquire_count_total`, `new_conns_count_total`. | plan §M9 — connection-pool saturation is an early warning before the DB ceiling. |

**Clock discipline.** Latency measurements that compare two ledger events —
`oldest_pending_age_seconds` and `doorbell_wakeup_lag_seconds` — take *both*
endpoints from the DB clock inside one SQL statement, never mixing in the process
clock (the same rule the lease and ordering paths follow). Within-process
durations (loop utilization, request latency, the wait histogram, slow-query
timing) are measured with the process clock, which is correct for an elapsed span
that begins and ends in the same process.

**Not app metrics.** Spec §13 also lists raw `DB row-writes/s`, `WAL throughput`,
and `fsync latency`. Those are Postgres-server metrics; scrape them from a Postgres
exporter alongside this app's `/metrics`, not from the app itself (plan §M9 scopes
the app to the set above plus pgxpool stats).

### Watching it locally

Run the load generator against a live `api` node and scrape the `processor`'s
`/metrics` to watch ρ, queue depth, and batching behave under load:

```sh
make loadgen ARGS="-rate 200 -duration 30s -accounts 20"
# groups instead of singles:
make loadgen ARGS="-rate 80 -group-size 3"
curl -s localhost:9090/metrics | grep balancedb_
```

`cmd/loadgen` is a developer tool only (not part of any deployed binary); it inserts
small positive amounts against unbounded accounts, so operations are always
accepted — the point is throughput and timing, not validation.

## Development

The `Makefile` is the interface:

| Target | What it does |
|---|---|
| `make lint` | gofumpt check + `go vet` + CLAUDE.md/AGENTS.md sync. |
| `make test` | unit tests (no external dependencies). |
| `make itest` | integration tests against `TEST_DATABASE_URL` (each test in a throwaway schema). |
| `make simtest` | deterministic simulation harness against `TEST_DATABASE_URL` (spec §15). |
| `make openapi` | regenerate `api/openapi.yaml` from the code, fail on drift. |
| `make loadgen` | run the local load generator (see above). |

This box has no toolchain on the default PATH; see `docs/handoff.md` for the
bootstrap (`export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$HOME/bin:$PATH"`).
