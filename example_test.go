package balancedb_test

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb"
)

func ExampleDB_Insert() {
	ctx := context.Background()
	cfg := balancedb.Config{
		DatabaseURL: "postgres://user:password@localhost/app?sslmode=disable",
		Schema:      "ledger",
	}
	if _, err := balancedb.Migrate(ctx, cfg); err != nil {
		log.Print(err)
		return
	}
	ledger, err := balancedb.Open(ctx, cfg)
	if err != nil {
		log.Print(err)
		return
	}
	defer ledger.Close()

	// Every host replica can run a processor: the DB lease selects one leader.
	done := make(chan error, 1)
	go func() { done <- ledger.Run(ctx) }()
	defer func() {
		ledger.Close()
		if err := <-done; err != nil {
			log.Print(err)
		}
	}()

	host, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Print(err)
		return
	}
	defer host.Close(ctx)
	tx, err := host.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		log.Print(err)
		return
	}
	defer tx.Rollback(ctx)

	min := int64(0)
	if _, err := ledger.CreateAccount(ctx, tx, 1, "wallet", balancedb.Limits{MinBalance: &min}); err != nil {
		log.Print(err)
		return
	}

	// Execute the host's business SQL on tx here, before registering ledger work.
	_, err = ledger.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000001",
		Operations: []balancedb.InsertOp{{
			OwnerID: 1, ExternalID: "wallet", Amount: 1250,
			EffectiveAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		}},
	})
	if err != nil {
		log.Print(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Print(err)
		return
	}
	// Reads use the ledger pool and see committed state; confirmation is async.
	balance, err := ledger.GetBalance(ctx, 1, "wallet", nil)
	if err != nil {
		log.Print(err)
		return
	}
	log.Printf("confirmed balance: %d", balance.Balance)
}

// ExampleDB_Insert_edit registers an edit of an existing operation inside the
// host's own transaction, then a grouped edit mixed with a new operation — one
// atomic unit with the same rules as a grouped insert (ADR-0010).
func ExampleDB_Insert_edit() {
	ctx := context.Background()
	cfg := balancedb.Config{
		DatabaseURL: "postgres://user:password@localhost/app?sslmode=disable",
		Schema:      "ledger",
	}
	ledger, err := balancedb.Open(ctx, cfg)
	if err != nil {
		log.Print(err)
		return
	}
	defer ledger.Close()

	host, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Print(err)
		return
	}
	defer host.Close(ctx)
	tx, err := host.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		log.Print(err)
		return
	}
	defer tx.Rollback(ctx)

	// Edit one operation in place: zero-valued fields mean "unchanged", so this
	// keeps the account and the instant and changes only the amount. The result
	// is a PENDING edit registration naming its target (EditOf); the leader
	// applies it and appends the superseded state to the operation's history.
	target := int64(41)
	res, err := ledger.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000002",
		Operations: []balancedb.InsertOp{
			{OwnerID: 1, EditOf: &target, Amount: -1200},
		},
	})
	switch {
	case errors.Is(err, balancedb.ErrEditTargetNotFound):
		log.Print("no such operation for this owner") // the savepoint rolled back; tx is still usable
		return
	case errors.Is(err, balancedb.ErrEditsDisabled):
		log.Print("editing is disabled for this cell")
		return
	case err != nil:
		log.Print(err)
		return
	}
	_ = res.Operations[0].EditOf // == &target; Status is "PENDING"

	// Grouped: two edits plus a new operation, one atomic unit. The optional
	// ExpectedRevision guard rejects the whole group with STALE_REVISION unless
	// the target is at that revision when decided.
	other, rev := int64(42), int32(1)
	_, err = ledger.Insert(ctx, tx, balancedb.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000003",
		Operations: []balancedb.InsertOp{
			{OwnerID: 1, EditOf: &target, EffectiveAt: time.Date(2026, 2, 10, 9, 30, 0, 0, time.UTC)},
			{OwnerID: 1, EditOf: &other, Amount: 900, ExpectedRevision: &rev},
			{OwnerID: 1, ExternalID: "cash", Amount: 300, EffectiveAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)},
		},
	})
	if err != nil {
		log.Print(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Print(err)
	}
}
