package lease

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// execer is the subset of pgxpool.Pool the lease needs. Acquire and Release each
// run one autocommit statement, so a pool (or any pgx querier) satisfies it. The
// lease is deliberately not transactional: it is arbitrated by the leader_lease
// row, not by a connection or transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// The spec's single-UPDATE acquire/renew (§7.1). The row is claimed when this
// instance already owns it (renew), when it is unowned, or when the current
// lease has expired by the database clock. lease_until is stamped now() + ttl,
// with ttl passed as milliseconds so the caller sources it from the config table
// (lease_ttl_ms) without a Go-side time comparison.
const (
	acquireSQL = `UPDATE leader_lease
   SET owner = $1::uuid, lease_until = now() + ($2 * interval '1 millisecond')
 WHERE owner = $1::uuid OR owner IS NULL OR lease_until < now()`

	// Release clears ownership only if this instance still holds it, so it never
	// stomps a lease a new leader has already taken over.
	releaseSQL = `UPDATE leader_lease
   SET owner = NULL, lease_until = NULL
 WHERE owner = $1::uuid`
)

// Lease tracks one processor instance's claim on the leader_lease row. It is not
// safe for concurrent use: the owning processor loop calls it single-threaded.
type Lease struct {
	db    execer
	owner string
	held  bool
}

// New builds a Lease with a fresh boot UUID. The UUID identifies this instance
// for the lifetime of the process (spec §7.1).
func New(db execer) (*Lease, error) {
	owner, err := newUUID()
	if err != nil {
		return nil, fmt.Errorf("lease: generate instance uuid: %w", err)
	}
	return &Lease{db: db, owner: owner}, nil
}

// Owner returns this instance's boot UUID (for logging).
func (l *Lease) Owner() string { return l.owner }

// IsLeader reports whether the last Acquire claimed the lease. It reflects cached
// state from the most recent call; the loop keeps it current by calling Acquire
// each cycle. Release resets it to false.
func (l *Lease) IsLeader() bool { return l.held }

// Acquire runs the spec's single-UPDATE acquire/renew and returns whether this
// instance holds the lease for the coming cycle. ttl is the lease duration
// (sourced from config lease_ttl_ms); the expiry is computed by the database
// clock inside the UPDATE, never from time.Now. A returned error leaves the
// cached leader state unchanged; on success IsLeader mirrors the result.
func (l *Lease) Acquire(ctx context.Context, ttl time.Duration) (bool, error) {
	tag, err := l.db.Exec(ctx, acquireSQL, l.owner, ttl.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("lease: acquire/renew: %w", err)
	}
	l.held = tag.RowsAffected() == 1
	return l.held, nil
}

// Release clears this instance's ownership on graceful shutdown so a standby can
// take over immediately without waiting for TTL expiry. It is idempotent and a
// no-op if the lease is already owned by someone else. It always marks this
// instance as no longer leader.
func (l *Lease) Release(ctx context.Context) error {
	l.held = false
	if _, err := l.db.Exec(ctx, releaseSQL, l.owner); err != nil {
		return fmt.Errorf("lease: release: %w", err)
	}
	return nil
}

// newUUID generates a random (version 4) UUID string using crypto/rand — no
// external dependency. The value is only an opaque instance identifier stored in
// the owner column.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
