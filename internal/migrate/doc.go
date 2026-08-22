// Package migrate is BalanceDB's purpose-built, forward-only migration runner
// (spec §5.2, ADR-0001). On boot it creates the target schema if absent, takes a
// schema-keyed advisory lock so concurrent boots serialize, ensures a per-schema
// schema_migrations table, and applies each embedded migration not yet recorded,
// each in its own transaction. The embedded SQL lives in the repo-root
// migrations package.
//
// It is deliberately small and dependency-free (no third-party migration tool):
// the configurable-schema requirement (plan §0) makes a hand-written runner
// cleaner than bending an external one. Migrations are forward-only; a rollback
// is a new migration.
package migrate
