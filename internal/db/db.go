package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect builds a pgxpool bound to the given schema. Every connection sets
// search_path to that schema (as a quoted identifier) in AfterConnect, so all
// downstream SQL is written unqualified. It pings once before returning so a
// misconfigured DSN or unreachable database fails fast at boot.
func Connect(ctx context.Context, databaseURL, schema string, maxConns int32) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		poolCfg.MaxConns = maxConns
	}

	// pgx.Identifier.Sanitize quotes and escapes the identifier — the schema
	// name is never string-interpolated into SQL (plan §0). It is already
	// validated against a strict regex at config load, this is defense in depth.
	setSearchPath := fmt.Sprintf("SET search_path = %s", pgx.Identifier{schema}.Sanitize())
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, setSearchPath); err != nil {
			return fmt.Errorf("set search_path to %q: %w", schema, err)
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database (check BALANCEDB_DATABASE_URL and that Postgres is reachable): %w", err)
	}

	return pool, nil
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on any error or panic. The rollback error (if the context is still live) is
// ignored: the original error is what the caller needs to see.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
