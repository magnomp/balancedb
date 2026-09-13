package balancedb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/config"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/migrate"
	"github.com/magnomp/balancedb/internal/processor"
)

var (
	ErrClosed         = errors.New("balancedb: closed")
	ErrAlreadyRunning = errors.New("balancedb: processor already running")
)

// Config is deployment configuration for an embedded cell. It reads no env vars.
// Behavioral settings (lease TTL, batch size, etc.) remain in the DB config table.
type Config struct {
	DatabaseURL string
	Schema      string       // Default: balancedb.
	MaxConns    int32        // Background pool size; default 10, minimum 2 (LISTEN + work).
	Logger      *slog.Logger // Default: slog.Default(); never changes the global logger.
}

func (c Config) normalized() (Config, error) {
	if c.Schema == "" {
		c.Schema = "balancedb"
	}
	if err := config.ValidateSchema(c.Schema); err != nil {
		return Config{}, fmt.Errorf("balancedb schema: %w", err)
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("balancedb: DatabaseURL is required")
	}
	if c.MaxConns == 0 {
		c.MaxConns = 10
	}
	if c.MaxConns < 2 {
		return Config{}, errors.New("balancedb: MaxConns must be at least 2")
	}
	return c, nil
}

// Migrate applies the embedded, forward-only migrations and returns the versions
// applied by this call. It creates the target schema when absent. Concurrent
// calls from application replicas serialize per schema and are safe. Run this
// before Open/Run, at startup or in a separate deployment migration step.
func Migrate(ctx context.Context, cfg Config) ([]int, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return migrate.Run(ctx, cfg.DatabaseURL, cfg.Schema)
}

// DB is an embedded cell handle. Open constructs it; its zero value is not usable.
// Insert is safe to call concurrently with distinct host transactions. Run permits
// one loop per handle. Independent handles/processes coordinate through the lease.
type DB struct {
	schema string
	pool   *pgxpool.Pool
	proc   *processor.Processor

	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
	done   chan struct{}
}

// Open connects a separate schema-bound pool for background processing. It does
// not migrate, start processing, or take ownership of any host pool/transaction.
// Call Close after use. Insert uses only its supplied transaction, not this pool.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.Schema, cfg.MaxConns)
	if err != nil {
		return nil, err
	}
	l, err := lease.New(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{schema: cfg.Schema, pool: pool, proc: processor.New(pool, l, cfg.Logger)}, nil
}

// Run blocks until ctx is cancelled or Close is called, then releases leadership
// and returns nil. Run it in a host-managed goroutine on every replica. Standbys
// automatically take over after lease release or expiry; no external coordinator
// is needed. A second concurrent Run on the same handle returns ErrAlreadyRunning.
func (d *DB) Run(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	if d.done != nil {
		d.mu.Unlock()
		return ErrAlreadyRunning
	}
	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.done = make(chan struct{})
	d.mu.Unlock()

	defer func() {
		cancel()
		d.mu.Lock()
		close(d.done)
		d.done = nil
		d.cancel = nil
		d.mu.Unlock()
	}()
	return d.proc.Run(ctx)
}

// IsLeader reports the loop's last observed leadership state. This is diagnostic
// information, not authority to write balances: every decision uses DB guards.
func (d *DB) IsLeader() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closed && d.done != nil && d.proc.IsLeader()
}

// Close cancels and joins the processor, then closes only this handle's pool.
// It is safe to call repeatedly. The host still owns all insertion transactions.
func (d *DB) Close() {
	d.mu.Lock()
	d.closed = true
	done := d.done
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	if done != nil {
		<-done
	}
	d.pool.Close()
}
