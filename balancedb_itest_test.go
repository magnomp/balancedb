//go:build itest

package balancedb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb"
	"github.com/magnomp/balancedb/internal/dbtest"
)

func openCell(t *testing.T, pool *pgxpool.Pool) (*balancedb.DB, balancedb.Config) {
	t.Helper()
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg := balancedb.Config{
		DatabaseURL: dbtest.URL(t), Schema: schema, MaxConns: 2,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d, err := balancedb.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d, cfg
}

func request(n int) balancedb.InsertRequest {
	return balancedb.InsertRequest{
		IdempotencyKey: fmt.Sprintf("00000000-0000-4000-8000-%012x", n),
		Operations: []balancedb.InsertOp{{
			OwnerID: 1, ExternalID: "wallet", Amount: 100,
			EffectiveAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		}},
	}
}

func begin(t *testing.T, pool *pgxpool.Pool, opts pgx.TxOptions) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func execSQL(t *testing.T, tx pgx.Tx, sql string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%s: got %d, want %d", sql, n, want)
	}
}

func TestEmbeddedHostTransactionAtomicityAndSchema(t *testing.T) {
	ctx := context.Background()
	cell := dbtest.NewSchema(t)
	host := dbtest.NewSchema(t) // Deliberate same-named ledger tables in host schema.
	d, _ := openCell(t, cell)
	setup := begin(t, host, pgx.TxOptions{})
	execSQL(t, setup, `CREATE TABLE business_events (id int PRIMARY KEY)`)
	if err := setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			tx := begin(t, host, pgx.TxOptions{})
			var before, after string
			if err := tx.QueryRow(ctx, `SHOW search_path`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			execSQL(t, tx, `INSERT INTO business_events VALUES (1)`)
			// Temporary tables must not shadow the configured ledger schema.
			execSQL(t, tx, `CREATE TEMP TABLE operations (unrelated int) ON COMMIT DROP`)
			res, err := d.Insert(ctx, tx, request(1))
			if err != nil || len(res.Operations) != 1 {
				t.Fatalf("insert: %v, %v", res, err)
			}
			if err := tx.QueryRow(ctx, `SHOW search_path`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("host path changed: %q -> %q", before, after)
			}
			execSQL(t, tx, `INSERT INTO business_events VALUES (2)`)
			countRows(t, cell, `SELECT count(*) FROM operations`, 0)
			countRows(t, host, `SELECT count(*) FROM business_events`, 0)
			if commit {
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				countRows(t, cell, `SELECT count(*) FROM operations`, 1)
				countRows(t, host, `SELECT count(*) FROM business_events`, 2)
			} else if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			} else {
				countRows(t, cell, `SELECT count(*) FROM operations`, 0)
				countRows(t, cell, `SELECT count(*) FROM accounts`, 0)
				countRows(t, host, `SELECT count(*) FROM business_events`, 0)
			}
			countRows(t, host, `SELECT count(*) FROM operations`, 0)
		})
	}
}

