# Handoff notes

Running log left by each milestone agent for the next. Newest section at the
bottom. See `tasks/_common.md` for the protocol.

## OPEN QUESTIONS

_(none)_

## M1 — 2026-08-22

Built:
- Repo scaffold: `go mod init github.com/magnomp/balancedb` (Go 1.25), `.gitignore`,
  `Makefile` (`lint`, `test`, `itest` [stub until M2], `fmt`, `tools`).
- `internal/config` — parses/validates all `BALANCEDB_*` vars (plan §0 table),
  schema-name regex `^[a-z_][a-z0-9_]{0,62}$`, postgres URL validation, and
  `RedactedDSN()` (password → `xxxxx`) for the startup log. `LoadFrom(getenv)`
  seam keeps tests off the process environment.
- `internal/db` — `Connect()` builds a pgxpool, sets `search_path` per connection
  in `AfterConnect` via `pgx.Identifier{schema}.Sanitize()` (quoted, never
  interpolated), pings on boot with actionable error text. `WithTx()` helper
  (commit on success, rollback on error/panic).
- `cmd/balancedb/main.go` — role dispatch `api|processor|migrate`, slog setup
  (json/text, level from config), `signal.NotifyContext` on SIGTERM/SIGINT,
  graceful shutdown. api/processor boot → hold on ctx → clean exit; migrate is a
  stub (runner is M2).

Decisions: no ADRs. Only dependency added is `jackc/pgx/v5` (already mandated by
plan §0, so no ADR needed). No deviations from spec/plan.

Deferred/known issues:
- `make itest` is a stub — no DB integration tests until the migration/throwaway-
  schema harness lands (M2 + the M10 test-infra note).
- `make lint` is gofumpt-check + `go vet` only. golangci-lint config is planned
  for M13; the CLAUDE.md/AGENTS.md sync check is M1.5.
- `search_path` is set to a schema that does not exist until M2 migrations run.
  Postgres accepts this (resolution is lazy); `Ping` does not touch schema
  objects, so boot succeeds against an empty database. Fine as-is.

Notes for next agents (environment):
- **This box had no `go`, `make`, or `gofumpt` preinstalled and no passwordless
  sudo.** I installed Go 1.25 to `$HOME/sdk/go`, `gofumpt` to `$HOME/go/bin`, and
  extracted `make` to `$HOME/bin` (from the `make` .deb). Put these on PATH before
  running the Makefile:
  `export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$HOME/bin:$PATH"`.
  `make tools` installs gofumpt; a similar bootstrap may be needed for other tools.
- A `postgres:18` container `balancedb-pg` is running on `127.0.0.1:5432`
  (db/user/pass all `balancedb`). Manual boot check used:
  `BALANCEDB_DATABASE_URL=postgres://balancedb:balancedb@127.0.0.1:5432/balancedb?sslmode=disable`.
- Gates run green: `make lint`, `make test`. Manual: `migrate`, `api`, `processor`
  all boot, log the redacted startup line, and shut down cleanly.

## M1.5 — 2026-08-22

Built:
- `CLAUDE.md` (96 lines, ≤150 cap) — five sections per plan §M1.5: orientation,
  inviolables, code conventions, workflow, package map. Points at spec/plan/ADR
  sections; paraphrases nothing. Package map lists the full `internal/` target set
  with spec sections, marking unbuilt packages `(Mn)`.
- `AGENTS.md` — symlink → `CLAUDE.md` (sync by construction).
- ADR skeleton: `docs/decisions/README.md` (numbering rule + index),
  `template.md`, and `0001-custom-migration-runner.md` (records the plan §M2
  decision). Existing 0002/0003 integrated into the index, not renumbered.
- `doc.go` for `internal/config` and `internal/db` — package comment moved out of
  `config.go`/`db.go` into `doc.go` (single package comment per package), each
  stating responsibility + spec/plan section.
- `make check-docs` (wired into `make lint`) — verifies `AGENTS.md` is a symlink to
  `CLAUDE.md`, else byte-identical, else fails with an actionable message.

Decisions: ADR-0001 written (custom migration runner; retroactive record of the
plan §M2 call, no behavior change). No new dependencies. No deviations from
spec/plan.

Deferred/known issues:
- CLAUDE.md's workflow section references `make simtest` (M10) and a real
  `make itest` (M2) that are still stubs/absent — intentional; the map/workflow
  describe the stable target, not today's partial state.

Notes for next agents:
- CLAUDE.md is now authoritative and supersedes the seed "Conventions" in
  `tasks/_common.md`. Update CLAUDE.md in the SAME commit as any change that alters
  a convention it states, and keep it ≤150 lines.
