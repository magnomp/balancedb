//go:build itest

package snapshot_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/snapshot"
)

const acct int64 = 42

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func balOn(t *testing.T, pool *pgxpool.Pool, dayStr string) (int64, bool) {
	t.Helper()
	var b int64
	err := pool.QueryRow(context.Background(),
		`SELECT balance FROM balance_snapshots WHERE account_id = $1 AND day = $2::date`, acct, dayStr).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read snapshot %s: %v", dayStr, err)
	}
	return b, true
}

func apply(t *testing.T, pool *pgxpool.Pool, when time.Time, amount int64) {
	t.Helper()
	if err := snapshot.Apply(context.Background(), pool, acct, when, amount); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

// TestApplyNewDayAndIncrement: a fresh day is created; a second op on the same day
// increments it.
func TestApplyNewDayAndIncrement(t *testing.T) {
	pool := dbtest.NewSchema(t)
	apply(t, pool, day(2026, 8, 22), 100)
	if b, ok := balOn(t, pool, "2026-08-22"); !ok || b != 100 {
		t.Fatalf("after first: %d (ok=%v), want 100", b, ok)
	}
	apply(t, pool, day(2026, 8, 22), 25)
	if b, ok := balOn(t, pool, "2026-08-22"); !ok || b != 125 {
		t.Fatalf("after second same-day: %d (ok=%v), want 125", b, ok)
	}
}

// TestApplyCascadeAndSeed: newest-day common case touches no later rows; a
// backdated op seeds its new day from the prior snapshot and cascades into every
// existing later day (spec §8.4).
func TestApplyCascadeAndSeed(t *testing.T) {
	pool := dbtest.NewSchema(t)

	// Build forward: D2 then D3 (D3 seeds from D2 and touches no later row).
	apply(t, pool, day(2026, 8, 20), 100) // D2 = 100
	apply(t, pool, day(2026, 8, 25), 50)  // D3 = 150
	if b, _ := balOn(t, pool, "2026-08-20"); b != 100 {
		t.Fatalf("D2 = %d, want 100", b)
	}
	if b, _ := balOn(t, pool, "2026-08-25"); b != 150 {
		t.Fatalf("D3 = %d, want 150", b)
	}

	// Backdate D1: new day seeds from nothing (0)+30, cascades +30 into D2 and D3.
	apply(t, pool, day(2026, 8, 10), 30)
	for _, tc := range []struct {
		d string
		w int64
	}{{"2026-08-10", 30}, {"2026-08-20", 130}, {"2026-08-25", 180}} {
		if b, ok := balOn(t, pool, tc.d); !ok || b != tc.w {
			t.Fatalf("%s = %d (ok=%v), want %d", tc.d, b, ok, tc.w)
		}
	}
}
