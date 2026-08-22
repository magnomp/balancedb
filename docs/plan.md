# BalanceDB — Go Implementation Plan

Target: a single Go codebase producing one Docker image that can run as `api`, `processor`, or `migrate`, configured entirely by environment variables, self-migrating its schema (with a configurable schema name) on startup, plus a devcontainer for maintenance work.

---

## 0. Global decisions (apply to every milestone)

**One binary, three roles.** `balancedb api`, `balancedb processor`, `balancedb migrate`. One image, role selected by command/args. API and processor both run migrations on boot (idempotent, advisory-lock guarded — see M2), so a plain `docker run` against an empty database works with no separate migration step; `migrate` exists for CI/CD pipelines that want an explicit gate.

**Stack.**
- Go 1.25+ (toolchain pinned in `go.mod`; current Huma requires it)
- PostgreSQL 18 (latest stable major; pinned as `postgres:18` in compose/devcontainer so minor updates flow automatically). Nothing in the design requires 18-specific features — the SQL is deliberately plain — so managed offerings still on 16/17 remain compatible; 18 is simply the current default with the longest support runway (5-year EOL policy).
- `jackc/pgx/v5` + `pgxpool` — no ORM, no query builder. All SQL is hand-written, matching the spec's SQL-first design.
- HTTP API: **Huma v2** on top of chi/stdlib mux (ADR-0003, code-first). Handlers are typed operations (`func(ctx, *Input) (*Output, error)`); Huma reflects over the real request/response structs to produce OpenAPI 3.1 + JSON Schema, request validation, and RFC 7807 errors, and serves interactive docs at `/docs` and the spec at `/openapi.yaml`. The server is the authority; the generated spec is exported to `api/openapi.yaml` and committed (CI regenerates, fails on diff, and runs an `oasdiff` breaking-change check). Huma is confined to the API layer — the processor never touches it.
- `log/slog` for structured logging; `prometheus/client_golang` for metrics (§13).
- No other significant dependencies. Boring on purpose.

**Money is `int64` everywhere.** JSON amounts are decoded via `json.Number` → strict integer parse (reject floats, reject strings with decimals). One helper owns this conversion; nothing else touches it.

**Schema qualification strategy.** All SQL is written *unqualified*. Every pooled connection sets `search_path = <schema>` in `AfterConnect` (schema name validated at boot against `^[a-z_][a-z0-9_]{0,62}$` and applied with a quoted identifier — never string-interpolated from raw input). This keeps every query, index, and migration file clean while making the schema name a pure deployment concern.

**Repository layout.**

```
balancedb/
├── cmd/balancedb/main.go          # role dispatch: api | processor | migrate
├── internal/
│   ├── config/                    # env parsing + validation (M1)
│   ├── db/                        # pool setup, search_path, tx helpers (M1)
│   ├── migrate/                   # embedded migrations + runner (M2)
│   ├── model/                     # row types, status enums, reason codes (M3)
│   ├── lease/                     # leader lease acquire/renew/release (M4)
│   ├── processor/                 # main loop, single/group processing, guards, batching (M5–M6)
│   ├── snapshot/                  # snapshot upsert + cascade update (M5)
│   ├── api/                       # HTTP handlers, insertion, waiting, queries (M7–M8)
│   ├── notify/                    # LISTEN/NOTIFY plumbing: outcome fan-out (api, M8) + work doorbell (M3/M5)
│   └── obs/                       # metrics registry, health endpoints (M9)
├── migrations/                    # 0001_init.sql, ... (embedded via go:embed)
├── simtest/                       # deterministic simulation harness (M10)
├── Dockerfile
├── docker-compose.yml             # local prod-like run: app + postgres
├── .devcontainer/
│   ├── devcontainer.json
│   └── docker-compose.yml        # dev service + postgres service
├── Makefile
├── CLAUDE.md                      # AI maintenance instructions (M1.5); AGENTS.md symlinks to it
├── docs/
│   ├── architecture-spec.md       # the BalanceDB spec, committed verbatim as the authority
│   └── decisions/                 # short ADRs for deviations/refinements of the spec
└── README.md
```