- When you add an `internal/` package, add its `doc.go` (responsibility + spec
  section) and flip its map line in CLAUDE.md from `(Mn)` to **built**.
- Cold-session spot-check passed: CLAUDE.md alone answers "the three guards + where"
  (§7.2, every processor tx) and "how to run integration tests" (`make itest` +
  `TEST_DATABASE_URL`).
- Gates green: `make lint` (incl. `check-docs`), `make test`. PATH bootstrap from
  the M1 note above still applies on this box.

## M2 — 2026-08-22

Built:
- `migrations/0001_init.sql` — full spec §5.2 DDL verbatim (accounts, transactions,
  operations, balance_snapshots, leader_lease, config, all indexes) + seed rows for
  `leader_lease` and `config` (`INSERT ... ON CONFLICT DO NOTHING`; column defaults
  fill every behavioral value — no env-seeding needed, there are no env vars for the
  config-table knobs). Unqualified names; the runner sets `search_path` first.
- `migrations/` asset package (`migrations.go`, `package migrations`, `//go:embed
  *.sql` → `migrations.FS`). See note below on why the embed lives here.
- `internal/migrate` — the runner (`Run(ctx, databaseURL, schema) (applied []int,
  err error)`): dedicated connection → set search_path → advisory lock → CREATE
  SCHEMA → per-schema `schema_migrations` → apply each pending embedded migration in
  its own tx (recording the version in the same tx). Returns versions applied this
  call (empty on a no-op boot). Unit test covers filename parsing/ordering (no DB).
- `internal/dbtest` — the throwaway-schema integration harness used by every later
  milestone: `URL(t)` (skip if `TEST_DATABASE_URL` unset), `RandomSchema(t)`,
  `NewSchema(t) *pgxpool.Pool` (fresh migrated schema, auto drop+close via
  `t.Cleanup`), `DropSchema(ctx, url, schema)`. Normal (untagged) package so `make
  lint` vets it.
- `internal/migrate/migrate_itest_test.go` (build tag `itest`) — creates-objects +
  idempotent-second-boot, parallel-migrators race (8 goroutines, exactly-once
  apply), two-schemas-one-database independence.
- `cmd/balancedb/main.go` — migrations wired into boot: `migrate` role always runs
  them then exits; `api`/`processor` run them on boot unless
  `BALANCEDB_MIGRATE_ON_START=false`. Runner uses its own connection, so it runs
  before the app pool is built.
- `Makefile` — real `itest` target: requires `TEST_DATABASE_URL` (fails fast if
  unset), runs `go test -tags itest ./...`. `make test` stays DB-free (integration
  files are behind the `itest` tag).

Decisions:
- **ADR-0004** — reordered plan §M2 steps 2/3: take the advisory lock BEFORE
  `CREATE SCHEMA`. `CREATE SCHEMA IF NOT EXISTS` is not concurrency-safe in Postgres
  (racing boots → 23505 on `pg_namespace_nspname_index`); the lock exists precisely
  to serialize concurrent boots, so schema creation must sit inside it. The lock key
  is the schema *name* (a string) and needs no schema object, so lock-first is
  sound. Caught by the parallel-migrator test before the fix. No G1–G6 impact.
- No new dependencies. The race test uses `sync.WaitGroup` (not
  `golang.org/x/sync/errgroup`, which plan §M2 mentions only illustratively) to
  avoid adding a dependency.

Deferred/known issues:
- None blocking. `make simtest` still absent (M10).

Notes for next agents:
- **go:embed placement.** go:embed cannot reach across directories (`../migrations`),
  so the repo-root `migrations/` dir (per plan §0 layout) carries its own tiny
  `package migrations` with `//go:embed *.sql`; `internal/migrate` imports it. This
  keeps migrations at the root as diagrammed while embedding cleanly. To add a
  migration: drop `NNNN_name.sql` into `migrations/` (numeric prefix, unique,
  positive, unqualified SQL) — the runner discovers and orders it automatically.
- **Use `dbtest.NewSchema(t)`** for any DB-touching test from M3 on; put such files
  behind `//go:build itest` so `make test` stays DB-free and `make itest` runs them.
- The `migrate` runner opens its own connection and does NOT touch the app pool, so
  it is safe to call before `db.Connect`. It is safe to call concurrently.
- `migrate` package map line in CLAUDE.md flipped `(M2)` → **built**.

## M3 — 2026-08-22

