# Operating BalanceDB

Deployment, configuration, roles and observability reference for the `balancedb`
binary. The [README](../README.md) is the overview; `architecture-spec.md` and the
ADRs in `decisions/` are the authorities this page summarises.

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

## Deployment (Docker + compose)

The repository builds a **single, self-contained image** (multi-stage → static
binary on `gcr.io/distroless/static-debian12:nonroot` — no shell, non-root, and the
schema migrations are embedded via `go:embed`, so the image needs nothing but a
Postgres to point at):

```sh
make image                 # docker build -t balancedb:local .
```

The root `docker-compose.yml` is a **prod-like reference deployment**: one
`postgres:18`, a one-shot `migrate` gate, one `api`, and two `processor`s (one
active leader, one hot standby). Every BalanceDB service runs the *same* image and
differs only by its command (the role) and `BALANCEDB_*` environment.

```sh
docker compose up --build          # or: make smoke  (scripted end-to-end test)
```

| Service | Role | Host ports | Notes |
|---|---|---|---|
| `postgres` | — | `55432→5432` | Bundled Postgres 18. Published on 55432 to avoid colliding with a 5432 already in use; services reach it as `postgres:5432`. |
| `migrate` | `migrate` | — | Deploy gate: applies the embedded migrations and exits 0. The long-running roles start only after it succeeds. |
| `api` | `api` | `8080` (HTTP+`/docs`), `9090` (health/metrics) | `BALANCEDB_MIGRATE_ON_START=false` — the gate already migrated. |
| `processor` | `processor` | `9091→9090` | Leader-or-standby; the health body reports `leader`. |
| `processor-standby` | `processor` | `9092→9090` | Identical replica; takes over within `lease_ttl_ms` if the leader dies. |

Each role's health surface (`/healthz`, `/readyz`, `/metrics`) binds `:9090`
*inside* its container (isolated network namespaces, so no clash); the compose file
publishes them on distinct host ports. The ledger endpoints (spec §10) are on the
`api` container's `:8080`.

Exercising it once up (owner scope is the `X-Owner-Id` header, §2):

```sh
# Insert a 2-leg atomic group and wait synchronously for the decision (200 = decided):
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"account":"alice","amount":-100,"effective_at":"2026-01-01T00:00:00Z"},
      {"account":"bob",  "amount": 100,"effective_at":"2026-01-01T00:00:00Z"}
    ], "wait_ms": 10000 }'

curl -s localhost:8080/accounts/alice/balance -H 'X-Owner-Id: 1'   # {"balance":-100,...}
```

`make smoke` scripts exactly this plus a failover check: it kills the active
processor container and asserts the standby acquires leadership within the lease
TTL and resumes draining work. It is self-contained (builds, runs, tears down); set
`KEEP_UP=1` to leave the stack up for inspection. **M13 (CI) reuses this target.**

## Configuration contract

Deployment config is `BALANCEDB_*` environment, parsed once at boot; the process
refuses to start on any invalid value. *Behavioral* knobs (`lease_ttl_ms`,
`batch_size`, `api_max_wait_ms`, …) are **not** env — they live in the DB `config`
table (spec §5.2) and are hot-reloaded every cycle (defaults are seeded by the
first migration).

| Var | Required | Default | Meaning |
|---|---|---|---|
| `BALANCEDB_DATABASE_URL` | yes | — | Postgres URL/DSN (`postgres://…`, supports `sslmode` etc.). |
| `BALANCEDB_SCHEMA` | no | `balancedb` | Target schema; created if absent. Selects the cell (see below). |
| `BALANCEDB_HTTP_ADDR` | no | `:8080` | API listen address (`api` role). |
| `BALANCEDB_METRICS_ADDR` | no | `:9090` | Health + Prometheus endpoints (both roles). |
| `BALANCEDB_LOG_LEVEL` | no | `info` | slog level (`debug`\|`info`\|`warn`\|`error`). |
| `BALANCEDB_LOG_FORMAT` | no | `json` | `json` \| `text`. |
| `BALANCEDB_MIGRATE_ON_START` | no | `true` | Set `false` when using the explicit `migrate` role as a deploy gate. |
| `BALANCEDB_POOL_MAX_CONNS` | no | `10` | pgxpool size. |

Behavioral knobs — the single `config` row, changed with plain SQL (`UPDATE config
SET …;`) and picked up on the next cycle/request without a restart:

