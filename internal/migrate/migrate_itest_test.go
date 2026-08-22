//go:build itest

package migrate_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/migrate"
)

// TestRun_CreatesObjectsAndIdempotent: a first boot builds the schema and seeds
// the singleton rows; a second boot is a no-op.
func TestRun_CreatesObjectsAndIdempotent(t *testing.T) {
	url := dbtest.URL(t)
	schema := dbtest.RandomSchema(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schema) })

	applied, err := migrate.Run(ctx, url, schema)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(applied) != 1 || applied[0] != 1 {
		t.Fatalf("first Run applied = %v, want [1]", applied)
	}

	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// All six spec §5.2 tables plus schema_migrations exist in this schema.
	for _, tbl := range []string{
		"accounts", "transactions", "operations", "balance_snapshots",
		"leader_lease", "config", "schema_migrations",
	} {
		if n := countRegClass(t, ctx, pool, schema, tbl); n != 1 {
			t.Errorf("table %q: found %d, want 1", tbl, n)
		}
	}

	// Seed rows are present, exactly one each.
	if n := count(t, ctx, pool, "SELECT count(*) FROM config"); n != 1 {
		t.Errorf("config rows = %d, want 1", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM leader_lease"); n != 1 {
		t.Errorf("leader_lease rows = %d, want 1", n)
	}
	// Config defaults come from the DDL.
	if n := count(t, ctx, pool, "SELECT lease_ttl_ms FROM config"); n != 15000 {
		t.Errorf("config.lease_ttl_ms = %d, want 15000", n)
	}
	if n := count(t, ctx, pool, "SELECT version FROM schema_migrations"); n != 1 {
		t.Errorf("schema_migrations version = %d, want 1", n)
	}

	// Second boot: nothing to apply.
	applied2, err := migrate.Run(ctx, url, schema)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(applied2) != 0 {
		t.Fatalf("second Run applied = %v, want []", applied2)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM config"); n != 1 {
		t.Errorf("config rows after re-migrate = %d, want 1 (ON CONFLICT DO NOTHING)", n)
	}
}

// TestRun_ParallelMigratorsRace: many processes booting the same schema at once
// (api + processor + replicas) must be safe. The advisory lock serializes them;
// exactly one applies the migration, the rest find it done.
func TestRun_ParallelMigratorsRace(t *testing.T) {
	url := dbtest.URL(t)
	schema := dbtest.RandomSchema(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schema) })

	const migrators = 8
	var wg sync.WaitGroup
	results := make([][]int, migrators)
	errs := make([]error, migrators)
	for i := 0; i < migrators; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = migrate.Run(ctx, url, schema)
		}(i)
	}
	wg.Wait()

	totalApplied := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
		totalApplied += len(results[i])
	}
	// Version 1 is applied exactly once across all racing migrators.
	if totalApplied != 1 {
		t.Fatalf("version 1 applied %d times across %d migrators, want exactly 1", totalApplied, migrators)
	}

	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if n := count(t, ctx, pool, "SELECT count(*) FROM schema_migrations"); n != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM config"); n != 1 {
		t.Errorf("config rows = %d, want 1", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM leader_lease"); n != 1 {
		t.Errorf("leader_lease rows = %d, want 1", n)
	}
}

// TestRun_TwoSchemasIndependent: a different BALANCEDB_SCHEMA in the same
// database is a parallel, independently versioned installation.
func TestRun_TwoSchemasIndependent(t *testing.T) {
	url := dbtest.URL(t)
	schemaA := dbtest.RandomSchema(t)
	schemaB := dbtest.RandomSchema(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schemaA) })
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schemaB) })

	if _, err := migrate.Run(ctx, url, schemaA); err != nil {
		t.Fatalf("migrate A: %v", err)
	}
	if _, err := migrate.Run(ctx, url, schemaB); err != nil {
		t.Fatalf("migrate B: %v", err)
	}

	poolA, err := db.Connect(ctx, url, schemaA, 4)
	if err != nil {
		t.Fatalf("connect A: %v", err)
	}
	defer poolA.Close()
	poolB, err := db.Connect(ctx, url, schemaB, 4)
	if err != nil {
		t.Fatalf("connect B: %v", err)
	}
	defer poolB.Close()

	// Each schema has its own version table recording version 1.
	if n := count(t, ctx, poolA, "SELECT version FROM schema_migrations"); n != 1 {
		t.Errorf("A schema_migrations version = %d, want 1", n)
	}
	if n := count(t, ctx, poolB, "SELECT version FROM schema_migrations"); n != 1 {
		t.Errorf("B schema_migrations version = %d, want 1", n)
	}

	// Data written into A's accounts is invisible in B: fully separate tables.
	if _, err := poolA.Exec(ctx, "INSERT INTO accounts (owner_id, external_id) VALUES (1, 'only-in-a')"); err != nil {
		t.Fatalf("insert into A.accounts: %v", err)
	}
	if n := count(t, ctx, poolA, "SELECT count(*) FROM accounts"); n != 1 {
		t.Errorf("A accounts = %d, want 1", n)
	}
	if n := count(t, ctx, poolB, "SELECT count(*) FROM accounts"); n != 0 {
		t.Errorf("B accounts = %d, want 0 (independent installation)", n)
	}
}

// count runs a scalar query returning one bigint/int.
func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// countRegClass returns 1 if the named table exists in the given schema, else 0.
func countRegClass(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, table string) int64 {
	t.Helper()
	var n int64
	const q = `SELECT count(*) FROM information_schema.tables
	           WHERE table_schema = $1 AND table_name = $2`
	if err := pool.QueryRow(ctx, q, schema, table).Scan(&n); err != nil {
		t.Fatalf("check table %q: %v", table, err)
	}
	return n
}