**Configuration (env vars).** All prefixed `BALANCEDB_`. Parsed once at boot into a typed struct; the process refuses to start on any invalid value. Note the split: *deployment* config comes from env; *behavioral* config (`lease_ttl_ms`, `batch_size`, …) lives in the DB `config` table per spec §5.2 and is hot-reloaded every cycle. Env only seeds the config row on first migration.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `BALANCEDB_DATABASE_URL` | yes | — | Postgres URL/DSN (standard `postgres://…` form; supports sslmode etc.) |
| `BALANCEDB_SCHEMA` | no | `balancedb` | Target schema; created if absent |
| `BALANCEDB_HTTP_ADDR` | no | `:8080` | API listen address (api role) |
| `BALANCEDB_METRICS_ADDR` | no | `:9090` | Prometheus + health endpoints (both roles) |
| `BALANCEDB_LOG_LEVEL` | no | `info` | slog level |
| `BALANCEDB_LOG_FORMAT` | no | `json` | `json` \| `text` |
| `BALANCEDB_MIGRATE_ON_START` | no | `true` | Set `false` when using the explicit `migrate` role as a deploy gate |
| `BALANCEDB_POOL_MAX_CONNS` | no | `10` | pgxpool size |

---

## M1 — Skeleton: config, DB pool, role dispatch

**Scope.** Repo scaffold, `cmd/balancedb` with role dispatch, `internal/config` (env parsing, schema-name validation, URL validation), `internal/db` (pgxpool with `AfterConnect` setting `search_path`, ping-on-boot with clear error text, small `WithTx` helper). Graceful shutdown wiring (context cancelled on SIGTERM/SIGINT) from day one — the processor loop and API server in later milestones just consume the context.

**Done when:** `balancedb api` and `balancedb processor` boot against a reachable Postgres, log a structured startup line (role, schema, redacted DSN), and shut down cleanly; unit tests cover config parsing edge cases (bad schema names, missing URL).

## M1.5 — AI maintenance instructions (CLAUDE.md + docs)

**Scope.** Populate the repository with the instruction set that future AI sessions (and humans) load before touching code. This exists *before* the first real feature so every subsequent milestone is written under, and validates, these rules.

- **`docs/architecture-spec.md`** — the BalanceDB specification committed verbatim. It is the single authority; CLAUDE.md points at it rather than paraphrasing it (paraphrases drift).
- **`docs/decisions/`** — one short ADR per deviation or refinement of the spec, numbered, ~half a page each (e.g., `0001-custom-migration-runner.md`, `0002-poll-vs-notify.md` once that call is made). The rule: the spec plus the ADR log is always the complete current truth.
- **`CLAUDE.md`** at repo root (with `AGENTS.md` as a symlink for other tools), containing, in order of importance:
  1. **Orientation** — what BalanceDB is in three sentences; read `docs/architecture-spec.md` §2–§4 before any non-trivial change; the consistency contract G1–G6 is the product.
  2. **Inviolables** — the rules no change may break, stated imperatively: all three processing guards (§7.2) stay, with rowcount checks, in every processor transaction; operations are never mutated after their facts are set; money is `int64` minor units end to end — no floats, no `float64` JSON decoding, all amount parsing goes through the one designated helper; time comparisons for lease/ordering use the DB clock, never `time.Now()`; no atomic work across owners (stop and write an ADR instead); `INVALID`/`REJECTED` are terminal.
  3. **Code conventions** — SQL as `const` at its single call site or in `migrations/`, unqualified (search_path owns the schema), no query builders/ORM; migrations forward-only, never edit an applied file; new behavior knobs go in the `config` table, deployment knobs in env; errors handled explicitly at every SQL call, guard misses roll back and continue; dependency additions require an ADR.
  4. **Workflow** — the Makefile is the interface: `make test` / `make itest` / `make simtest` / `make lint` and what each requires; integration tests self-isolate in throwaway schemas via `TEST_DATABASE_URL`; the definition of done for any change includes a green `make simtest`, and changes to processor logic or validation must extend the simulation harness's reference model in the same PR.
  5. **Map** — one line per `internal/` package (mirroring the layout table above) so a session can navigate without a full-tree scan.
- **Per-package doc comments** — each `internal/` package gets a `doc.go` whose comment states its single responsibility and its spec section; CLAUDE.md's map links are these packages.

