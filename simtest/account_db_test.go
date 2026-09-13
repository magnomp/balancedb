//go:build simtest

package simtest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/ledger"
)

func TestSimAccountCreationMatchesReference(t *testing.T) {
	pool := dbtest.NewSchema(t)
	m := NewModel()
	for i, bounds := range [][2]int64{{0, 0}, {-10, 10}, {1, 10}, {-10, -1}, {10, -10}} {
		name := fmt.Sprintf("account_%d", i)
		want := m.CreateAccount(1, name, &bounds[0], &bounds[1])
		_, got := ledger.CreateAccount(context.Background(), pool, 1, name, ledger.Limits{MinBalance: &bounds[0], MaxBalance: &bounds[1]})
		if !errors.Is(got, want) {
			t.Fatalf("bounds=%v: got %v, want %v", bounds, got, want)
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(m.accounts) {
		t.Fatalf("persisted %d, want %d", count, len(m.accounts))
	}
}
