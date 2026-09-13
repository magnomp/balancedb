package balancedb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/ledger"
)

// Insertion types are shared with the HTTP API. Amounts are int64 minor units.
type (
	InsertOp      = ledger.InsertOp
	InsertRequest = ledger.InsertRequest
	OpOutcome     = ledger.OpOutcome
	InsertResult  = ledger.InsertResult
)

// Insertion errors support errors.Is, including through wrapped SQL errors.
var (
	ErrNoOperations          = ledger.ErrNoOperations
	ErrInvalidIdempotencyKey = ledger.ErrInvalidIdempotencyKey
	ErrMixedOwners           = ledger.ErrMixedOwners
	ErrGroupTooLarge         = ledger.ErrGroupTooLarge
	ErrPayloadConflict       = ledger.ErrPayloadConflict
	ErrZeroAmount            = ledger.ErrZeroAmount
	ErrUnsupportedIsolation  = errors.New("balancedb: insertion requires READ COMMITTED")
)

// Insert registers one single operation or an atomic group in a caller-owned
// READ COMMITTED pgx transaction in this cell's database. Accounts are created on
// demand with unbounded limits. No REST call or pool checkout occurs.
//
// A savepoint contains all ledger writes and schema changes; an error rolls it
// back, allowing the host to handle the error without accidentally committing a
// partial group. Success releases the savepoint and restores the host search_path
// but DOES NOT commit the host transaction. Returned IDs/statuses are provisional
// until that commit. NOTIFY is delivered only by the enclosing commit.
//
// Insert as close as possible to the host commit and keep the transaction short.
// Insertion is not serialized: a later ID can commit and be decided while an
// earlier operation is still uncommitted, changing acceptance outcomes (ADR-0008).
// Inserting at the end reduces this risk but does not guarantee global ID order.
// Never wait for a decision before committing, access this pgx.Tx concurrently,
// or write ledger tables directly.
func (d *DB) Insert(ctx context.Context, tx pgx.Tx, req InsertRequest) (result *InsertResult, err error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if tx == nil {
		return nil, errors.New("balancedb: transaction is required")
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("balancedb savepoint: %w", err)
	}
	defer func() {
		// Also rolls back on panic. A separate bounded context permits cleanup
		// after a cancelled request; a lost connection still requires host rollback.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := sp.Rollback(cleanupCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("balancedb rollback savepoint: %w", rollbackErr))
			result = nil
		}
	}()

	const settings = `SELECT current_setting('search_path'), current_setting('transaction_isolation')`
	var previousPath, isolation string
	if err := sp.QueryRow(ctx, settings).Scan(&previousPath, &isolation); err != nil {
		return nil, fmt.Errorf("balancedb transaction settings: %w", err)
	}
	if isolation != "read committed" {
		return nil, ErrUnsupportedIsolation
	}
	// pg_temp is explicit and last so host temporary tables cannot shadow the
	// ledger. pg_catalog first also prevents host functions shadowing builtins.
	path := "pg_catalog, " + pgx.Identifier{d.schema}.Sanitize() + ", pg_temp"
	if err := setLocalPath(ctx, sp, path); err != nil {
		return nil, err
	}
	result, err = ledger.Insert(ctx, sp, req)
	if err != nil {
		return nil, err
	}
	if err := setLocalPath(ctx, sp, previousPath); err != nil {
		return nil, err
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, fmt.Errorf("balancedb release savepoint: %w", err)
	}
	return result, nil
}

func setLocalPath(ctx context.Context, tx pgx.Tx, path string) error {
	const stmt = `SELECT pg_catalog.set_config('search_path', $1, true)`
	if _, err := tx.Exec(ctx, stmt, path); err != nil {
		return fmt.Errorf("balancedb set transaction search_path: %w", err)
	}
	return nil
}