Maintenance rule for the file itself: CLAUDE.md is updated in the same commit as any change that alters a convention it states — a stale instruction file is worse than none. Keep it under ~150 lines; details live in the spec and ADRs, not in CLAUDE.md.

**Done when:** CLAUDE.md, `docs/architecture-spec.md`, and the ADR skeleton are committed; a cold AI session given only the repo can correctly answer "what are the three guards and where do they live" and "how do I run integration tests" from the instruction set alone (spot-check); `make lint` includes a check that `CLAUDE.md` and `AGENTS.md` stay in sync (symlink or content-identical).

## M2 — Migrations: on-the-fly schema creation + versioning

**Scope.** A small purpose-built runner in `internal/migrate` (~150 lines) instead of a third-party tool — the configurable-schema requirement makes this cleaner than bending golang-migrate:

1. Open a dedicated connection; `SET search_path` to the target schema position after step 2.
2. `CREATE SCHEMA IF NOT EXISTS <schema>` (quoted identifier).
3. Take `pg_advisory_lock(hashtext('balancedb:' || <schema>))` — serializes concurrent boots (api + processor + replicas all migrating at once) without inter-process coordination.
4. `CREATE TABLE IF NOT EXISTS schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())` *inside the target schema* — so several BalanceDB schemas can coexist in one database, each independently versioned.
5. Apply each embedded migration (`//go:embed migrations/*.sql`, ordered by numeric prefix) not yet in `schema_migrations`, each in its own transaction, recording the version in the same transaction.
6. Release the lock.

`0001_init.sql` is the full spec §5.2 DDL (accounts, transactions, operations, balance_snapshots, leader_lease, config, all indexes) plus seed rows for `leader_lease` and `config` (`INSERT … ON CONFLICT DO NOTHING`). Migrations are forward-only; no down migrations (immutable-ledger posture — a rollback is a new migration).

**Done when:** booting any role against an empty database creates the schema and all objects; booting two processes simultaneously is safe (test with `errgroup` spawning parallel migrators); a second boot is a no-op; a *different* `BALANCEDB_SCHEMA` in the same database creates a parallel, independent installation (integration test).

## M3 — Domain model + insertion core (no HTTP yet)

**Scope.** `internal/model`: row structs, status constants (`PENDING/CONFIRMED/INVALID`, `PENDING/COMMITTED/REJECTED`), machine-readable reason codes (`LIMIT_VIOLATED` + detail struct: account external id, limit side, shortfall). `internal/api/insert.go` as a pure function over a `pgx.Tx` (HTTP comes in M7): single vs group branch, account upsert by `(owner_id, external_id)` with `ON CONFLICT`, atomic multi-leg insert, `op_count` cross-check, payload hashing (canonical JSON → SHA-256), idempotency probe-and-insert logic (unique-violation → fetch existing → compare `payload_hash` → replay result or conflict error), `max_group_size` enforcement, one-owner-per-group enforcement (the sharding invariant — validated here and only here, per spec §2/§10.1). Each successful insert transaction ends with `NOTIFY work_available` — the processor doorbell (ADR-0002); payload-free, purely a wakeup hint, so it carries no correctness weight.

**Done when:** table-driven tests against a real Postgres (see M10 note on test infra) cover: single insert, group insert, idempotent replay returns original ids, same key + different payload → conflict error, group with mixed owners rejected, group size cap, `amount = 0` rejected by CHECK.

## M4 — Leader lease

**Scope.** `internal/lease`: instance UUID at boot; the spec's single-`UPDATE` acquire/renew (§7.1) with all time comparisons in SQL (`now()` — DB clock only, never `time.Now()` in comparisons); graceful release (`owner = NULL`) on shutdown; a tiny state surface for the processor (`IsLeader() bool` refreshed by the loop, not a background goroutine — the loop *is* the heartbeat, keeping the design single-threaded).

**Done when:** tests cover: empty lease acquired; held lease renewed; foreign unexpired lease not acquired; expired foreign lease taken over; graceful release; two competing instances never both hold the lease (loop 100 rounds).

## M5 — Processor: single operations + snapshots

