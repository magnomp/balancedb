# ADR-0004 — Advisory lock before CREATE SCHEMA in the migration runner

Status: accepted (M2, 2026-08-22)

## Context
Plan §M2 lists the runner's boot sequence as: (1) open a connection and set
`search_path`, (2) `CREATE SCHEMA IF NOT EXISTS <schema>`, (3) take
`pg_advisory_lock(hashtext('balancedb:' || <schema>))`, then create the version
table, apply migrations, and release the lock. The advisory lock's stated purpose
is to "serialize concurrent boots (api + processor + replicas all migrating at
once) without inter-process coordination."

With `CREATE SCHEMA` at step 2 — before the lock — schema creation sits *outside*
the critical section the lock is meant to protect. `CREATE SCHEMA IF NOT EXISTS`
is not concurrency-safe in PostgreSQL: two racing boots can both pass the internal
existence check and one then fails with `duplicate key value violates unique
constraint "pg_namespace_nspname_index"` (SQLSTATE 23505). The M2 parallel-migrator
test reproduces this deterministically. Options considered:

- **Tolerate 23505** on the `CREATE SCHEMA` and continue. Keeps the literal step
  order but swallows a specific error and leaves the lock covering only part of the
  boot — contrary to its documented intent.
- **Reorder: take the lock first, then create the schema.** The lock key is derived
  purely from the schema *name* (a string) and needs no schema object to exist, so
  nothing prevents acquiring it first. This puts schema creation, version-table
  creation, and migration application all inside one critical section.

## Decision
Reorder plan §M2 steps 2 and 3: acquire the advisory lock immediately after
setting `search_path`, and perform `CREATE SCHEMA IF NOT EXISTS` (and everything
after) while holding it. `search_path` may be set against a not-yet-existing schema
(resolution is lazy), so it can stay first. All other steps are unchanged.

## Consequences
- Concurrent boots of the *same* schema are fully serialized: exactly one creates
  the schema and applies migrations; the rest block on the lock and then find the
  work done. No error-swallowing.
- Concurrent boots of *different* schemas still run in parallel — the lock keys
  differ — so the two-schemas-one-database independence property is unaffected.
- Refines ADR-0001's runner description; no change to the migration set, the
  forward-only posture, or any spec §4 guarantee. **Invariants preserved:** none of
  G1–G6 touched — this is boot-time schema installation, not the processing path.
