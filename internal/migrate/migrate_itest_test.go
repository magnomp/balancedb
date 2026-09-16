//go:build itest

package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/migrate"
	"github.com/magnomp/balancedb/migrations"
)

// latestVersion is the highest embedded migration; every "fresh boot" assertion
// below is phrased against it so adding a migration updates one constant.
const latestVersion = 3

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
	if len(applied) != latestVersion {
		t.Fatalf("first Run applied = %v, want 1..%d", applied, latestVersion)
	}
	for i, v := range applied {
		if v != i+1 {
			t.Fatalf("first Run applied = %v, want ascending 1..%d", applied, latestVersion)
		}
	}

	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// All six spec §5.2 tables, operation_revisions (0002) and schema_migrations
	// exist in this schema.
	for _, tbl := range []string{
		"accounts", "transactions", "operations", "balance_snapshots",
		"leader_lease", "config", "schema_migrations", "operation_revisions",
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
	if n := count(t, ctx, pool, "SELECT max(version) FROM schema_migrations"); n != latestVersion {
		t.Errorf("schema_migrations max version = %d, want %d", n, latestVersion)
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
	// Every version is applied exactly once across all racing migrators.
	if totalApplied != latestVersion {
		t.Fatalf("%d versions applied across %d migrators, want exactly %d", totalApplied, migrators, latestVersion)
	}

	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if n := count(t, ctx, pool, "SELECT count(*) FROM schema_migrations"); n != latestVersion {
		t.Errorf("schema_migrations rows = %d, want %d", n, latestVersion)
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

	// Each schema has its own version table recording the latest version.
	if n := count(t, ctx, poolA, "SELECT max(version) FROM schema_migrations"); n != latestVersion {
		t.Errorf("A schema_migrations max version = %d, want %d", n, latestVersion)
	}
	if n := count(t, ctx, poolB, "SELECT max(version) FROM schema_migrations"); n != latestVersion {
		t.Errorf("B schema_migrations max version = %d, want %d", n, latestVersion)
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

// TestRun_FreshSchemaHasEditObjects (IT-001): a fresh boot applies 0002 too —
// the edit columns, their CHECKs, the partial index, operation_revisions and
// config.allow_edits = true are all present.
func TestRun_FreshSchemaHasEditObjects(t *testing.T) {
	url := dbtest.URL(t)
	schema := dbtest.RandomSchema(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schema) })

	if _, err := migrate.Run(ctx, url, schema); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	assertEditObjects(t, ctx, pool, schema)
}

// TestRun_FreshSchemaHasDeleteObjects (IT-001, deletion): a fresh boot applies
// 0003 too — is_delete defaults to FALSE, deleted_by/deleted_at exist and are
// NULL, both CHECKs reject the inconsistent states, and config.allow_deletes
// reads true.
func TestRun_FreshSchemaHasDeleteObjects(t *testing.T) {
	url := dbtest.URL(t)
	schema := dbtest.RandomSchema(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schema) })

	if _, err := migrate.Run(ctx, url, schema); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	assertDeleteObjects(t, ctx, pool, schema)

	// Defaults: a row inserted without the new columns is a non-delete with
	// NULL deletion markers.
	var accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (owner_id, external_id) VALUES (1, 'cash') RETURNING id`).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	var opID int64
	var isDelete bool
	var deletedBy *int64
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, status, idempotency_key, payload_hash, confirmed_at)
		 VALUES ($1, -1500, '2026-08-22T10:00:00Z', 'CONFIRMED', gen_random_uuid(), '\x00', now())
		 RETURNING id, is_delete, deleted_by, deleted_at`, accountID).Scan(&opID, &isDelete, &deletedBy, &deletedAt); err != nil {
		t.Fatalf("seed operation: %v", err)
	}
	if isDelete || deletedBy != nil || deletedAt != nil {
		t.Errorf("fresh operation: is_delete = %v, deleted_by = %v, deleted_at = %v; want false, NULL, NULL", isDelete, deletedBy, deletedAt)
	}

	// ops_delete_is_edit: is_delete = TRUE requires edit_of.
	_, err = pool.Exec(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, is_delete)
		 VALUES ($1, -1500, '2026-08-22T10:00:00Z', TRUE)`, accountID)
	assertCheckViolation(t, err, "ops_delete_is_edit")

	// ...and is satisfied by a delete registration shaped like the ledger
	// writes it: edit_of set, values copied from the target.
	if _, err := pool.Exec(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, edit_of, is_delete)
		 VALUES ($1, -1500, '2026-08-22T10:00:00Z', $2, TRUE)`, accountID, opID); err != nil {
		t.Fatalf("insert delete registration: %v", err)
	}

	// ops_deleted_pair: deleted_by and deleted_at are set together or not at all.
	_, err = pool.Exec(ctx, `UPDATE operations SET deleted_by = $1 WHERE id = $2`, opID, opID)
	assertCheckViolation(t, err, "ops_deleted_pair")
	_, err = pool.Exec(ctx, `UPDATE operations SET deleted_at = now() WHERE id = $1`, opID)
	assertCheckViolation(t, err, "ops_deleted_pair")
	if _, err := pool.Exec(ctx,
		`UPDATE operations SET status = 'DELETED', deleted_by = $1, deleted_at = now() WHERE id = $1`, opID); err != nil {
		t.Fatalf("mark DELETED with the pair set: %v", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM operations WHERE status = 'DELETED' AND deleted_by IS NOT NULL AND deleted_at IS NOT NULL"); n != 1 {
		t.Errorf("DELETED rows with both markers = %d, want 1", n)
	}
}

