# ADR-0001 — Custom migration runner (not a third-party tool)

Status: accepted (plan §M2 decision, recorded retroactively in M1.5, 2026-08-22)

## Context
BalanceDB installs into a *configurable* schema (`BALANCEDB_SCHEMA`), and several
independent installations may share one database, each versioned separately. Every
role (`api`, `processor`, `migrate`) migrates on boot, so concurrent migrators —
including replicas — must be safe without external coordination. Third-party runners
(e.g. golang-migrate) assume a fixed/`public` schema and their own advisory-lock and
version-table placement; bending them to per-schema versioning plus a boot-time
`search_path` dance costs more than it saves for the ~150 lines of SQL involved.

## Decision
Hand-write the runner in `internal/migrate`. On boot it: opens a dedicated
connection; `CREATE SCHEMA IF NOT EXISTS <schema>` (quoted identifier); takes
`pg_advisory_lock(hashtext('balancedb:' || <schema>))` to serialize concurrent
boots; ensures a `schema_migrations(version, applied_at)` table *inside the target
schema*; applies each embedded (`//go:embed migrations/*.sql`, numeric-prefix order)
migration not yet recorded, each in its own transaction that also records the
version; releases the lock. Migrations are forward-only; no down migrations
(immutable-ledger posture — a rollback is a new migration).

## Consequences
- No migration dependency to add or track; the runner is ours to keep small.
- Per-schema `schema_migrations` lets multiple BalanceDB installations coexist in one
  database, each independently versioned (integration-tested in M2).
- The advisory lock makes simultaneous boots (api + processor + replicas) safe with
  no inter-process coordination; a second boot is a no-op.
- `0001_init.sql` carries the full spec §5.2 DDL plus seed rows for `leader_lease`
  and `config` (`INSERT … ON CONFLICT DO NOTHING`).
- The "no unnecessary dependencies" default (plan §0) is upheld, not bent.
