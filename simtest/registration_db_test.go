//go:build simtest

package simtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/magnomp/balancedb"
	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/model"
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

// TestSimHostRegistrationEditVisibility (SIM-003) extends the ADR-0008 visibility
// schedules with edits (ADR-0010). A host transaction registers the target and
// holds it open; an edit of it from another transaction is refused on both sides
// (the uncommitted target is invisible to the ledger's SELECT, so
// ErrEditTargetNotFound — never a foreign-key wait: an edit row cannot reference
// what its transaction cannot see). Once the target commits — still PENDING —
// a single edit and a mixed group (edit + new leg) are registered against it,
// optionally with an early drain in between, and the real processor and the
// reference must agree on every status, the target's current columns and
// revision, the history rows and the balances: a target that ends INVALID
// rejects both units with TARGET_NOT_EDITABLE; a CONFIRMED one applies the
// single edit first (id order) and the group after it, at revision 2 then 3. The
// processor's own deferral path (an edit selected while its target is PENDING)
// is unreachable through committed inserts and is driven directly by the
// processor's IT-042 and group-deferral tests.
func TestSimHostRegistrationEditVisibility(t *testing.T) {
	for seed := 0; seed < 8; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
			// invalidTarget: the target is a debit that breaches min 0, so it ends
			// INVALID and every edit of it is TARGET_NOT_EDITABLE. earlyDrain: the
			// target is decided before the edits are registered. groupFirst: the
			// mixed group is registered before the single edit (revision order flips).
			invalidTarget, earlyDrain, groupFirst := seed&1 != 0, seed&2 != 0, seed&4 != 0
			amount := int64(100)
			if invalidTarget {
				amount = -80
			}
			targetReq := registrationRequest("00000000-0000-4000-8000-000000000001", amount)

			// H1 registers the target and holds it open.
			first, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(context.Background())
			targetRes, err := d.Insert(ctx, first, targetReq)
			if err != nil {
				t.Fatal(err)
			}
			refTarget, err := m.BeginRegistration(targetReq)
			if err != nil {
				t.Fatal(err)
			}
			target, refTargetID := targetRes.Operations[0].ID, refTarget.Operations[0].ID

			// An edit of the uncommitted target is refused on both sides, without
			// waiting on H1.
			editOf := func(id int64, key string) api.InsertRequest {
				return api.InsertRequest{IdempotencyKey: key, Operations: []api.InsertOp{{OwnerID: 1, EditOf: &id, Amount: 60}}}
			}
			second, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Rollback(context.Background())
			if _, err := api.Insert(ctx, second, editOf(target, "00000000-0000-4000-8000-000000000002")); !errors.Is(err, api.ErrEditTargetNotFound) {
				t.Fatalf("db edit of an uncommitted target: %v, want ErrEditTargetNotFound", err)
			}
			if err := second.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Insert(editOf(refTargetID, "00000000-0000-4000-8000-000000000002")); !errors.Is(err, api.ErrEditTargetNotFound) {
				t.Fatalf("reference edit of an uncommitted target: %v, want ErrEditTargetNotFound", err)
			}

			// H1 commits: the target is visible and PENDING.
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			m.CommitRegistration(refTarget)
			if earlyDrain {
				runProcessorToDrain(t, pool, 5*time.Second)
				m.ProcessAll()
			}

			// A single edit and a mixed group against the (PENDING or decided) target.
			single := editOf(target, "00000000-0000-4000-8000-000000000003")
			refSingle := editOf(refTargetID, "00000000-0000-4000-8000-000000000003")
			group := api.InsertRequest{IdempotencyKey: "00000000-0000-4000-8000-000000000004", Operations: []api.InsertOp{
				{OwnerID: 1, EditOf: &target, EffectiveAt: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)},
				{OwnerID: 1, ExternalID: "other", Amount: 25, EffectiveAt: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)},
			}}
			refGroup := group
			refGroup.Operations = append([]api.InsertOp(nil), group.Operations...)
			refGroup.Operations[0].EditOf = &refTargetID
			reqs := [][2]api.InsertRequest{{single, refSingle}, {group, refGroup}}
			if groupFirst {
				reqs[0], reqs[1] = reqs[1], reqs[0]
			}
			var (
				dbRes  []*api.InsertResult
				refRes []*api.InsertResult
			)
			for _, pair := range reqs {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				res, err := d.Insert(ctx, tx, pair[0])
				if err != nil {
					t.Fatalf("db edit registration: %v", err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				ref, err := m.Insert(pair[1])
				if err != nil {
					t.Fatalf("reference edit registration: %v", err)
				}
				dbRes, refRes = append(dbRes, res), append(refRes, ref)
			}

			runProcessorToDrain(t, pool, 5*time.Second)
			m.ProcessAll()
			assertG2(t, pool, int64(seed))

			// Outcomes, current columns, history and balances agree with the reference.
			wantTarget := m.ops[refTargetID]
			requireStatus(t, pool, target, string(wantTarget.Status))
			var (
				gotAmount    int64
				gotEffective time.Time
				gotRevision  int32
				gotRevisions int
			)
			if err := pool.QueryRow(ctx, `SELECT amount, effective_at, revision FROM operations WHERE id=$1`, target).Scan(&gotAmount, &gotEffective, &gotRevision); err != nil {
				t.Fatal(err)
			}
			if gotAmount != wantTarget.Amount || !gotEffective.Equal(wantTarget.EffectiveAt) || gotRevision != wantTarget.Revision {
				t.Fatalf("target db={%d, %v, rev %d}, reference={%d, %v, rev %d}", gotAmount, gotEffective, gotRevision, wantTarget.Amount, wantTarget.EffectiveAt, wantTarget.Revision)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM operation_revisions WHERE operation_id=$1`, target).Scan(&gotRevisions); err != nil {
				t.Fatal(err)
			}
			if gotRevisions != len(m.revisions[refTargetID]) {
				t.Fatalf("target has %d history rows, reference %d", gotRevisions, len(m.revisions[refTargetID]))
			}
			for i := range dbRes {
				for j, op := range dbRes[i].Operations {
					want := m.ops[refRes[i].Operations[j].ID]
					requireStatus(t, pool, op.ID, string(want.Status))
					if want.EditOf != nil && want.Status == model.OpInvalid {
						var reason string
						if err := pool.QueryRow(ctx, `SELECT invalidation_reason FROM operations WHERE id=$1`, op.ID).Scan(&reason); err != nil {
							t.Fatal(err)
						}
						rej, err := model.ParseRejection(reason)
						if err != nil || rej.Code != model.ReasonTargetNotEditable || rej.OperationID == nil || *rej.OperationID != target {
							t.Fatalf("rejected edit %d reason %s (%v), want TARGET_NOT_EDITABLE{%d}", op.ID, reason, err, target)
						}
					}
				}
			}
			if invalidTarget {
				if wantTarget.Revision != 1 || gotRevisions != 0 {
					t.Fatalf("an INVALID target was edited: revision %d, %d history rows", wantTarget.Revision, gotRevisions)
				}
			} else if wantTarget.Revision != 3 || gotRevisions != 2 {
				t.Fatalf("both edits should have applied: revision %d, %d history rows", wantTarget.Revision, gotRevisions)
			}
			for _, name := range []string{"wallet", "other"} {
				requireBalance(t, pool, name, m.AccountBalance(1, name))
			}
		})
	}
}
