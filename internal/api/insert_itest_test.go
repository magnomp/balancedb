//go:build itest

package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/model"
)

// key returns a distinct valid UUID per call within a test run.
func key(n int) string {
	return "00000000-0000-4000-8000-" + padHex(n)
}

func padHex(n int) string {
	const hexdigits = "0123456789abcdef"
	buf := []byte("000000000000")
	i := len(buf) - 1
	for n > 0 && i >= 0 {
		buf[i] = hexdigits[n&0xf]
		n >>= 4
		i--
	}
	return string(buf)
}

// insert runs api.Insert inside a real transaction and commits it, returning the
// result. A test error rolls back.
func insert(t *testing.T, pool *pgxpool.Pool, req api.InsertRequest) (*api.InsertResult, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := api.Insert(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return res, nil
}

func TestInsertSingle(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	when := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	res, err := insert(t, pool, api.InsertRequest{
		IdempotencyKey: key(1),
		Operations:     []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: -1500, EffectiveAt: when}},
	})
	if err != nil {
		t.Fatalf("insert single: %v", err)
	}
	if res.Replayed {
		t.Fatal("fresh insert must not be a replay")
	}
	if res.TransactionID != nil {
		t.Fatal("single must have no transaction row")
	}
	if len(res.Operations) != 1 || res.Operations[0].Status != string(model.OpPending) {
		t.Fatalf("unexpected operations: %+v", res.Operations)
	}

	// The operation row exists, is a single (no transaction_id), and carries the
	// key + hash.
	var (
		accountID int64
		amount    int64
		txID      *int64
		status    string
		hasKey    bool
		hasHash   bool
	)
	err = pool.QueryRow(ctx, `SELECT account_id, amount, transaction_id, status,
		idempotency_key IS NOT NULL, payload_hash IS NOT NULL FROM operations WHERE id=$1`,
		res.Operations[0].ID).Scan(&accountID, &amount, &txID, &status, &hasKey, &hasHash)
	if err != nil {
		t.Fatalf("read op: %v", err)
	}
	if amount != -1500 || txID != nil || status != "PENDING" || !hasKey || !hasHash {
		t.Fatalf("unexpected op row: amount=%d tx=%v status=%s key=%v hash=%v", amount, txID, status, hasKey, hasHash)
	}

	// The account was upserted on demand with unbounded limits.
	var minB, maxB *int64
	var version int64
	if err := pool.QueryRow(ctx, `SELECT min_balance, max_balance, version FROM accounts WHERE id=$1`, accountID).
		Scan(&minB, &maxB, &version); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if minB != nil || maxB != nil || version != 0 {
		t.Fatalf("expected unbounded new account at version 0: min=%v max=%v version=%d", minB, maxB, version)
	}
}

func TestInsertGroup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	when := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	res, err := insert(t, pool, api.InsertRequest{
		IdempotencyKey: key(2),
		Operations: []api.InsertOp{
			{OwnerID: 7, ExternalID: "checking", Amount: -1000, EffectiveAt: when},
			{OwnerID: 7, ExternalID: "envelope", Amount: -1000, EffectiveAt: when},
			{OwnerID: 7, ExternalID: "checking", Amount: 250, EffectiveAt: when},
		},
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if res.TransactionID == nil {
		t.Fatal("group must have a transaction row")
	}
	if res.TransactionStatus != string(model.TxPending) || len(res.Operations) != 3 {
		t.Fatalf("unexpected group result: %+v", res)
	}

	// Registration order (by id) matches request order.
	for i := 1; i < len(res.Operations); i++ {
		if res.Operations[i].ID <= res.Operations[i-1].ID {
			t.Fatalf("leg ids not in registration order: %+v", res.Operations)
		}
	}

	// op_count matches the number of legs, all legs point at the transaction, and
	// legs carry no idempotency key (that lives on the transaction).
	var opCount, legCount, keyed int
	if err := pool.QueryRow(ctx, `SELECT op_count FROM transactions WHERE id=$1`, *res.TransactionID).Scan(&opCount); err != nil {
		t.Fatalf("read tx: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*), count(idempotency_key) FROM operations WHERE transaction_id=$1`,
		*res.TransactionID).Scan(&legCount, &keyed); err != nil {
		t.Fatalf("count legs: %v", err)
	}
	if opCount != 3 || legCount != 3 || keyed != 0 {
		t.Fatalf("op_count=%d legCount=%d keyed=%d, want 3,3,0", opCount, legCount, keyed)
	}
}

func TestInsertIdempotentReplaySingle(t *testing.T) {
	pool := dbtest.NewSchema(t)
	req := api.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: 500, EffectiveAt: time.Unix(0, 0).UTC()}},
	}
	first, err := insert(t, pool, req)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	second, err := insert(t, pool, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed {
		t.Fatal("second insert with same key must be a replay")
	}
	if second.Operations[0].ID != first.Operations[0].ID {
		t.Fatalf("replay returned a different id: %d != %d", second.Operations[0].ID, first.Operations[0].ID)
	}
	assertOpCount(t, pool, 1) // G6: no duplicate row
}