func TestEmbeddedSavepointErrorsAndReplay(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	d, _ := openCell(t, pool)
	tx := begin(t, pool, pgx.TxOptions{})
	bad := request(1)
	bad.Operations = append(bad.Operations, balancedb.InsertOp{OwnerID: 1, ExternalID: "bad", Amount: 0, EffectiveAt: bad.Operations[0].EffectiveAt})
	if _, err := d.Insert(ctx, tx, bad); !errors.Is(err, balancedb.ErrZeroAmount) {
		t.Fatalf("partial group: %v", err)
	}
	good := request(2)
	good.Operations = append(good.Operations, balancedb.InsertOp{OwnerID: 1, ExternalID: "other", Amount: -80, EffectiveAt: good.Operations[0].EffectiveAt})
	first, err := d.Insert(ctx, tx, good)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := d.Insert(ctx, tx, good)
	if err != nil || !replay.Replayed || *replay.TransactionID != *first.TransactionID {
		t.Fatalf("replay: %v, %v", replay, err)
	}
	conflict := request(2)
	conflict.Operations = append(conflict.Operations, balancedb.InsertOp{OwnerID: 1, ExternalID: "different", Amount: -1, EffectiveAt: conflict.Operations[0].EffectiveAt})
	if _, err := d.Insert(ctx, tx, conflict); !errors.Is(err, balancedb.ErrPayloadConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	countRows(t, pool, `SELECT count(*) FROM operations`, 2)
	countRows(t, pool, `SELECT count(*) FROM transactions`, 1)
	countRows(t, pool, `SELECT count(*) FROM accounts`, 2)
}

func TestEmbeddedIndependentSchemaRegistration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aPool, bPool := dbtest.NewSchema(t), dbtest.NewSchema(t)
	a, _ := openCell(t, aPool)
	b, _ := openCell(t, bPool)
	// Hold A's registration open. B must remain able to insert and commit,
	// even through a host connection whose usual schema is A.
	aTx := begin(t, aPool, pgx.TxOptions{})
	if _, err := a.Insert(ctx, aTx, request(1)); err != nil {
		t.Fatal(err)
	}
	bTx := begin(t, aPool, pgx.TxOptions{})
	if _, err := b.Insert(ctx, bTx, request(1)); err != nil {
		t.Fatal(err)
	}
	if err := bTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	countRows(t, aPool, `SELECT count(*) FROM operations`, 0)
	countRows(t, bPool, `SELECT count(*) FROM operations`, 1)
	if err := aTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Reuse a host transaction's statement cache across alternating target
	// schemas. Identical UUIDs must still resolve in the intended schema.
	tx := begin(t, aPool, pgx.TxOptions{})
	if res, err := b.Insert(ctx, tx, request(1)); err != nil || !res.Replayed {
		t.Fatalf("schema B replay: %v, %v", res, err)
	}
	if res, err := a.Insert(ctx, tx, request(1)); err != nil || res.Replayed {
		t.Fatalf("schema A fresh insert: %v, %v", res, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	countRows(t, aPool, `SELECT count(*) FROM operations`, 1)
	countRows(t, bPool, `SELECT count(*) FROM operations`, 1)
}

func TestEmbeddedRejectsIsolationWithoutPoisoningHost(t *testing.T) {
	pool := dbtest.NewSchema(t)
	d, _ := openCell(t, pool)
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable, pgx.ReadUncommitted} {
		t.Run(string(iso), func(t *testing.T) {
			tx := begin(t, pool, pgx.TxOptions{IsoLevel: iso})
			if _, err := d.Insert(context.Background(), tx, request(1)); !errors.Is(err, balancedb.ErrUnsupportedIsolation) {
				t.Fatalf("isolation: %v", err)
			}
			execSQL(t, tx, `SELECT 1`)
		})
	}
}

func TestEmbeddedConcurrentMigrations(t *testing.T) {
	ctx := context.Background()
	cfg := balancedb.Config{DatabaseURL: dbtest.URL(t), Schema: dbtest.RandomSchema(t)}
	t.Cleanup(func() {
		if err := dbtest.DropSchema(ctx, cfg.DatabaseURL, cfg.Schema); err != nil {
			t.Error(err)
		}
	})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			_, err := balancedb.Migrate(ctx, cfg)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	versions, err := balancedb.Migrate(ctx, cfg)
	if err != nil || len(versions) != 0 {
		t.Fatalf("repeat migration: %v, %v", versions, err)
	}
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func TestEmbeddedReplicaFailoverAndLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	tx := begin(t, pool, pgx.TxOptions{})
	execSQL(t, tx, `UPDATE config SET loop_interval_ms = 10, lease_ttl_ms = 1000`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := openCell(t, pool)
	b, _ := openCell(t, pool)
	other, _ := openCell(t, dbtest.NewSchema(t))
	for _, d := range []*balancedb.DB{a, b, other} {
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		t.Cleanup(func() {
			d.Close()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	eventually(t, func() bool { return a.IsLeader() != b.IsLeader() && other.IsLeader() })
	leader, standby := a, b
	if b.IsLeader() {
		leader, standby = b, a
	}
	if err := leader.Run(ctx); !errors.Is(err, balancedb.ErrAlreadyRunning) {
		t.Fatalf("duplicate Run: %v", err)
	}
	// Cancel/join through Close, then prove the other handle can decide new work.
	leader.Close()
	if leader.IsLeader() {
		t.Fatal("closed handle reports leadership")
	}
	if err := leader.Run(ctx); !errors.Is(err, balancedb.ErrClosed) {
		t.Fatalf("Run after Close: %v", err)
	}
	eventually(t, standby.IsLeader)
	tx = begin(t, pool, pgx.TxOptions{})
	res, err := standby.Insert(ctx, tx, request(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM operations WHERE id=$1`, res.Operations[0].ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status == "CONFIRMED"
	})
	if !other.IsLeader() {
		t.Fatal("unrelated schema lost leadership")
	}
}

// editRequest builds a single-edit request (owner 1) against target.
func editRequest(n int, target int64, amount int64) balancedb.InsertRequest {
	return balancedb.InsertRequest{
		IdempotencyKey: fmt.Sprintf("00000000-0000-4000-8000-%012x", n),
		Operations:     []balancedb.InsertOp{{OwnerID: 1, EditOf: &target, Amount: amount}},
	}
}

// IT-050: DB.Insert with EditOf inside a host transaction commits the PENDING
// edit together with the host's own row (or neither), leaves the host
// search_path untouched, returns EditOf in the outcome, and the edit reaches
// APPLIED through Run — as does a grouped edit mixed with a new operation.
func TestEmbeddedEditInHostTransaction(t *testing.T) {
	ctx := context.Background()
	cell := dbtest.NewSchema(t)
	host := dbtest.NewSchema(t)
	d, _ := openCell(t, cell)
	setup := begin(t, host, pgx.TxOptions{})
	execSQL(t, setup, `CREATE TABLE corrections (id int PRIMARY KEY)`)
	if err := setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		d.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	// The target: a regular operation registered and decided as usual.
	tx := begin(t, host, pgx.TxOptions{})
	res, err := d.Insert(ctx, tx, request(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	target := res.Operations[0].ID
	eventually(t, func() bool { return opStatus(t, cell, target) == "CONFIRMED" })

	// The edit, registered with a host row; a rolled-back host transaction takes
	// the edit with it, a committed one makes both durable together.
	for _, commit := range []bool{false, true} {
		tx := begin(t, host, pgx.TxOptions{})
		var before, after string
		if err := tx.QueryRow(ctx, `SHOW search_path`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		execSQL(t, tx, `INSERT INTO corrections VALUES (1)`)
		edit, err := d.Insert(ctx, tx, editRequest(2, target, 250))
		if err != nil {
			t.Fatalf("edit insert: %v", err)
		}
		out := edit.Operations[0]
		if out.Status != "PENDING" || out.EditOf == nil || *out.EditOf != target || edit.Replayed {
			t.Fatalf("edit outcome = %+v, want PENDING with EditOf %d", out, target)
		}
		if err := tx.QueryRow(ctx, `SHOW search_path`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("host path changed: %q -> %q", before, after)
		}
		countRows(t, cell, `SELECT count(*) FROM operations WHERE edit_of IS NOT NULL`, 0)
		if !commit {
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			countRows(t, cell, `SELECT count(*) FROM operations WHERE edit_of IS NOT NULL`, 0)
			countRows(t, host, `SELECT count(*) FROM corrections`, 0)
			continue
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		countRows(t, cell, `SELECT count(*) FROM operations WHERE edit_of IS NOT NULL`, 1)
		countRows(t, host, `SELECT count(*) FROM corrections`, 1)
		eventually(t, func() bool { return opStatus(t, cell, out.ID) == "APPLIED" })
		var amount int64
		var revision int32
		if err := cell.QueryRow(ctx, `SELECT amount, revision FROM operations WHERE id = $1`, target).Scan(&amount, &revision); err != nil {
			t.Fatal(err)
		}
		if amount != 250 || revision != 2 {
			t.Fatalf("target after the applied edit = %d at revision %d, want 250 at 2", amount, revision)
		}
		countRows(t, cell, `SELECT count(*) FROM operation_revisions`, 1)
	}

	// Grouped: an edit plus a new operation, one atomic unit through the host tx.
	rev := int32(2)
	tx = begin(t, host, pgx.TxOptions{})
	group, err := d.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000003",
		Operations: []balancedb.InsertOp{
			{OwnerID: 1, EditOf: &target, Amount: 300, ExpectedRevision: &rev},
			{OwnerID: 1, ExternalID: "wallet", Amount: -20, EffectiveAt: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)},
		},
	})
	if err != nil {
		t.Fatalf("grouped edit insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if group.TransactionID == nil || group.Operations[0].EditOf == nil || group.Operations[1].EditOf != nil {
		t.Fatalf("grouped outcome = %+v", group)
	}
	eventually(t, func() bool { return txStatus(t, cell, *group.TransactionID) == "COMMITTED" })
	if s := opStatus(t, cell, group.Operations[0].ID); s != "APPLIED" {
		t.Fatalf("grouped edit leg = %s, want APPLIED", s)
	}
	if s := opStatus(t, cell, group.Operations[1].ID); s != "CONFIRMED" {
		t.Fatalf("grouped new leg = %s, want CONFIRMED", s)
	}
	var balance int64
	if err := cell.QueryRow(ctx, `SELECT confirmed_balance FROM accounts WHERE owner_id = 1 AND external_id = 'wallet'`).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 280 { // 100 → 250 → 300, then −20
		t.Fatalf("wallet balance = %d, want 280", balance)
	}
}

// IT-051: DB.Insert with a foreign (or unknown) edit target returns an error
// errors.Is-able as ErrEditTargetNotFound; the savepoint is rolled back so the
// host statement issued before the call is still committed and no ledger row is
// left behind. The other sentinels come through the same savepoint unchanged.
func TestEmbeddedEditSentinelsThroughSavepoint(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	d, _ := openCell(t, pool)
	setup := begin(t, pool, pgx.TxOptions{})
	execSQL(t, setup, `CREATE TABLE host_rows (id int PRIMARY KEY)`)
	if err := setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Owner 2's operation is not a valid target for owner 1.
	tx := begin(t, pool, pgx.TxOptions{})
	foreign, err := d.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-0000000000f1",
		Operations:     []balancedb.InsertOp{{OwnerID: 2, ExternalID: "theirs", Amount: 5, EffectiveAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	foreignID := foreign.Operations[0].ID

	tx = begin(t, pool, pgx.TxOptions{})
	execSQL(t, tx, `INSERT INTO host_rows VALUES (1)`)
	if _, err := d.Insert(ctx, tx, editRequest(1, foreignID, 7)); !errors.Is(err, balancedb.ErrEditTargetNotFound) {
		t.Fatalf("foreign target: %v, want ErrEditTargetNotFound", err)
	}
	if _, err := d.Insert(ctx, tx, editRequest(2, foreignID+1000, 7)); !errors.Is(err, balancedb.ErrEditTargetNotFound) {
		t.Fatalf("unknown target: %v, want ErrEditTargetNotFound", err)
	}
	// The remaining sentinels, all structural, all through the same savepoint.
	own, err := d.Insert(ctx, tx, request(3))
	if err != nil {
		t.Fatal(err)
	}
	ownID := own.Operations[0].ID
	if _, err := d.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000004",
		Operations:     []balancedb.InsertOp{{OwnerID: 1, EditOf: &ownID}},
	}); !errors.Is(err, balancedb.ErrEditChangesNothing) {
		t.Fatalf("changes nothing: %v", err)
	}
	zero := int32(0)
	if _, err := d.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000005",
		Operations:     []balancedb.InsertOp{{OwnerID: 1, EditOf: &ownID, Amount: 1, ExpectedRevision: &zero}},
	}); !errors.Is(err, balancedb.ErrInvalidExpectedRevision) {
		t.Fatalf("expected_revision 0: %v", err)
	}
	if _, err := d.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000006",
		Operations:     []balancedb.InsertOp{{OwnerID: 1, EditOf: &ownID, Amount: 1, ReversalOf: &ownID}},
	}); !errors.Is(err, balancedb.ErrEditWithReversal) {
		t.Fatalf("reversal on edit: %v", err)
	}
	if _, err := d.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000007",
		Operations:     []balancedb.InsertOp{{OwnerID: 1, EditOf: &ownID, Amount: 1}, {OwnerID: 1, EditOf: &ownID, Amount: 2}},
	}); !errors.Is(err, balancedb.ErrDuplicateEditTarget) {
		t.Fatalf("duplicate target: %v", err)
	}
	// An edit of an edit registration is refused as well.
	edit, err := d.Insert(ctx, tx, editRequest(8, ownID, 9))
	if err != nil {
		t.Fatal(err)
	}
	editID := edit.Operations[0].ID
	if _, err := d.Insert(ctx, tx, editRequest(9, editID, 9)); !errors.Is(err, balancedb.ErrEditTargetNotOperation) {
		t.Fatalf("edit of an edit: %v", err)
	}
	execSQL(t, tx, `UPDATE config SET allow_edits = false`)
	if _, err := d.Insert(ctx, tx, editRequest(10, ownID, 11)); !errors.Is(err, balancedb.ErrEditsDisabled) {
		t.Fatalf("edits disabled: %v", err)
	}
	execSQL(t, tx, `UPDATE config SET allow_edits = true`)
	// The host transaction is still usable after every refusal.
	execSQL(t, tx, `INSERT INTO host_rows VALUES (2)`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	countRows(t, pool, `SELECT count(*) FROM host_rows`, 2)
	countRows(t, pool, `SELECT count(*) FROM operations WHERE edit_of IS NOT NULL`, 1)
	countRows(t, pool, `SELECT count(*) FROM operations`, 3) // theirs, own, one edit
}

func opStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM operations WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func txStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM transactions WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
