package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/migrations"
)

// SQL kept as consts beside their single call site (plan §0). Written
// unqualified — the runner sets search_path to the target schema first, so
// schema_migrations lives inside that schema (ADR-0001, step 4).
const (
	createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INT PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

	selectApplied = `SELECT version FROM schema_migrations`

	insertVersion = `INSERT INTO schema_migrations (version) VALUES ($1)`
)

// advisoryLockNamespace prefixes the advisory-lock key so BalanceDB's per-schema
// migration lock cannot collide with an unrelated advisory lock in the same
// database. The key is hashtext('balancedb:' || <schema>), so concurrent boots
// of the same schema serialize while different schemas migrate independently.
const advisoryLockNamespace = "balancedb:"

// migration is one embedded file, keyed by its numeric prefix.
type migration struct {
	version int
	name    string // filename, for error messages
	sql     string
}

// Run applies all pending embedded migrations to the target schema, creating the
// schema if absent. It opens its own dedicated connection (not the app pool) so
// the session-scoped advisory lock is released deterministically when that
// connection closes, even if an explicit unlock fails (ADR-0001).
//
// It is safe to call concurrently from any number of processes against the same
// schema: the advisory lock serializes them and each migration is recorded in
// schema_migrations, so all but one caller find nothing to do. Returns the
// versions applied by this call (empty on a no-op second boot).
func Run(ctx context.Context, databaseURL, schema string) (applied []int, err error) {
	migs, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	// Step 1: a dedicated connection. Not from the pgxpool — a session-level
	// advisory lock left on a pooled connection would leak into later checkouts.
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect for migration: %w", err)
	}
	defer func() {
		// Closing the connection releases any advisory lock unconditionally.
		if cerr := conn.Close(context.Background()); cerr != nil && err == nil {
			err = fmt.Errorf("close migration connection: %w", cerr)
		}
	}()

	// Set search_path on this session (quoted identifier, never interpolated).
	// The schema need not exist yet — resolution is lazy until an object is used.
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = conn.Exec(ctx, "SET search_path = "+quoted); err != nil {
		return nil, fmt.Errorf("set search_path to %q: %w", schema, err)
	}

	// Serialize concurrent boots on a schema-derived advisory lock BEFORE
	// creating the schema. The lock key is the schema name and needs no schema
	// object, and CREATE SCHEMA IF NOT EXISTS is not concurrency-safe in Postgres
	// (racing boots can both pass the existence check and one fails 23505), so
	// schema creation must sit inside the lock (ADR-0004; a reorder of plan §M2
	// steps 2 and 3).
	lockArg := advisoryLockNamespace + schema
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockArg); err != nil {
		return nil, fmt.Errorf("acquire advisory lock for %q: %w", schema, err)
	}
	// Release the lock. Best-effort — the deferred Close above is the
	// unconditional backstop.
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", lockArg)
	}()

	// Create the schema, now guarded by the lock.
	if _, err = conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
		return nil, fmt.Errorf("create schema %q: %w", schema, err)
	}

	// The per-schema version table.
	if _, err = conn.Exec(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("ensure schema_migrations in %q: %w", schema, err)
	}

	done, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	// Step 5: apply each pending migration in its own transaction, recording the
	// version in the same transaction so a crash leaves a consistent record.
	for _, m := range migs {
		if _, ok := done[m.version]; ok {
			continue
		}
		if err = applyOne(ctx, conn, m); err != nil {
			return applied, fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		applied = append(applied, m.version)
	}

	return applied, nil
}

// applyOne runs a single migration and records its version atomically.
func applyOne(ctx context.Context, conn *pgx.Conn, m migration) (err error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err = tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err = tx.Exec(ctx, insertVersion, m.version); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// appliedVersions returns the set of versions already recorded in the schema.
func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]struct{}, error) {
	rows, err := conn.Query(ctx, selectApplied)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	done := make(map[int]struct{})
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		done[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return done, nil
}

// loadMigrations reads the embedded *.sql files, parses each numeric prefix, and
// returns them sorted ascending by version. It rejects malformed names and
// duplicate versions so a packaging mistake fails loudly rather than silently
// skipping or double-applying.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var migs []migration
	seen := make(map[int]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := parseVersion(name)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, name)
		}
		seen[version] = name

		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", name, err)
		}
		migs = append(migs, migration{version: version, name: name, sql: string(body)})
	}

	if len(migs) == 0 {
		return nil, fmt.Errorf("no embedded migrations found")
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

// parseVersion extracts the leading integer of an "NNNN_name.sql" filename.
func parseVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	prefix, _, found := strings.Cut(base, "_")
	if !found || prefix == "" {
		return 0, fmt.Errorf("migration %q: name must be NNNN_description.sql", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration %q: numeric prefix: %w", name, err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("migration %q: version must be positive, got %d", name, version)
	}
	return version, nil
}
