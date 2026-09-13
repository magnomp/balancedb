//go:build simtest

package simtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/magnomp/balancedb"
	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/dbtest"
)

// Both insertion paths leave transaction lifetime to the host (ADR-0008). These
// schedules deliberately commit a higher-ID debit before a held credit, and
// choose whether the processor sees the debit alone or both operations together.
// Compare to the visibility-aware reference, including terminal group rejection.
func TestSimHostRegistrationVisibility(t *testing.T) {
	for seed := 0; seed < 8; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			pool := dbtest.NewSchema(t)
			setSimConfig(t, pool, 5, 1000, 10)
			var schema string
			if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			d, err := balancedb.Open(ctx, balancedb.Config{DatabaseURL: dbtest.URL(t), Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			// Account creation would introduce ordinary unique-index waits;
			// preexisting accounts isolate transaction visibility from that case.
			const createAccounts = `INSERT INTO accounts (owner_id, external_id, min_balance) VALUES (1, 'wallet', 0), (1, 'other', 0)`
			if _, err := pool.Exec(ctx, createAccounts); err != nil {
				t.Fatal(err)
			}
			m := NewModel()
			min := int64(0)
			for _, name := range []string{"wallet", "other"} {
				if err := m.SetLimits(1, name, &min, nil); err != nil {
					t.Fatal(err)
				}
			}
			early, commit, group := seed&1 != 0, seed&2 != 0, seed&4 != 0
			credit := registrationRequest("00000000-0000-4000-8000-000000000001", int64(100+seed))
			debit := registrationRequest("00000000-0000-4000-8000-000000000002", -80)
			if group {
				debit.Operations = append(debit.Operations, api.InsertOp{OwnerID: 1, ExternalID: "other", Amount: 80, EffectiveAt: debit.Operations[0].EffectiveAt})
			}
			first, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(context.Background())
			creditResult, err := d.Insert(ctx, first, credit)
			if err != nil {
				t.Fatal(err)
			}
			refCredit, err := m.BeginRegistration(credit)
			if err != nil {
				t.Fatal(err)
			}
			second, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Rollback(context.Background())
			// Must finish with the first transaction still open. An insertion
			// lock would make this call hit its context deadline and fail.
			debitResult, err := api.Insert(ctx, second, debit)
			if err != nil {
				t.Fatal(err)
			}
			refDebit, err := m.BeginRegistration(debit)
			if err != nil {
				t.Fatal(err)
			}
			if err := second.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			m.CommitRegistration(refDebit)
			if early {
				runProcessorToDrain(t, pool, 3*time.Second)
				m.ProcessAll()
				requireStatus(t, pool, debitResult.Operations[0].ID, "INVALID")
				requireBalance(t, pool, "wallet", 0)
			}
			if commit {
				if err := first.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				m.CommitRegistration(refCredit)
			} else {
				if err := first.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				m.RollbackRegistration(refCredit)
			}
			m.ProcessAll()
			runProcessorToDrain(t, pool, 3*time.Second)
			for _, name := range []string{"wallet", "other"} {
				requireBalance(t, pool, name, m.AccountBalance(1, name))
			}
			for i, op := range debitResult.Operations {
				requireStatus(t, pool, op.ID, string(m.ops[refDebit.Operations[i].ID].Status))
			}
			if group {
				var status string
				const txStatus = `SELECT status FROM transactions WHERE id=$1`
				if err := pool.QueryRow(ctx, txStatus, *debitResult.TransactionID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if want := string(m.txs[*refDebit.TransactionID].Status); status != want {
					t.Fatalf("group status %s, want %s", status, want)
				}
				assertG2(t, pool, int64(seed))
			}
			if commit {
				requireStatus(t, pool, creditResult.Operations[0].ID, "CONFIRMED")
				if debitResult.Operations[0].ID <= creditResult.Operations[0].ID {
					t.Fatal("expected the debit to have a higher immutable ID")
				}
			}
		})
	}
}