**Scope.** `internal/processor` main loop skeleton: re-read `config` row → renew lease → if leader, fetch `PENDING` batch ordered by `id` → process → drain until an empty select. Idle wait is doorbell-driven (ADR-0002): the leader holds a dedicated `LISTEN work_available` connection and sleeps on it with deadline `min(loop_interval, time to next safe lease renewal)` — a doorbell wakes it early; the timed wakeup is the correctness path (a lost doorbell costs at most one interval). Notifications are coalesced (they carry no state; "work exists" is the only message) and ordering still comes exclusively from the `ORDER BY id` select, never from notification arrival — G3 untouched. Standbys don't listen; they poll for the lease on the normal interval. `process_single` in one DB transaction implementing §8.2 with all three guards from §7.2 verbatim (Guard 1 lease fence, Guard 2 conditional status flip with rowcount check, Guard 3 versioned account update); any guard miss → rollback → re-acquire → continue. `internal/snapshot`: upsert the op's UTC-day cumulative row, then the sparse cascade `UPDATE … WHERE day > :op_day`. Emit `NOTIFY outcomes, 'op:<id>'` inside the commit.

**Done when:** integration tests cover: accept path (balance, version bump, snapshot row, status, `confirmed_at`); reject path (INVALID + reason detail, no balance change); backdated op cascading through existing later snapshots; future-dated op entering final balance (N5); unbounded (`NULL`) limits; guard-miss scenarios (stale version → rollback and clean retry next cycle); doorbell wakeup (insert against an idle leader with a long `loop_interval` is decided in well under one interval) and doorbell loss (suppress the notification; the timed wakeup still decides it within one interval).

## M6 — Processor: groups + batching

**Scope.** `process_group` per §8.3: load all legs by `transaction_id`, cross-check `op_count`, net per account, validate every account, all-or-nothing flip of legs + transaction row, snapshots per leg, `NOTIFY 'tx:<id>'`. Skip-by-status for later legs of an already-decided group. Then §8.5 batching: pack up to `batch_size` decisions into one DB transaction with guards evaluated per decision; on any guard miss, rollback the whole batch and reprocess (idempotent by construction — prove it with a test that crashes mid-batch).

**Done when:** tests cover: multi-account group commit; one-leg-fails → whole group REJECTED with offending account + shortfall; net-per-account correctness (two legs on the same account); non-zero-sum group; registration-order determinism (G3: interleaved singles and groups decided at first-leg position — assert exact outcome sequence); batch crash/reprocess yields identical final state as unbatched sequential run.

## M7 — HTTP API: insertion + queries (code-first OpenAPI via Huma)

