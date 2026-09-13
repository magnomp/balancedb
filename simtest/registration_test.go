package simtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/model"
)

func registrationRequest(key string, amount int64) api.InsertRequest {
	return api.InsertRequest{IdempotencyKey: key, Operations: []api.InsertOp{{
		OwnerID: 1, ExternalID: "wallet", Amount: amount,
		EffectiveAt: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
	}}}
}

func TestReferenceRegistrationVisibility(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("early=%v/commit=%v", early, commit), func(t *testing.T) {
				m := NewModel()
				min := int64(0)
				if err := m.SetLimits(1, "wallet", &min, nil); err != nil {
					t.Fatal(err)
				}
				credit, err := m.BeginRegistration(registrationRequest("00000000-0000-4000-8000-000000000001", 100))
				if err != nil {
					t.Fatal(err)
				}
				debit, err := m.BeginRegistration(registrationRequest("00000000-0000-4000-8000-000000000002", -80))
				if err != nil {
					t.Fatal(err)
				}
				if len(m.ProcessAll()) != 0 {
					t.Fatal("uncommitted work was decided")
				}
				// Higher ID commits first. This does not block on the first host.
				m.CommitRegistration(debit)
				if early {
					decisions := m.ProcessAll()
					if len(decisions) != 1 || decisions[0].OpID != debit.Operations[0].ID || decisions[0].Status != model.OpInvalid {
						t.Fatalf("early debit: %+v", decisions)
					}
				}
				if commit {
					m.CommitRegistration(credit)
				} else {
					m.RollbackRegistration(credit)
				}
				m.ProcessAll()
				want := int64(0)
				status := model.OpInvalid
				if commit {
					want = 100
					if !early {
						want, status = 20, model.OpConfirmed
					}
				}
				if got := m.AccountBalance(1, "wallet"); got != want {
					t.Fatalf("balance %d, want %d", got, want)
				}
				if got := m.ops[debit.Operations[0].ID].Status; got != status {
					t.Fatalf("debit %s, want %s", got, status)
				}
			})
		}
	}
}