Built:
- `internal/model` (now **built**): domain model per spec §5. Row types (`Account`,
  `Transaction`, `Operation`, `BalanceSnapshot`, `Config`) mirroring the schema;
  status vocabularies (`OpStatus`, `TxStatus`); reason codes (`ReasonLimitViolated`,
  `LimitSide`, and the `Rejection` detail struct with `Marshal`/`ParseRejection`
  for the reason columns); `ParseAmount(json.Number) (int64,error)` — THE single
  money entry point, strict (rejects decimals/exponents/whitespace, accepts 0 so the
  DB CHECK stays the authority); `HashPayload([]CanonicalOp)` — canonical JSON (UTC-
  normalised effective_at, struct encoding) → SHA-256 for idempotency.
- `internal/api` insertion core (**built (M3)**; HTTP is still M7–M8): `Insert(ctx,
  tx, req)` as a pure function over a `pgx.Tx` (spec §10.1). Single vs group branch;
  on-demand account upsert by `(owner_id, external_id)`; atomic multi-leg group
  insert with an `op_count` cross-check; Stripe-model idempotency; `payload_hash`
  conflict → `ErrPayloadConflict`; `max_group_size` read from the `config` table;
  one-owner-per-group (sharding invariant, spec §2) enforced here and only here.
  Fresh inserts ring `NOTIFY work_available` inside the tx (ADR-0002); replays do
  not. Sentinel errors for every rejection so M7 can map status codes without string
  matching (`ErrNoOperations`, `ErrInvalidIdempotencyKey`, `ErrMixedOwners`,
  `ErrGroupTooLarge`, `ErrPayloadConflict`, `ErrZeroAmount`).
- `internal/api/insert_itest_test.go` (build tag `itest`, via `dbtest.NewSchema`):
  all plan §M3 cases — single, group, idempotent replay (single + group) returns
  original ids, same-key/different-payload conflict, mixed-owner rejection, group-size
  cap, amount=0 rejected by CHECK, doorbell fires on fresh insert, replay commits
  cleanly with no new rows.
- `simtest/` started (plan §M10 says grow it from M3): `Model`, an in-memory
  sequential reference model that reproduces the inserter's insert-level outcomes
  (new / replay / conflict, guards) and assigns ids like identity columns. First §15
  property test asserts G6 — idempotent replays never duplicate and never grow the op
  count, and same-key/different-payload retries conflict and change nothing; prints
  its seed on failure. Runs under `make test` (pure, no DB); a dedicated
  `make simtest` target stays deferred to M10 per CLAUDE.md.

Decisions:
- **ADR-0005** — insertion uses `ON CONFLICT DO NOTHING RETURNING` for the
  probe-and-insert (avoids 23505 poisoning the single insert transaction, so no
  SAVEPOINTs) and rings the doorbell on fresh inserts only (a replay creates no
  PENDING work; the doorbell is a hint, ADR-0002). Same DO-NOTHING idiom for account
  upsert so an existing account's row is never rewritten (no MVCC churn / no lock
  contention with the processor's version CAS). No new dependencies (pgx/pgconn only).

Deferred/known issues:
- None blocking. `op_count` cross-check on insert confirms all legs landed; the
  processor-side load-and-cross-check for partial groups is M6.

Notes for next agents:
- **Doorbell channel is global, not schema-scoped.** LISTEN/NOTIFY channels are
  per-database, so `NOTIFY work_available` (literal, per ADR-0002) crosses schemas:
  if several cells share one Postgres, a processor for schema A is woken by inserts
  to schema B. Harmless (the doorbell is a hint; the leader selects only its own
  schema's PENDING work `ORDER BY id`), but M5 (the LISTEN side) may want to
  schema-scope the channel (e.g. `work_available_<schema>` via `pg_notify`) to avoid
  spurious cross-cell wakeups. Left literal here on purpose; flag for M5, not a
  blocker. In `dbtest` (many schemas per DB) this is why the doorbell test asserts a
  notification *arrives* rather than asserting none arrives on replay.
- The insert core takes an already-parsed `int64` amount; M7's HTTP layer must decode
  the body with `json.Decoder.UseNumber()` and call `model.ParseAmount` — that is the
  only sanctioned path from JSON to money.
- Idempotency keys are validated as canonical UUIDs in Go (`ErrInvalidIdempotencyKey`)
  before the DB sees them; SQL casts the key with `$n::uuid`.
- `simtest` imports `internal/api` for the shared request/result types; M10 will feed
  the same `api.InsertRequest` schedules to both the DB and the reference model.