**Scope.** `internal/api` built on Huma v2 over chi (ADR-0003): each §10 endpoint is a registered typed operation whose request/response structs *are* the contract — Huma derives OpenAPI 3.1 from them, validates requests, and emits RFC 7807 errors. Endpoints: `POST /transactions` (validation, error envelope, `202` fire-and-forget path; `wait_ms` documented as strictly optional — `0`/omitted returns `202` immediately, outcomes queryable later), `GET /transactions/{id}`, `GET /operations/{id}`, `GET /accounts/{ext}/balance`, `?at=T` point-in-time projection (§9: last snapshot before T's day + same-day prefix), `GET /accounts/{ext}/statement` (keyset pagination over `idx_ops_timeline`, snapshot-seeded running balance), `POST /accounts`, `PUT /accounts/{ext}/limits` (§6 rules, version-guarded, retry-once-on-conflict then `409`). Machine-readable rejection payloads as typed schemas. Amounts are `int64` struct fields (schema: `integer/int64`) with minor-units + JS-precision warnings in field descriptions; body decoding rejects fractional numbers by type. Interactive docs at `/docs`, spec at `/openapi.yaml`, both served by Huma. Contract-as-artifact: `make openapi` exports the generated spec to `api/openapi.yaml` (committed); CI regenerates and fails on diff, and runs `oasdiff` against the previous version to flag breaking changes. Owner scoping: an `X-Owner-Id` header (or token claim — pluggable interface, single header impl for now) resolves the owner, declared as a header parameter on every operation; the directory is out of scope for a single-cell deployment (one cell = this database), but the routing seam is left as an interface so a directory can be added without touching handlers.

**Done when:** httptest-based integration tests cover every endpoint contract, including: point-in-time balance that violates limits (N2 visible), statement pagination across a snapshot-day boundary, limits update racing a processor confirmation (version conflict surfaces as `409`); `/docs` renders and can execute a request against a local instance; `api/openapi.yaml` is committed and `make openapi` is diff-clean; a deliberately malformed request (float amount, missing field) is rejected by Huma validation with a 7807 error, testing the contract enforcement path.

## M8 — Synchronous waiting: LISTEN/NOTIFY + poll fallback (decided: ADR-0002)

**Scope.** Waiting is strictly optional per contract — `wait_ms = 0` (or omitted) returns `202` immediately and the client queries the outcome later; this milestone only adds the opt-in wait path. `internal/notify`: one dedicated LISTEN connection per API process, a subscription registry (`tx:<id>` / `op:<id>` → waiter channels), reconnect-with-resubscribe on connection loss. `wait_ms > 0` path in `POST /transactions`: register waiter → check current status once (race close: outcome may have committed before LISTEN registration) → wait on channel with 1–2 s poll fallback ticker → `200` on outcome, `202` with current state on expiry (capped by `api_max_wait_ms` from the config row). Together with M5's doorbell this completes the bidirectional low-latency path of ADR-0002: insert wakes the leader; the deciding commit wakes the waiter.

**Done when:** tests cover: outcome arrives via NOTIFY (fast path); NOTIFY suppressed/dropped (kill the listen connection mid-wait) and the poll fallback still resolves; outcome decided *before* the wait registered (no lost wakeup); wait expiry returns `202` + PENDING; `wait_ms = 0` never touches the notify machinery; end-to-end doorbell + outcome-notify synchronous insert on an idle system resolves in well under one `loop_interval`.

## M9 — Observability + operational hardening

**Scope.** Prometheus metrics per spec §13: oldest-PENDING age, queue depth, loop utilization ρ, snapshot rows touched per confirmation, leadership changes, decision counters by outcome, NOTIFY→outcome lag, doorbell wakeup lag (insert commit → leader pickup; the ADR-0002 latency win, measured), plus pgxpool stats. `/healthz` (process up) and `/readyz` (DB reachable; for the processor: lease state included as info, not readiness — a standby is healthy). Panic-recovery middleware; request logging with latency; slow-query logging via pgx tracer. A `Makefile` target `make loadgen` with a small load generator (inserts at a configurable rate) to observe ρ and batching behavior locally.

**Done when:** metrics visible end-to-end in the devcontainer (`curl :9090/metrics` while loadgen runs); README documents each metric and its §13 rationale.

## M10 — Deterministic simulation testing (spec §15 — the big one)

**Scope.** `simtest/`: a seeded-PRNG harness that generates randomized schedules of: account creations with tight/loose/unbounded limits, singles, groups (mixed sizes, non-zero-sum, same-account multi-leg), aggressive backdating and future-dating (crossing snapshot days, exact-timestamp ties), reversals (including double-reversal attempts and reversal-after-reject retries), limit changes, idempotent retries — interleaved with fault injection: processor kill/restart, leader failover (spawn competing processor), zombie-leader simulation (hold a stale lease view and attempt writes — guards must catch), batch-boundary crashes.

Against every run, an in-memory **sequential reference model** replays the same inserts in registration order and computes expected outcomes and final balances. Assertions: G1 (final balance within limits, every account), G2 (no partially-applied group ever observable — checked by invariant queries between steps), G3 (outcome sequence identical to reference), G4 (timeline order stable), G6 (no duplicates). Every failure prints its seed; every seed reproduces exactly.

Test infra note (applies to M3–M9 too): integration tests run against the devcontainer's Postgres via `TEST_DATABASE_URL`, each test in a throwaway schema (`test_<random>`) created/dropped by a helper — cheap isolation, parallel-safe, and it doubles as a continuous test of the configurable-schema machinery.

**Done when:** 1,000+ seeded scenarios pass in CI in reasonable time; a deliberately introduced bug (e.g., skip Guard 3) is caught by the harness (mutation smoke test, done once manually and documented).

## M11 — Docker image + prod-like compose

**Scope.** Multi-stage `Dockerfile`:

```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /balancedb ./cmd/balancedb

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /balancedb /balancedb
ENTRYPOINT ["/balancedb"]
CMD ["api"]
```

Static binary, distroless, non-root, no shell. Migrations are embedded — the image is self-contained. Root `docker-compose.yml` demonstrating the intended deployment: one `postgres:18` service, one `api` service, one `processor` service (plus an optional second processor as standby), all configured purely by `BALANCEDB_*` env vars. README section: exact env contract, role semantics, how `BALANCEDB_SCHEMA` enables multiple cells or environments in one Postgres instance, upgrade procedure (deploy new image → it migrates on boot; or gate with the `migrate` role + `BALANCEDB_MIGRATE_ON_START=false`).

**Done when:** `docker compose up` from a clean checkout yields a working system: `curl` inserts a group, waits synchronously, reads balances; killing the active processor container fails over to the standby within the lease TTL.

## M12 — Devcontainer

**Scope.** `.devcontainer/docker-compose.yml`: a `dev` service (image `mcr.microsoft.com/devcontainers/go:1.25`, sleep-infinity, workspace mount) and a `postgres` service (`postgres:18`, healthcheck, named volume). `devcontainer.json`:

- `dockerComposeFile` + `service: dev`, `forwardPorts: [8080, 9090, 5432]`
- `containerEnv`: `BALANCEDB_DATABASE_URL=postgres://balancedb:balancedb@postgres:5432/balancedb?sslmode=disable`, `BALANCEDB_SCHEMA=balancedb`, `TEST_DATABASE_URL` (same URL — tests self-isolate via throwaway schemas), `BALANCEDB_LOG_FORMAT=text`
- Features: `docker-in-docker` (to build/test the production image from inside), `postgres-client` (psql for poking at state)
- `postCreateCommand`: `go mod download && make tools` (installs `golangci-lint`, `gofumpt`)
- VS Code customizations: Go extension, `gopls` staticcheck on, format-on-save, SQL syntax highlighting for `migrations/`

`Makefile` targets as the maintenance interface: `make run-api`, `make run-processor`, `make test` (unit), `make itest` (integration, requires DB), `make simtest`, `make lint`, `make image`, `make loadgen`, `make psql`.

**Done when:** "Reopen in Container" on a fresh clone → `make itest` passes with zero manual setup; `make run-processor` + `make run-api` in two terminals gives a working local system against the bundled Postgres.

## M13 — CI + release

**Scope.** GitHub Actions (or equivalent): lint → unit tests → integration + simulation tests against a Postgres service container → build image → smoke test (run compose, hit the API) → push image tagged by git SHA + semver on tags. `golangci-lint` config committed; `go vet` + `gofumpt` enforced.

**Done when:** a tagged commit produces a pullable image that passes the M11 compose smoke test.

---

## Sequencing and effort shape

Dependency chain: M1 → M1.5 → M2 → M3 → {M4, M7} → M5 → M6 → M8 → M9 → M10 hardening → M11 → M12 → M13. M12 (devcontainer) is worth pulling forward to right after M2 in practice — it makes every subsequent milestone's integration tests trivially runnable, and its throwaway-schema test helper is used from M3 onward. M10's harness skeleton should also start early (the reference model can be written against M3's inserter) and grow with each milestone rather than being bolted on at the end.

The bulk of the correctness-critical code is M5+M6 (~the processor) and M10 (~the proof). Everything else is deliberately thin plumbing around SQL that already appears verbatim in the spec.

## Maintenance conventions (for future sessions)

These seed CLAUDE.md in M1.5; once the repo exists, CLAUDE.md is the authoritative copy and this section is historical.

- Every SQL statement lives either in `migrations/` or as a `const` next to its single call site — greppable, no query builders, no string concatenation.
- Guard semantics (§7.2) are never "optimized away"; any change to processor transactions must keep all three guards and their rowcount checks.
- New behavioral knobs go in the `config` table (hot-reloaded), not env; new deployment concerns go in env.
- Schema changes: new numbered migration only; never edit an applied migration; forward-only.
- Any feature touching two owners atomically is out of contract (spec §2/§11) — stop and design, don't improvise.
