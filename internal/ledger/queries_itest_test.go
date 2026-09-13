//go:build itest

package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
)

// afterScan commits a competing write after a chosen read has taken its snapshot.
type (
	afterScan struct {
		Queryer
		calls, trigger int
		fn             func()
	}
	hookedRow struct {
		pgx.Row
		fn func()
	}
)

func (r hookedRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		r.fn()
	}
	return err
}

func (q *afterScan) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.calls++
	row := q.Queryer.QueryRow(ctx, sql, args...)
	if q.calls == q.trigger {
		return hookedRow{row, q.fn}
	}
	return row
}

func TestProjectionUsesOneSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	const seed = `INSERT INTO accounts (owner_id,external_id,confirmed_balance) VALUES (1,'wallet',100);
INSERT INTO operations (account_id,amount,effective_at,status) SELECT id,100,'2030-01-03 12:00Z','CONFIRMED' FROM accounts;
INSERT INTO balance_snapshots (account_id,day,balance) SELECT id,'2030-01-03',100 FROM accounts;`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatal(err)
	}
	value, err := db.Read(ctx, pool, func(tx pgx.Tx) (*Balance, error) {
		q := &afterScan{Queryer: tx, trigger: 2, fn: func() {
			// Commit after reading the previous-day snapshot but before summing today's
			// operations. Without a consistent snapshot the result would be 125, which
			// is neither the before balance 100 nor the after balance 175.
			const change = `UPDATE accounts SET confirmed_balance=175,version=version+1;
INSERT INTO operations (account_id,amount,effective_at,status) SELECT id,50,'2030-01-02 12:00Z','CONFIRMED' FROM accounts;
INSERT INTO operations (account_id,amount,effective_at,status) SELECT id,25,'2030-01-03 13:00Z','CONFIRMED' FROM accounts;
INSERT INTO balance_snapshots (account_id,day,balance) SELECT id,'2030-01-02',50 FROM accounts;
UPDATE balance_snapshots SET balance=175 WHERE day='2030-01-03';`
			if _, err := pool.Exec(ctx, change); err != nil {
				t.Fatal(err)
			}
		}}
		at := time.Date(2030, 1, 3, 23, 0, 0, 0, time.UTC)
		return GetBalance(ctx, q, 1, "wallet", &at)
	})
	if err != nil || value.Balance != 100 {
		t.Fatalf("mixed projection: %+v %v", value, err)
	}
	fresh, err := db.Read(ctx, pool, func(tx pgx.Tx) (*Balance, error) { return GetBalance(ctx, tx, 1, "wallet", nil) })
	if err != nil || fresh.Balance != 175 {
		t.Fatalf("competing commit missing: %+v %v", fresh, err)
	}
}

func TestGroupOutcomeUsesOneSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	req := InsertRequest{IdempotencyKey: "00000000-0000-4000-8000-000000000001", Operations: []InsertOp{
		{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		{OwnerID: 1, ExternalID: "b", Amount: 1, EffectiveAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
	}}
	res, err := Insert(ctx, tx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := db.Read(ctx, pool, func(tx pgx.Tx) (*TransactionOutcome, error) {
		q := &afterScan{Queryer: tx, trigger: 1, fn: func() {
			// Terminal parent/leg changes are committed together, as in the processor.
			const change = `UPDATE transactions SET status='REJECTED'; UPDATE operations SET status='INVALID';`
			if _, err := pool.Exec(ctx, change); err != nil {
				t.Fatal(err)
			}
		}}
		return GetTransaction(ctx, q, 1, *res.TransactionID)
	})
	if err != nil || value.Status != "PENDING" {
		t.Fatalf("parent: %+v %v", value, err)
	}
	for _, leg := range value.Operations {
		if leg.Status != "PENDING" {
			t.Fatalf("mixed parent/leg snapshots: %+v", value)
		}
	}
}

func TestLimitCASRevalidatesAndRetries(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	if _, err := CreateAccount(ctx, pool, 1, "wallet", Limits{}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	max := int64(100)
	a, err := UpdateLimits(ctx, pool, 1, "wallet", Limits{MaxBalance: &max}, func(ctx context.Context) {
		calls++
		if calls == 1 {
			const stmt = `UPDATE accounts SET version=version+1`
			if _, err := pool.Exec(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil || a.Version != 2 || calls != 2 {
		t.Fatalf("retry: %+v calls=%d %v", a, calls, err)
	}
	// A first miss must re-check the new balance before trying another CAS.
	calls = 0
	_, err = UpdateLimits(ctx, pool, 1, "wallet", Limits{MaxBalance: &max}, func(ctx context.Context) {
		calls++
		const stmt = `UPDATE accounts SET confirmed_balance=150,max_balance=NULL,version=version+1`
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrInvalidLimits) || calls != 1 {
		t.Fatalf("revalidation: calls=%d %v", calls, err)
	}
}