| Column | Default | Meaning |
|---|---|---|
| `lease_ttl_ms` | `15000` | Leader lease TTL; failover bound (spec §7.1). |
| `loop_interval_ms` | `1000` | Processor polling interval (the doorbell wakes it earlier, ADR-0002). |
| `batch_size` | `200` | Decisions per DB commit (spec §8.5). |
| `max_group_size` | `10` | Maximum items per `POST /transactions` unit — new operations, edits and deletes together. |
| `api_max_wait_ms` | `30000` | Cap on `wait_ms` for synchronous waits (spec §10.1). |
| `allow_edits` | `true` | `false` refuses **new** edit registrations (`403` / `ErrEditsDisabled`) and keeps the cell on the reversal-only contract; already registered edits are still decided (ADR-0010). Read per request by the insertion core; the processor never checks it. |
| `allow_deletes` | `true` | `false` refuses **new** delete registrations (`403` / `ErrDeletesDisabled`), independently of `allow_edits`; already registered deletes are still decided (ADR-0011). Same read pattern as `allow_edits`. |

## Roles

- **`api`** — serves the HTTP API (spec §10). Stateless; scale horizontally behind a
  load balancer. Each instance keeps its own `LISTEN outcomes` connection for the
  synchronous-wait path (ADR-0002).
- **`processor`** — the single-leader decision loop (spec §7–§8). Run **two or more**;
  exactly one holds the leader lease at a time (spec §7.1) and the rest idle as hot
  standbys. Keep every standby in service — on leader death a standby acquires the
  lease within `lease_ttl_ms` and continues selecting visible committed work in ID order (ADR-0008). The three
  processing guards (spec §7.2) make a stale ex-leader's late writes no-ops, so
  failover is safe even if the old leader is only *slow*, not dead.
- **`migrate`** — applies the embedded migrations and exits. Use it as a CI/CD gate
  (see upgrades). The runner is advisory-lock guarded, so running it concurrently
  (or alongside `MIGRATE_ON_START=true` roles) is safe and idempotent.

## Multiple cells / environments in one Postgres

`BALANCEDB_SCHEMA` selects an **independent installation** inside the same database:
all objects are unqualified and resolved through `search_path` (plan §0), and each
schema carries its own migration history. Point one cell's api+processors at
`BALANCEDB_SCHEMA=cell_a` and another's at `cell_b` (or `staging` / `prod`) against
the same Postgres and they share nothing — separate accounts, ledgers, leases, and
config. (Note: the LISTEN/NOTIFY doorbell/outcome channels are per-*database*, so
cells sharing a database may receive each other's wakeups — harmless hints; ordering
and correctness come from each cell's own `ORDER BY id` work select, ADR-0002.)

## Upgrade procedure

The image is self-contained and forward-only (migrations never edit an applied file;
a rollback is a new migration). Two supported paths:

1. **Migrate-on-boot (default).** Deploy the new image; every `api`/`processor`
   runs the embedded migrations on boot under an advisory lock
   (`BALANCEDB_MIGRATE_ON_START=true`), so concurrent rollout is safe and the first
   one to win the lock applies, the rest no-op.
2. **Gated migrate (recommended for controlled rollouts).** Set
   `BALANCEDB_MIGRATE_ON_START=false` on the long-running roles and run the new
   image once as the **`migrate`** role first (this is what the compose `migrate`
   service does). It applies migrations and exits; only then do the api/processor
   containers start. This makes "schema is up to date" an explicit, observable gate
   in your pipeline rather than a boot side effect.

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
| `decisions_total{kind,outcome}` | counter | Committed terminal decisions: `kind` ∈ single\|group\|edit\|delete, `outcome` ∈ confirmed\|invalid\|committed\|rejected\|applied (`edit` and `delete` × applied\|invalid). | §13 decision counters by outcome; the `edit` and `delete` kinds are the adoption/rejection mix of each (ADR-0010, ADR-0011). Counted **only on a committed batch**, so a guard-miss rollback never inflates them. |
| `edit_deferrals_total` | counter | Units (a single edit or delete, or a whole group) skipped because an edit or delete target was still PENDING; decided on a later cycle once the target is. | §13 / ADR-0010 / ADR-0011 — one shared counter; zero is the norm; a sustained rate means overlapping insertion transactions (ADR-0008). Never blocks the work behind it. |
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

## Editing and deleting operations

Edits (ADR-0010) and deletes (ADR-0011) are registrations decided by the leader
like inserts. The full HTTP contract with examples is in
[`operations-editing.md`](operations-editing.md).