func TestInsertIdempotentReplayGroup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	when := time.Unix(0, 0).UTC()
	req := api.InsertRequest{
		IdempotencyKey: key(4),
		Operations: []api.InsertOp{
			{OwnerID: 2, ExternalID: "a", Amount: -100, EffectiveAt: when},
			{OwnerID: 2, ExternalID: "b", Amount: 100, EffectiveAt: when},
		},
	}
	first, err := insert(t, pool, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := insert(t, pool, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || *second.TransactionID != *first.TransactionID {
		t.Fatalf("group replay mismatch: %+v vs %+v", second, first)
	}
	if len(second.Operations) != len(first.Operations) {
		t.Fatalf("leg count changed on replay")
	}
	for i := range first.Operations {
		if first.Operations[i].ID != second.Operations[i].ID {
			t.Fatalf("leg %d id changed on replay: %d != %d", i, second.Operations[i].ID, first.Operations[i].ID)
		}
	}
	assertOpCount(t, pool, 2)
}

func TestInsertSameKeyDifferentPayloadConflicts(t *testing.T) {
	pool := dbtest.NewSchema(t)
	when := time.Unix(0, 0).UTC()
	base := api.InsertRequest{
		IdempotencyKey: key(5),
		Operations:     []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: 500, EffectiveAt: when}},
	}
	if _, err := insert(t, pool, base); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same key, different amount → conflict.
	conflicting := base
	conflicting.Operations = []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: 501, EffectiveAt: when}}
	if _, err := insert(t, pool, conflicting); !errors.Is(err, api.ErrPayloadConflict) {
		t.Fatalf("expected ErrPayloadConflict, got %v", err)
	}
	assertOpCount(t, pool, 1) // the conflicting attempt wrote nothing
}

func TestInsertGroupMixedOwnersRejected(t *testing.T) {
	pool := dbtest.NewSchema(t)
	when := time.Unix(0, 0).UTC()
	_, err := insert(t, pool, api.InsertRequest{
		IdempotencyKey: key(6),
		Operations: []api.InsertOp{
			{OwnerID: 1, ExternalID: "a", Amount: -100, EffectiveAt: when},
			{OwnerID: 2, ExternalID: "b", Amount: 100, EffectiveAt: when},
		},
	})
	if !errors.Is(err, api.ErrMixedOwners) {
		t.Fatalf("expected ErrMixedOwners, got %v", err)
	}
	assertOpCount(t, pool, 0) // rejected before any write
}

func TestInsertGroupSizeCap(t *testing.T) {
	pool := dbtest.NewSchema(t)
	when := time.Unix(0, 0).UTC()

	// Default max_group_size is 10; 11 legs must be rejected.
	ops := make([]api.InsertOp, 11)
	for i := range ops {
		ops[i] = api.InsertOp{OwnerID: 3, ExternalID: "acct", Amount: 1, EffectiveAt: when}
	}
	_, err := insert(t, pool, api.InsertRequest{IdempotencyKey: key(7), Operations: ops})
	if !errors.Is(err, api.ErrGroupTooLarge) {
		t.Fatalf("expected ErrGroupTooLarge, got %v", err)
	}
	assertOpCount(t, pool, 0)
}

func TestInsertZeroAmountRejectedByCheck(t *testing.T) {
	pool := dbtest.NewSchema(t)
	_, err := insert(t, pool, api.InsertRequest{
		IdempotencyKey: key(8),
		Operations:     []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: 0, EffectiveAt: time.Unix(0, 0).UTC()}},
	})
	if !errors.Is(err, api.ErrZeroAmount) {
		t.Fatalf("expected ErrZeroAmount, got %v", err)
	}
	assertOpCount(t, pool, 0)
}

func TestInsertRingsDoorbellOnFreshInsert(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	// A dedicated connection listening on the work doorbell.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN work_available"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if _, err := insert(t, pool, api.InsertRequest{
		IdempotencyKey: key(9),
		Operations:     []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: 42, EffectiveAt: time.Unix(0, 0).UTC()}},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("expected doorbell notification: %v", err)
	}
	if n.Channel != "work_available" {
		t.Fatalf("unexpected channel %q", n.Channel)
	}
}

// TestInsertReplayCommitsCleanlyWithoutNewRows asserts the property the task
// calls out: a replay writes no new rows and still commits cleanly (it does not
// error, even though it rang no doorbell).
func TestInsertReplayCommitsCleanlyWithoutNewRows(t *testing.T) {
	pool := dbtest.NewSchema(t)
	req := api.InsertRequest{
		IdempotencyKey: key(10),
		Operations:     []api.InsertOp{{OwnerID: 1, ExternalID: "wallet", Amount: 5, EffectiveAt: time.Unix(0, 0).UTC()}},
	}
	if _, err := insert(t, pool, req); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Replay several times; op count must stay at 1 and every replay commits.
	for i := 0; i < 3; i++ {
		res, err := insert(t, pool, req)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !res.Replayed {
			t.Fatalf("replay %d: expected Replayed", i)
		}
	}
	assertOpCount(t, pool, 1)
}

func assertOpCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM operations").Scan(&got); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	if got != want {
		t.Fatalf("operations rowcount = %d, want %d", got, want)
	}
}
