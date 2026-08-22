package processor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/model"
)

// configRetryBackoff is the fixed pause after a failed config read before the
// loop retries — long enough not to spin on a transient DB problem.
const configRetryBackoff = 2 * time.Second

// errGuardMiss is the sentinel a processing transaction returns when one of the
// three guards (spec §7.2) matches 0/mismatched rows. It is normal control flow,
// not a failure: the transaction rolls back, the loop re-acquires the lease and
// continues. It never escapes Run.
var errGuardMiss = errors.New("processor: guard miss")

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	selectConfig = `SELECT lease_ttl_ms, loop_interval_ms, batch_size, max_group_size, api_max_wait_ms FROM config`

	// The full spec §8.1 work select: every PENDING operation — singles and group
	// legs alike — ordered by id (the work index idx_ops_work is on (id) WHERE
	// status='PENDING'). transaction_id NULL marks a single; otherwise the row is a
	// group leg and the group is decided at its FIRST leg by id, later legs skipped
	// by status. Ordering is taken only from here (G3), never from doorbell arrival.
	fetchPendingWork = `SELECT id, account_id, amount, effective_at, transaction_id
  FROM operations
 WHERE status = 'PENDING'
 ORDER BY id
 LIMIT $1`

	// The doorbell channel is the literal global work_available (ADR-0002); the api
	// insert path rings the same channel. It is per-database, so a processor may be
	// woken by inserts to another schema in the same database — harmless: the leader
	// re-selects only its own schema's PENDING work and sleeps again if there is none.
	listenDoorbell = `LISTEN work_available`
)

// pendingOp is the slice of an operations row the single-op path needs.
type pendingOp struct {
	ID          int64
	AccountID   int64
	Amount      int64
	EffectiveAt time.Time
}

// workRow is one PENDING operation as the §8.1 dispatcher sees it. TransactionID
// is nil for a single and set for a group leg.
type workRow struct {
	ID            int64
	AccountID     int64
	Amount        int64
	EffectiveAt   time.Time
	TransactionID *int64
}

// Processor is one cell's decision engine. It owns a pgx pool, the leader lease,
// and a dedicated doorbell listen connection. It is single-threaded: Run drives
// everything; nothing here is safe for concurrent use.
type Processor struct {
	pool  *pgxpool.Pool
	lease *lease.Lease
	log   *slog.Logger

	// listenConn is the dedicated LISTEN work_available connection, lazily armed
	// the first time the leader idle-waits and re-armed if it breaks.
	listenConn *pgxpool.Conn

	// Test seams; always nil in production.
	//
	// afterAccountRead runs between the account read and the Guard 3 CAS in both the
	// single and the group path, letting a test commit a concurrent account mutation
	// to exercise the version-CAS miss path deterministically.
	afterAccountRead func()
	// enteredIdleWait fires once the leader has armed its listener and is about to
	// block on the doorbell, so a timing test can insert without racing the listen
	// registration.
	enteredIdleWait func()
}

// New builds a Processor over the given pool and lease.
func New(pool *pgxpool.Pool, l *lease.Lease, log *slog.Logger) *Processor {
	if log == nil {
		log = slog.Default()
	}
	return &Processor{pool: pool, lease: l, log: log}
}

// Run drives the loop until ctx is cancelled, then releases the lease and returns
// nil. Transient errors (a failed config read, a guard miss, a dropped doorbell
// connection) are logged and folded back into the loop — only ctx cancellation
// ends it.
func (p *Processor) Run(ctx context.Context) error {
	defer p.releaseOnExit()
	defer p.dropListener()

	p.log.Info("processor loop starting", "owner", p.lease.Owner())

	for {
		if ctx.Err() != nil {
			return nil
		}

		cfg, err := p.readConfig(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Cannot read config: back off a fixed interval and retry rather than
			// spin. The config row is created by migrations, so this is a transient
			// DB problem, not a permanent one.
			p.log.Error("read config", "err", err)
			if sleepCtx(ctx, configRetryBackoff) != nil {
				return nil
			}
			continue
		}

		ttl := time.Duration(cfg.LeaseTTLMs) * time.Millisecond
		loopInterval := clampInterval(time.Duration(cfg.LoopIntervalMs) * time.Millisecond)

		leader, err := p.lease.Acquire(ctx, ttl)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			p.log.Error("acquire lease", "err", err)
			if sleepCtx(ctx, loopInterval) != nil {
				return nil
			}
			continue
		}

		if !leader {
			// Standby: do not listen; just poll for the lease next interval.
			if sleepCtx(ctx, loopInterval) != nil {
				return nil
			}
			continue
		}

		if err := p.drain(ctx, cfg.BatchSize); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errGuardMiss) {
				// Rollback already happened; re-acquire on the next iteration.
				p.log.Debug("guard miss during drain; re-acquiring lease")
			} else {
				p.log.Error("drain", "err", err)
			}
			// Either way fall through to the idle wait, then loop.
		}

		deadline := idleDeadline(loopInterval, ttl)
		if err := p.idleWaitLeader(ctx, deadline); err != nil {
			return nil
		}
	}
}

// drain fetches PENDING work ordered by id and decides it in batches, repeating
// until a select returns nothing (spec §8.1). Each batch is one DB transaction
// packing up to batchSize operations' worth of decisions (spec §8.5). A guard miss
// (or any error) rolls the whole batch back and bubbles up so Run re-acquires the
// lease; the undecided work is simply re-selected next cycle — idempotent by
// construction (the Guard 2 conditional flips make reprocessing a no-op on already
// decided rows).
func (p *Processor) drain(ctx context.Context, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 1
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		work, err := p.fetchPending(ctx, batchSize)
		if err != nil {
			return err
		}
		if len(work) == 0 {
			return nil
		}
		if err := p.processBatch(ctx, work); err != nil {
			return err
		}
	}
}

