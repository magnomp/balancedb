package balancedb_test

import (
	"context"
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
