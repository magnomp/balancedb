package balancedb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// withSchema isolates one embedded write using a savepoint in the host transaction.
func withSchema[T any](ctx context.Context, d *DB, tx pgx.Tx, fn func(pgx.Tx) (T, error)) (result T, err error) {
	var zero T
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return zero, ErrClosed
	}
	if tx == nil {
		return zero, errors.New("balancedb: transaction is required")
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return zero, fmt.Errorf("balancedb savepoint: %w", err)
	}
	defer func() {
		// Also rolls back on panic. A separate bounded context permits cleanup
		// after a cancelled request; a lost connection still requires host rollback.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := sp.Rollback(cleanupCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("balancedb rollback savepoint: %w", rollbackErr))
			result = zero
		}
	}()

	const settings = `SELECT current_setting('search_path'), current_setting('transaction_isolation')`
	var previousPath, isolation string
	if err := sp.QueryRow(ctx, settings).Scan(&previousPath, &isolation); err != nil {
		return zero, fmt.Errorf("balancedb transaction settings: %w", err)
	}
	if isolation != "read committed" {
		return zero, ErrUnsupportedIsolation
	}
	// pg_temp is explicit and last so host temporary tables cannot shadow the
	// ledger. pg_catalog first also prevents host functions shadowing builtins.
	path := "pg_catalog, " + pgx.Identifier{d.schema}.Sanitize() + ", pg_temp"
	if err := setLocalPath(ctx, sp, path); err != nil {
		return zero, err
	}
	result, err = fn(sp)
	if err != nil {
		return zero, err
	}
	if err := setLocalPath(ctx, sp, previousPath); err != nil {
		return zero, err
	}
	if err := sp.Commit(ctx); err != nil {
		return zero, fmt.Errorf("balancedb release savepoint: %w", err)
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
