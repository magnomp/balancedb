//go:build itest

package balancedb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb"
	"github.com/magnomp/balancedb/internal/dbtest"
)

func TestAccountWritesStayInHostTransaction(t *testing.T) {
	ctx := context.Background()
	pool, host := dbtest.NewSchema(t), dbtest.NewSchema(t)
	d, _ := openCell(t, pool)
	min, max := int64(0), int64(100)
	tx := begin(t, host, pgx.TxOptions{})
	a, err := d.CreateAccount(ctx, tx, 1, "wallet", balancedb.Limits{MinBalance: &min, MaxBalance: &max})
	if err != nil {
		t.Fatal(err)
	}
	if a.OwnerID != 1 || a.ExternalID != "wallet" || a.ConfirmedBalance != 0 {
		t.Fatalf("account: %+v", a)
	}
	if _, err := d.GetAccount(ctx, 1, "wallet"); !errors.Is(err, balancedb.ErrNotFound) {
		t.Fatalf("uncommitted account visible: %v", err)
	}
	if _, err := d.CreateAccount(ctx, tx, 1, "wallet", balancedb.Limits{}); !errors.Is(err, balancedb.ErrAccountExists) {
		t.Fatalf("duplicate: %v", err)
	}
	bad := int64(1)
	if _, err := d.CreateAccount(ctx, tx, 1, "bad", balancedb.Limits{MinBalance: &bad}); !errors.Is(err, balancedb.ErrInvalidLimits) {
		t.Fatalf("zero outside limits: %v", err)
	}
	a, err = d.UpdateLimits(ctx, tx, 1, "wallet", balancedb.Limits{})
	if err != nil || a.Version != 1 || a.MinBalance != nil || a.MaxBalance != nil {
		t.Fatalf("clear limits: %+v %v", a, err)
	}
	var path string
	if err := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	var hostPath string
	if err := host.QueryRow(ctx, `SELECT current_schema()`).Scan(&hostPath); err != nil {
		t.Fatal(err)
	}
	if path != hostPath {
		t.Fatalf("host path changed: %s != %s", path, hostPath)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = d.GetAccount(ctx, 1, "wallet")
	if err != nil || a.Version != 1 {
		t.Fatalf("committed account: %+v %v", a, err)
	}
	if _, err := d.GetAccount(ctx, 2, "wallet"); !errors.Is(err, balancedb.ErrNotFound) {
		t.Fatalf("owner leaked: %v", err)
	}
	countRows(t, host, `SELECT count(*) FROM accounts`, 0)
	countRows(t, pool, `SELECT count(*) FROM accounts`, 1)

	rollback := begin(t, host, pgx.TxOptions{})
	if _, err := d.UpdateLimits(ctx, rollback, 1, "wallet", balancedb.Limits{MaxBalance: &max}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAccount(ctx, rollback, 1, "discard", balancedb.Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := rollback.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = d.GetAccount(ctx, 1, "wallet")
	if err != nil || a.MaxBalance != nil || a.Version != 1 {
		t.Fatalf("rollback changed account: %+v %v", a, err)
	}
	countRows(t, pool, `SELECT count(*) FROM accounts`, 1)
}

func TestEmbeddedQueriesWithRealProcessor(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewSchema(t)
	d, _ := openCell(t, pool)
	tx := begin(t, pool, pgx.TxOptions{})
	min := int64(0)
	for _, name := range []string{"wallet", "other"} {
		if _, err := d.CreateAccount(ctx, tx, 1, name, balancedb.Limits{MinBalance: &min}); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, tx, `UPDATE config SET loop_interval_ms=5, lease_ttl_ms=1000`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx = begin(t, pool, pgx.TxOptions{})
	credit := request(10)
	credit.Operations[0].Amount = 9007199254740993 // Remains exact beyond float64 precision.
	credit.Operations[0].EffectiveAt = time.Date(2030, 1, 3, 12, 0, 0, 0, time.UTC)
	c, err := d.Insert(ctx, tx, credit)
	if err != nil {
		t.Fatal(err)
	}
	debit := request(11)
	debit.Operations[0].Amount = -80
	debit.Operations[0].EffectiveAt = time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC) // Backdated debit.
	b, err := d.Insert(ctx, tx, debit)
	if err != nil {
		t.Fatal(err)
	}
	group := request(12)
	group.Operations[0].Amount = -9007199254740993
	group.Operations = append(group.Operations, balancedb.InsertOp{OwnerID: 1, ExternalID: "other", Amount: 1, EffectiveAt: group.Operations[0].EffectiveAt})
	g, err := d.Insert(ctx, tx, group)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetOperation(ctx, 1, c.Operations[0].ID); !errors.Is(err, balancedb.ErrNotFound) {
		t.Fatalf("uncommitted operation visible: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := d.GetTransaction(ctx, 1, *g.TransactionID)
	if err != nil || pending.Status != balancedb.TxPending {
		t.Fatalf("pending group: %+v %v", pending, err)
	}
	for _, op := range pending.Operations {
		if op.Status != balancedb.OpPending {
			t.Fatalf("pending leg: %+v", op)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- d.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	eventually(t, func() bool {
		o, err := d.GetTransaction(ctx, 1, *g.TransactionID)
		return err == nil && o.Status == balancedb.TxRejected
	})
	got, err := d.GetTransaction(ctx, 1, *g.TransactionID)
	if err != nil || got.Rejection == nil || got.Rejection.Shortfall != 80 || len(got.Operations) != 2 {
		t.Fatalf("rejected group: %+v %v", got, err)
	}
	for _, op := range got.Operations {
		if op.Status != balancedb.OpInvalid || op.Rejection == nil {
			t.Fatalf("rejected leg: %+v", op)
		}
	}
	for _, owner := range []int64{2, 3} {
		if _, err := d.GetOperation(ctx, owner, c.Operations[0].ID); !errors.Is(err, balancedb.ErrNotFound) {
			t.Fatalf("foreign operation: %v", err)
		}
		if _, err := d.GetTransaction(ctx, owner, *g.TransactionID); !errors.Is(err, balancedb.ErrNotFound) {
			t.Fatalf("foreign group: %v", err)
		}
	}
	final, err := d.GetBalance(ctx, 1, "wallet", nil)
	if err != nil || final.Balance != 9007199254740913 {
		t.Fatalf("final: %+v %v", final, err)
	}
	at := time.Date(2030, 1, 2, 23, 0, 0, 0, time.UTC)
	historical, err := d.GetBalance(ctx, 1, "wallet", &at)
	if err != nil || historical.Balance != -80 {
		t.Fatalf("historical N2: %+v %v", historical, err)
	}
	at = time.Date(2030, 1, 4, 0, 0, 0, 0, time.UTC)
	historical, err = d.GetBalance(ctx, 1, "wallet", &at)
	if err != nil || historical.Balance != final.Balance {
		t.Fatalf("snapshot projection: %+v %v", historical, err)
	}
	page, err := d.GetStatement(ctx, 1, "wallet", balancedb.StatementOptions{Limit: 1})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ID != b.Operations[0].ID || page.Entries[0].RunningBalance != -80 || page.NextCursor == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = d.GetStatement(ctx, 1, "wallet", balancedb.StatementOptions{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ID != c.Operations[0].ID || page.Entries[0].RunningBalance != final.Balance || page.NextCursor != "" {
		t.Fatalf("last page: %+v %v", page, err)
	}
	for _, opts := range []balancedb.StatementOptions{{Limit: -1}, {Limit: 501}, {Cursor: "bad"}} {
		if _, err := d.GetStatement(ctx, 1, "wallet", opts); !errors.Is(err, balancedb.ErrInvalidArgument) {
			t.Fatalf("invalid statement: %v", err)
		}
	}
	limitsTx := begin(t, pool, pgx.TxOptions{})
	badMax := int64(0)
	if _, err := d.UpdateLimits(ctx, limitsTx, 1, "wallet", balancedb.Limits{MaxBalance: &badMax}); !errors.Is(err, balancedb.ErrInvalidLimits) {
		t.Fatalf("limit below final: %v", err)
	}
	if err := limitsTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAccountQueryLifecycleAndValidation(t *testing.T) {
	pool := dbtest.NewSchema(t)
	d, _ := openCell(t, pool)
	ctx := context.Background()
	if _, err := d.GetAccount(ctx, 0, "wallet"); !errors.Is(err, balancedb.ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, err := d.GetTransaction(ctx, 1, 0); !errors.Is(err, balancedb.ErrInvalidArgument) {
		t.Fatal(err)
	}
	tx := begin(t, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if _, err := d.CreateAccount(ctx, tx, 1, "wallet", balancedb.Limits{}); !errors.Is(err, balancedb.ErrUnsupportedIsolation) {
		t.Fatal(err)
	}
	execSQL(t, tx, `SELECT 1`)
	d.Close()
	if _, err := d.GetAccount(ctx, 1, "wallet"); !errors.Is(err, balancedb.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := d.CreateAccount(ctx, tx, 1, "wallet", balancedb.Limits{}); !errors.Is(err, balancedb.ErrClosed) {
		t.Fatal(err)
	}
}
