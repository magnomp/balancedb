package simtest

import (
	"errors"
	"testing"

	"github.com/magnomp/balancedb/internal/ledger"
)

func TestReferenceAccountInitialBalance(t *testing.T) {
	for _, bounds := range [][2]int64{{0, 0}, {-10, 10}, {1, 10}, {-10, -1}, {10, -10}} {
		m := NewModel()
		err := m.CreateAccount(1, "wallet", &bounds[0], &bounds[1])
		valid := bounds[0] <= 0 && bounds[1] >= 0
		if valid {
			if err != nil {
				t.Fatal(err)
			}
			if err := m.CreateAccount(1, "wallet", nil, nil); !errors.Is(err, ledger.ErrAccountExists) {
				t.Fatal(err)
			}
		} else {
			if !errors.Is(err, ledger.ErrInvalidLimits) || len(m.accounts) != 0 {
				t.Fatalf("invalid creation persisted: bounds=%v err=%v", bounds, err)
			}
		}
	}
}