// processBatch decides one batch of work in a single transaction (spec §8.5). It
// dispatches per spec §8.1: a single is decided by processSingleTx; a group leg
// triggers processGroupTx for the whole group at its first leg by id, and later
// legs of that group are skipped (both in-batch, via the decided set, and across
// batches, because a decided group's legs are no longer PENDING). Guards are
// evaluated per decision; any guard miss returns errGuardMiss, which db.WithTx
// turns into a whole-batch rollback.
func (p *Processor) processBatch(ctx context.Context, work []workRow) error {
	return db.WithTx(ctx, p.pool, func(tx pgx.Tx) error {
		decided := make(map[int64]bool)
		for _, w := range work {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if w.TransactionID == nil {
				op := pendingOp{ID: w.ID, AccountID: w.AccountID, Amount: w.Amount, EffectiveAt: w.EffectiveAt}
				if err := p.processSingleTx(ctx, tx, op); err != nil {
					return err
				}
				continue
			}
			txID := *w.TransactionID
			if decided[txID] {
				continue // a later leg of a group already decided in this batch
			}
			if err := p.processGroupTx(ctx, tx, txID); err != nil {
				return err
			}
			decided[txID] = true
		}
		return nil
	})
}

func (p *Processor) fetchPending(ctx context.Context, batchSize int) ([]workRow, error) {
	rows, err := p.pool.Query(ctx, fetchPendingWork, batchSize)
	if err != nil {
		return nil, fmt.Errorf("fetch pending work: %w", err)
	}
	defer rows.Close()

	var work []workRow
	for rows.Next() {
		var w workRow
		if err := rows.Scan(&w.ID, &w.AccountID, &w.Amount, &w.EffectiveAt, &w.TransactionID); err != nil {
			return nil, fmt.Errorf("scan pending work: %w", err)
		}
		work = append(work, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending work: %w", err)
	}
	return work, nil
}

func (p *Processor) readConfig(ctx context.Context) (model.Config, error) {
	var c model.Config
	err := p.pool.QueryRow(ctx, selectConfig).Scan(
		&c.LeaseTTLMs, &c.LoopIntervalMs, &c.BatchSize, &c.MaxGroupSize, &c.APIMaxWaitMs,
	)
	if err != nil {
		return model.Config{}, fmt.Errorf("read config row: %w", err)
	}
	return c, nil
}

// idleWaitLeader blocks the leader on the work doorbell until a notification
// arrives or the deadline elapses, whichever comes first. Both are normal wakeups
// (nil); only ctx cancellation returns non-nil. If the doorbell connection cannot
// be armed or breaks, it falls back to a plain timed sleep — the correctness path.
func (p *Processor) idleWaitLeader(ctx context.Context, deadline time.Duration) error {
	if err := p.ensureListener(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.log.Warn("arm doorbell listener; falling back to timed wait", "err", err)
		return sleepCtx(ctx, deadline)
	}

	if p.enteredIdleWait != nil {
		p.enteredIdleWait()
	}

	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	_, err := p.listenConn.Conn().WaitForNotification(waitCtx)
	if err == nil {
		return nil // doorbell rang
	}
	if ctx.Err() != nil {
		return ctx.Err() // shutdown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil // timed wakeup — the correctness path
	}
	// Connection problem: drop it so the next cycle re-arms; treat as a wakeup so
	// the loop re-selects immediately.
	p.log.Warn("doorbell wait failed; dropping listener", "err", err)
	p.dropListener()
	return nil
}

// ensureListener lazily acquires the dedicated doorbell connection and issues
// LISTEN work_available. It is a no-op once armed.
func (p *Processor) ensureListener(ctx context.Context) error {
	if p.listenConn != nil {
		return nil
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire doorbell connection: %w", err)
	}
	if _, err := conn.Exec(ctx, listenDoorbell); err != nil {
		conn.Release()
		return fmt.Errorf("listen work_available: %w", err)
	}
	p.listenConn = conn
	return nil
}

func (p *Processor) dropListener() {
	if p.listenConn != nil {
		p.listenConn.Release()
		p.listenConn = nil
	}
}

// releaseOnExit clears the lease on graceful shutdown so a standby takes over
// immediately without waiting for TTL expiry. It uses a fresh short-lived context
// because Run's ctx is already cancelled at this point.
func (p *Processor) releaseOnExit() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.lease.Release(ctx); err != nil {
		p.log.Warn("release lease on shutdown", "err", err)
	}
}

// idleDeadline is the leader's maximum idle sleep: the smaller of one loop
// interval and half the lease TTL. The TTL half is the "time to next safe lease
// renewal" — it guarantees the loop wakes to renew well before the lease expires
// even if loop_interval is configured longer than the TTL.
func idleDeadline(loopInterval, ttl time.Duration) time.Duration {
	safeRenewal := ttl / 2
	if safeRenewal > 0 && safeRenewal < loopInterval {
		return safeRenewal
	}
	return loopInterval
}

// clampInterval floors a non-positive configured interval to a small value so a
// misconfiguration cannot spin the loop.
func clampInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Millisecond
	}
	return d
}

// sleepCtx sleeps for d or until ctx is cancelled. It returns ctx.Err() on
// cancellation and nil when the timer fires.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