// TestRun_UpgradeFromVersion1KeepsRows (IT-002): a cell installed at version 1
// and holding data — an account, a CONFIRMED single, a COMMITTED group — is
// upgraded by Run applying exactly [2, 3]. Every row survives with revision = 1
// and is_delete = false and, because the new columns have constant defaults,
// no operations tuple is rewritten: xmin is unchanged for every operation row.
func TestRun_UpgradeFromVersion1KeepsRows(t *testing.T) {
	url := dbtest.URL(t)
	schema := dbtest.RandomSchema(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = dbtest.DropSchema(context.Background(), url, schema) })

	installVersion1(t, ctx, url, schema)

	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Seed data exactly as a pre-editing cell would hold it.
	var accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (owner_id, external_id, confirmed_balance, version)
		 VALUES (1, 'cash', -1500, 3) RETURNING id`).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	var singleID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, status, idempotency_key, payload_hash, confirmed_at)
		 VALUES ($1, -1500, '2026-08-22T10:00:00Z', 'CONFIRMED', gen_random_uuid(), '\x00', now())
		 RETURNING id`, accountID).Scan(&singleID); err != nil {
		t.Fatalf("seed single: %v", err)
	}
	var txID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO transactions (idempotency_key, payload_hash, op_count, status, decided_at)
		 VALUES (gen_random_uuid(), '\x00', 2, 'COMMITTED', now()) RETURNING id`).Scan(&txID); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, transaction_id, status, confirmed_at)
		 VALUES ($1, 700, '2026-08-23T10:00:00Z', $2, 'CONFIRMED', now()),
		        ($1, -200, '2026-08-23T10:00:00Z', $2, 'CONFIRMED', now())`, accountID, txID); err != nil {
		t.Fatalf("seed group legs: %v", err)
	}
	xminBefore := operationXmins(t, ctx, pool)
	if len(xminBefore) != 3 {
		t.Fatalf("seeded operations = %d, want 3", len(xminBefore))
	}

	applied, err := migrate.Run(ctx, url, schema)
	if err != nil {
		t.Fatalf("upgrade Run: %v", err)
	}
	if len(applied) != 2 || applied[0] != 2 || applied[1] != 3 {
		t.Fatalf("upgrade Run applied = %v, want [2 3]", applied)
	}

	assertEditObjects(t, ctx, pool, schema)
	assertDeleteObjects(t, ctx, pool, schema)

	// Rows intact, revision = 1 everywhere, never-edited markers NULL.
	if n := count(t, ctx, pool, "SELECT count(*) FROM operations"); n != 3 {
		t.Errorf("operations after upgrade = %d, want 3", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM operations WHERE revision = 1 AND revised_at IS NULL AND edit_of IS NULL AND expected_revision IS NULL"); n != 3 {
		t.Errorf("operations with revision = 1 and NULL edit columns = %d, want 3", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM operations WHERE NOT is_delete AND deleted_by IS NULL AND deleted_at IS NULL"); n != 3 {
		t.Errorf("operations with is_delete = false and NULL delete columns = %d, want 3", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM operations WHERE status = 'CONFIRMED'"); n != 3 {
		t.Errorf("CONFIRMED operations after upgrade = %d, want 3", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM transactions WHERE status = 'COMMITTED' AND op_count = 2"); n != 1 {
		t.Errorf("COMMITTED transactions after upgrade = %d, want 1", n)
	}
	if n := count(t, ctx, pool, "SELECT confirmed_balance FROM accounts WHERE id = $1", accountID); n != -1500 {
		t.Errorf("account confirmed_balance after upgrade = %d, want -1500", n)
	}
	if n := count(t, ctx, pool, "SELECT amount FROM operations WHERE id = $1", singleID); n != -1500 {
		t.Errorf("single amount after upgrade = %d, want -1500", n)
	}

	// No tuple rewritten: xmin unchanged for every operation row.
	xminAfter := operationXmins(t, ctx, pool)
	for id, before := range xminBefore {
		if after, ok := xminAfter[id]; !ok || after != before {
			t.Errorf("operation %d xmin: before %d, after %d (row rewritten by migration)", id, before, after)
		}
	}
}

// installVersion1 builds a cell exactly as a pre-editing binary did: the schema,
// the runner's version table, 0001_init.sql applied and recorded as version 1 —
// and nothing newer. It reads 0001 from the embedded FS so the fixture cannot
// drift from the file the runner ships.
func installVersion1(t *testing.T, ctx context.Context, url, schema string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET search_path = "+quoted); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (
		version    INT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	init, err := fs.ReadFile(migrations.FS, "0001_init.sql")
	if err != nil {
		t.Fatalf("read embedded 0001_init.sql: %v", err)
	}
	if _, err := conn.Exec(ctx, string(init)); err != nil {
		t.Fatalf("apply 0001_init.sql: %v", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES (1)"); err != nil {
		t.Fatalf("record version 1: %v", err)
	}
}

// assertEditObjects checks every object 0002 adds (spec Data Models).
func assertEditObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	if n := countRegClass(t, ctx, pool, schema, "operation_revisions"); n != 1 {
		t.Errorf("table operation_revisions: found %d, want 1", n)
	}
	for _, col := range []string{"edit_of", "expected_revision", "revision", "revised_at"} {
		if n := countColumn(t, ctx, pool, schema, "operations", col); n != 1 {
			t.Errorf("operations.%s: found %d, want 1", col, n)
		}
	}
	if n := countColumn(t, ctx, pool, schema, "config", "allow_edits"); n != 1 {
		t.Errorf("config.allow_edits: found %d, want 1", n)
	}
	for _, c := range []string{"ops_edit_not_reversal", "ops_edit_not_self", "ops_expected_rev_pos"} {
		const q = `SELECT count(*) FROM information_schema.table_constraints
		           WHERE table_schema = $1 AND table_name = 'operations'
		             AND constraint_name = $2 AND constraint_type = 'CHECK'`
		if n := count(t, ctx, pool, q, schema, c); n != 1 {
			t.Errorf("CHECK %s on operations: found %d, want 1", c, n)
		}
	}
	const idx = `SELECT count(*) FROM pg_indexes
	             WHERE schemaname = $1 AND tablename = 'operations' AND indexname = 'idx_ops_edit_of'`
	if n := count(t, ctx, pool, idx, schema); n != 1 {
		t.Errorf("idx_ops_edit_of: found %d, want 1", n)
	}
	var allow bool
	if err := pool.QueryRow(ctx, "SELECT allow_edits FROM config").Scan(&allow); err != nil {
		t.Fatalf("read config.allow_edits: %v", err)
	}
	if !allow {
		t.Errorf("config.allow_edits = false, want true (default)")
	}
}

// assertDeleteObjects checks every object 0003 adds (spec Data Models).
func assertDeleteObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	for _, col := range []string{"is_delete", "deleted_by", "deleted_at"} {
		if n := countColumn(t, ctx, pool, schema, "operations", col); n != 1 {
			t.Errorf("operations.%s: found %d, want 1", col, n)
		}
	}
	const dflt = `SELECT column_default FROM information_schema.columns
	              WHERE table_schema = $1 AND table_name = 'operations' AND column_name = 'is_delete'`
	var isDeleteDefault string
	if err := pool.QueryRow(ctx, dflt, schema).Scan(&isDeleteDefault); err != nil {
		t.Fatalf("read operations.is_delete default: %v", err)
	}
	if isDeleteDefault != "false" {
		t.Errorf("operations.is_delete default = %q, want \"false\"", isDeleteDefault)
	}
	if n := countColumn(t, ctx, pool, schema, "config", "allow_deletes"); n != 1 {
		t.Errorf("config.allow_deletes: found %d, want 1", n)
	}
	for _, c := range []string{"ops_delete_is_edit", "ops_deleted_pair"} {
		const q = `SELECT count(*) FROM information_schema.table_constraints
		           WHERE table_schema = $1 AND table_name = 'operations'
		             AND constraint_name = $2 AND constraint_type = 'CHECK'`
		if n := count(t, ctx, pool, q, schema, c); n != 1 {
			t.Errorf("CHECK %s on operations: found %d, want 1", c, n)
		}
	}
	var allow bool
	if err := pool.QueryRow(ctx, "SELECT allow_deletes FROM config").Scan(&allow); err != nil {
		t.Fatalf("read config.allow_deletes: %v", err)
	}
	if !allow {
		t.Errorf("config.allow_deletes = false, want true (default)")
	}
}

// assertCheckViolation fails unless err is a CHECK violation (SQLSTATE 23514)
// on the named constraint.
func assertCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("CHECK %s: statement succeeded, want violation", constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("CHECK %s: error is not a PgError: %v", constraint, err)
	}
	if pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Fatalf("CHECK %s: got SQLSTATE %s on constraint %q: %v", constraint, pgErr.Code, pgErr.ConstraintName, err)
	}
}

// operationXmins returns xmin per operation id — the tuple's inserting
// transaction, which changes whenever Postgres rewrites the row.
func operationXmins(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[int64]uint32 {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT id, xmin::text::bigint FROM operations")
	if err != nil {
		t.Fatalf("read xmins: %v", err)
	}
	defer rows.Close()
	out := make(map[int64]uint32)
	for rows.Next() {
		var id int64
		var xmin int64
		if err := rows.Scan(&id, &xmin); err != nil {
			t.Fatalf("scan xmin: %v", err)
		}
		out[id] = uint32(xmin)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate xmins: %v", err)
	}
	return out
}

// count runs a scalar query returning one bigint/int.
func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// countColumn returns 1 if the named column exists on the table in the given
// schema, else 0.
func countColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, table, column string) int64 {
	t.Helper()
	const q = `SELECT count(*) FROM information_schema.columns
	           WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`
	return count(t, ctx, pool, q, schema, table, column)
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
