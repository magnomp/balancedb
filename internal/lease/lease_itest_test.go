//go:build itest

package lease_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/lease"
)

// newLease builds a Lease against the given pool, failing the test on the
// (essentially impossible) UUID-generation error.
func newLease(t *testing.T, pool *pgxpool.Pool) *lease.Lease {
	t.Helper()
	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	return l
}

// forceLease writes the leader_lease row directly so a test can set up a foreign
// or expired holder. lease_until is stamped by the database clock (now() + the
// given millisecond offset, which may be negative for an already-expired lease).
func forceLease(t *testing.T, pool *pgxpool.Pool, owner string, offsetMs int) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE leader_lease SET owner = $1::uuid, lease_until = now() + ($2 * interval '1 millisecond')`,
		owner, offsetMs)
	if err != nil {
		t.Fatalf("force lease: %v", err)
	}
}

func readOwner(t *testing.T, pool *pgxpool.Pool) *string {
	t.Helper()
	var owner *string
	if err := pool.QueryRow(context.Background(),
		`SELECT owner::text FROM leader_lease`).Scan(&owner); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	return owner
}

func TestAcquireEmptyLease(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	l := newLease(t, pool)
	got, err := l.Acquire(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !got || !l.IsLeader() {
		t.Fatalf("expected to acquire the empty lease; got=%v IsLeader=%v", got, l.IsLeader())
	}
	if owner := readOwner(t, pool); owner == nil || *owner != l.Owner() {
		t.Fatalf("owner not stamped: %v want %s", owner, l.Owner())
	}
}

func TestRenewHeldLease(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	l := newLease(t, pool)
	if ok, err := l.Acquire(ctx, 15*time.Second); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}

	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_until FROM leader_lease`).Scan(&before); err != nil {
		t.Fatalf("read lease_until: %v", err)
	}

	if ok, err := l.Acquire(ctx, 30*time.Second); err != nil || !ok {
		t.Fatalf("renew: ok=%v err=%v", ok, err)
	}

	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT lease_until FROM leader_lease`).Scan(&after); err != nil {
		t.Fatalf("read lease_until: %v", err)
	}
	if !after.After(before) {
		t.Fatalf("renew did not extend lease_until: before=%s after=%s", before, after)
	}
	if !l.IsLeader() {
		t.Fatal("still expected to be leader after renew")
	}
}

func TestForeignUnexpiredLeaseNotAcquired(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	other := newLease(t, pool)
	forceLease(t, pool, other.Owner(), 60_000) // held for another minute

	me := newLease(t, pool)
	got, err := me.Acquire(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got || me.IsLeader() {
		t.Fatal("acquired a foreign unexpired lease; must not")
	}
	if owner := readOwner(t, pool); owner == nil || *owner != other.Owner() {
		t.Fatalf("foreign owner was overwritten: %v", owner)
	}
}

func TestExpiredForeignLeaseTakenOver(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	other := newLease(t, pool)
	forceLease(t, pool, other.Owner(), -1_000) // expired one second ago

	me := newLease(t, pool)
	got, err := me.Acquire(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !got || !me.IsLeader() {
		t.Fatal("did not take over an expired foreign lease")
	}
	if owner := readOwner(t, pool); owner == nil || *owner != me.Owner() {
		t.Fatalf("owner not taken over: %v want %s", owner, me.Owner())
	}
}

func TestGracefulRelease(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	l := newLease(t, pool)
	if ok, err := l.Acquire(ctx, 15*time.Second); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if l.IsLeader() {
		t.Fatal("IsLeader true after release")
	}
	if owner := readOwner(t, pool); owner != nil {
		t.Fatalf("owner not cleared on release: %v", *owner)
	}

	// A fresh instance can immediately take over without waiting for TTL.
	other := newLease(t, pool)
	if ok, err := other.Acquire(ctx, 15*time.Second); err != nil || !ok {
		t.Fatalf("takeover after release: ok=%v err=%v", ok, err)
	}
}

// TestReleaseDoesNotStompNewLeader confirms Release is scoped to this instance's
// ownership: after a takeover, the previous holder's late Release is a no-op.
func TestReleaseDoesNotStompNewLeader(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	a := newLease(t, pool)
	if ok, err := a.Acquire(ctx, 15*time.Second); err != nil || !ok {
		t.Fatalf("A acquire: ok=%v err=%v", ok, err)
	}
	// B takes over (simulate A's lease having expired).
	forceLease(t, pool, a.Owner(), -1_000)
	b := newLease(t, pool)
	if ok, err := b.Acquire(ctx, 15*time.Second); err != nil || !ok {
		t.Fatalf("B takeover: ok=%v err=%v", ok, err)
	}
	// A's late graceful release must not clear B's ownership.
	if err := a.Release(ctx); err != nil {
		t.Fatalf("A release: %v", err)
	}
	if owner := readOwner(t, pool); owner == nil || *owner != b.Owner() {
		t.Fatalf("A's release stomped B: owner=%v want %s", owner, b.Owner())
	}
}

// TestMutualExclusion runs two competing instances for 100 rounds and asserts
// they never both hold the lease in the same round, and that exactly one leads
// each round. The TTL comfortably exceeds the loop so, once A wins the first
// round, it renews and B stays standby throughout.
func TestMutualExclusion(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	a := newLease(t, pool)
	b := newLease(t, pool)

	const ttl = 30 * time.Second
	for round := 0; round < 100; round++ {
		aLeader, err := a.Acquire(ctx, ttl)
		if err != nil {
			t.Fatalf("round %d: A acquire: %v", round, err)
		}
		bLeader, err := b.Acquire(ctx, ttl)
		if err != nil {
			t.Fatalf("round %d: B acquire: %v", round, err)
		}
		if aLeader && bLeader {
			t.Fatalf("round %d: both instances hold the lease", round)
		}
		if !aLeader && !bLeader {
			t.Fatalf("round %d: neither instance holds the lease", round)
		}
		// Cached state must agree with the returned decision.
		if a.IsLeader() != aLeader || b.IsLeader() != bLeader {
			t.Fatalf("round %d: IsLeader disagrees with Acquire", round)
		}
	}
}
