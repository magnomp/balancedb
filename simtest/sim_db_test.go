//go:build simtest

// Package simtest DB harness (M6): it drives the real processor over a real
// throwaway schema and asserts the database against the sequential reference model.
// It is the seed the full spec §15 simulation (M10) grows from, scoped here to the
// M6 concerns — group processing (§8.3) and batching (§8.5). Run via `make simtest`
// against TEST_DATABASE_URL; each test self-isolates in a throwaway schema.
package simtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/processor"
)

const simOwner int64 = 1

// simAccount is one account the schedule targets, with its limits.
type simAccount struct {
	ext string
	min *int64
	max *int64
}

var simAccounts = []simAccount{
	{"tight", ptr(-60), ptr(60)},
	{"loose", ptr(-100000), ptr(100000)},
	{"unbounded", nil, nil},
	{"floor", ptr(-300), nil},
	{"ceiling", nil, ptr(300)},
}

// TestSimGroupsBatchedMatchReference runs randomized schedules of interleaved
// singles and groups through the real processor with large batches, then asserts
// the database matches the sequential reference model: G3 (every operation and
// transaction reaches the reference outcome), G1 (every confirmed balance within
// limits and equal to the reference), and G2 (no group partially applied). Every
// failure prints its seed.
func TestSimGroupsBatchedMatchReference(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		seed := int64(1_000 + iter)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			m := seedAccounts(t, pool)
			generateSchedule(t, pool, m, rand.New(rand.NewSource(seed)), 60)

			// A big batch exercises multi-decision transactions; a fast loop drains
			// quickly.
			setSimConfig(t, pool, 5, 5000, 50)
			runProcessorToDrain(t, pool, 20*time.Second)

			m.ProcessAll() // sequential oracle
			assertG2(t, pool, seed)
			assertMatchesReference(t, pool, m, seed)
		})
	}
}

// TestSimBatchCrashEqualsSequential proves batching is crash-safe: it drives the
// processor with small batches while repeatedly crashing it mid-flight (cancelling
// its context, which rolls back any open batch), restarts it, and after the churn
// finishes draining. The final database state must equal the sequential reference
// model — a batch crash is simply reprocessed, idempotent by construction (spec
// §8.5). G2 is checked at every restart boundary.
func TestSimBatchCrashEqualsSequential(t *testing.T) {
	for iter := 0; iter < 8; iter++ {
		seed := int64(9_000 + iter)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			m := seedAccounts(t, pool)
			rng := rand.New(rand.NewSource(seed))
			generateSchedule(t, pool, m, rng, 60)

			// Small batches → more batch boundaries → more chances to crash mid-batch.
			setSimConfig(t, pool, 3, 5000, 4)
			runWithCrashChurn(t, pool, rng, seed)
			// Finish cleanly (churn may leave a tail of PENDING work).
			runProcessorToDrain(t, pool, 20*time.Second)

			m.ProcessAll()
			assertG2(t, pool, seed)
			assertMatchesReference(t, pool, m, seed)
		})
	}
}

// seedAccounts creates every schedule account, with limits, in both the database
// and the reference model in the same order — so identity ids line up and the two
// can be compared row for row.
func seedAccounts(t *testing.T, pool *pgxpool.Pool) *Model {
	t.Helper()
	m := NewModel()
	ctx := context.Background()
	for _, a := range simAccounts {
		if _, err := pool.Exec(ctx,
			`INSERT INTO accounts (owner_id, external_id, min_balance, max_balance) VALUES ($1,$2,$3,$4)`,
			simOwner, a.ext, a.min, a.max); err != nil {
			t.Fatalf("create account %s: %v", a.ext, err)
		}
		if err := m.SetLimits(simOwner, a.ext, a.min, a.max); err != nil {
			t.Fatalf("reference SetLimits %s: %v", a.ext, err)
		}
	}
	return m
}

// generateSchedule inserts `steps` fresh units (singles and groups, 1..4 legs) into
// both the database and the reference model in lockstep, asserting the two return
// identical ids (so the reference stays a faithful mirror before any processing).
func generateSchedule(t *testing.T, pool *pgxpool.Pool, m *Model, rng *rand.Rand, steps int) {
	t.Helper()
	ctx := context.Background()
	when := time.Unix(0, 0).UTC()
	for step := 0; step < steps; step++ {
		nLegs := 1 + rng.Intn(4)
		ops := make([]api.InsertOp, nLegs)
		for i := range ops {
			amt := int64(rng.Intn(121) - 60)
			if amt == 0 {
				amt = 1
			}
			ops[i] = api.InsertOp{
				OwnerID:     simOwner,
				ExternalID:  simAccounts[rng.Intn(len(simAccounts))].ext,
				Amount:      amt,
				EffectiveAt: when.Add(time.Duration(rng.Intn(5)) * 24 * time.Hour),
			}
		}
		req := api.InsertRequest{IdempotencyKey: uuid(step + 1), Operations: ops}

		refRes, err := m.Insert(req)
		if err != nil {
			t.Fatalf("reference insert step %d: %v", step, err)
		}
		var dbRes *api.InsertResult
		err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
			r, e := api.Insert(ctx, tx, req)
			dbRes = r
			return e
		})
		if err != nil {
			t.Fatalf("db insert step %d: %v", step, err)
		}
		if !sameIDs(refRes, dbRes) {
			t.Fatalf("step %d: db and reference assigned different ids", step)
		}
	}
}

func setSimConfig(t *testing.T, pool *pgxpool.Pool, loopMs, ttlMs, batchSize int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE config SET loop_interval_ms=$1, lease_ttl_ms=$2, batch_size=$3`,
		loopMs, ttlMs, batchSize); err != nil {
		t.Fatalf("set config: %v", err)
	}
}

// runProcessorToDrain runs one processor until no PENDING operation remains, then
// stops it.
func runProcessorToDrain(t *testing.T, pool *pgxpool.Pool, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := startProcessor(t, pool, ctx)
	defer func() { cancel(); <-done }()
	waitDrained(t, pool, timeout)
}

// runWithCrashChurn repeatedly starts the processor, lets it run for a short random
// slice, then crashes it (cancel → any open batch rolls back). It checks G2 at each
// restart boundary. It stops early once the queue is drained.
func runWithCrashChurn(t *testing.T, pool *pgxpool.Pool, rng *rand.Rand, seed int64) {
	t.Helper()
	for i := 0; i < 40; i++ {
		if pendingCount(t, pool) == 0 {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := startProcessor(t, pool, ctx)
		time.Sleep(time.Duration(1+rng.Intn(12)) * time.Millisecond)
		cancel()
		<-done
		// After a crash, an open batch must have rolled back wholesale — no group is
		// ever partially applied.
		assertG2(t, pool, seed)
	}
}

// startProcessor builds a fresh processor (its own lease/owner) and runs it in a
// goroutine, returning a channel closed when Run returns.
func startProcessor(t *testing.T, pool *pgxpool.Pool, ctx context.Context) chan struct{} {
	t.Helper()
	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	p := processor.New(pool, l, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	return done
}

func waitDrained(t *testing.T, pool *pgxpool.Pool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pendingCount(t, pool) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("processor did not drain within %v (%d still PENDING)", timeout, pendingCount(t, pool))
}

func pendingCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM operations WHERE status = 'PENDING'`).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return n
}

// assertG2 checks the group-atomicity invariant directly against the database: no
// transaction has legs in more than one status, and every transaction's status
// agrees with its legs' status. This is the property invariant queries protect
// (spec §4.1 G2) — impossible to observe a partially applied group.
func assertG2(t *testing.T, pool *pgxpool.Pool, seed int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT transaction_id, count(DISTINCT status)
		   FROM operations WHERE transaction_id IS NOT NULL
		  GROUP BY transaction_id HAVING count(DISTINCT status) > 1`)
	if err != nil {
		t.Fatalf("seed %d: G2 query: %v", seed, err)
	}
	defer rows.Close()
	for rows.Next() {
		var txID int64
		var n int
		if err := rows.Scan(&txID, &n); err != nil {
			t.Fatalf("seed %d: G2 scan: %v", seed, err)
		}
		t.Fatalf("seed %d: G2 violated — transaction %d has legs in %d distinct statuses", seed, txID, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seed %d: G2 iterate: %v", seed, err)
	}

	// Transaction status must agree with its legs' single status.
	mismatch, err := pool.Query(ctx,
		`SELECT t.id, t.status, min(o.status)
		   FROM transactions t JOIN operations o ON o.transaction_id = t.id
		  GROUP BY t.id, t.status
		 HAVING (t.status = 'PENDING'   AND min(o.status) <> 'PENDING')
		     OR (t.status = 'COMMITTED' AND min(o.status) <> 'CONFIRMED')
		     OR (t.status = 'REJECTED'  AND min(o.status) <> 'INVALID')`)
	if err != nil {
		t.Fatalf("seed %d: G2 agreement query: %v", seed, err)
	}
	defer mismatch.Close()
	for mismatch.Next() {
		var id int64
		var ts, os string
		if err := mismatch.Scan(&id, &ts, &os); err != nil {
			t.Fatalf("seed %d: G2 agreement scan: %v", seed, err)
		}
		t.Fatalf("seed %d: G2 violated — transaction %d status %s disagrees with leg status %s", seed, id, ts, os)
	}
	if err := mismatch.Err(); err != nil {
		t.Fatalf("seed %d: G2 agreement iterate: %v", seed, err)
	}
}

// assertMatchesReference compares the drained database against the sequential
// reference model: every operation's status (G3), every transaction's status (G3),
// and every account's confirmed balance (G1). The reference is the oracle, so an
// exact match proves the batched, possibly-crashed run produced the same outcomes
// as a sequential unbatched run.
func assertMatchesReference(t *testing.T, pool *pgxpool.Pool, m *Model, seed int64) {
	t.Helper()
	ctx := context.Background()

	// Operations.
	rows, err := pool.Query(ctx, `SELECT id, status FROM operations ORDER BY id`)
	if err != nil {
		t.Fatalf("seed %d: read operations: %v", seed, err)
	}
	defer rows.Close()
	dbOps := 0
	for rows.Next() {
		var id int64
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatalf("seed %d: scan operation: %v", seed, err)
		}
		dbOps++
		ref, ok := m.ops[id]
		if !ok {
			t.Fatalf("seed %d: db operation %d absent from reference", seed, id)
		}
		if status != string(ref.Status) {
			t.Fatalf("seed %d: operation %d status db=%s reference=%s (G3)", seed, id, status, ref.Status)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seed %d: iterate operations: %v", seed, err)
	}
	if dbOps != len(m.ops) {
		t.Fatalf("seed %d: db has %d operations, reference has %d", seed, dbOps, len(m.ops))
	}

	// Transactions.
	txRows, err := pool.Query(ctx, `SELECT id, status FROM transactions ORDER BY id`)
	if err != nil {
		t.Fatalf("seed %d: read transactions: %v", seed, err)
	}
	defer txRows.Close()
	dbTxs := 0
	for txRows.Next() {
		var id int64
		var status string
		if err := txRows.Scan(&id, &status); err != nil {
			t.Fatalf("seed %d: scan transaction: %v", seed, err)
		}
		dbTxs++
		ref, ok := m.txs[id]
		if !ok {
			t.Fatalf("seed %d: db transaction %d absent from reference", seed, id)
		}
		if status != string(ref.Status) {
			t.Fatalf("seed %d: transaction %d status db=%s reference=%s (G3)", seed, id, status, ref.Status)
		}
	}
	if err := txRows.Err(); err != nil {
		t.Fatalf("seed %d: iterate transactions: %v", seed, err)
	}
	if dbTxs != len(m.txs) {
		t.Fatalf("seed %d: db has %d transactions, reference has %d", seed, dbTxs, len(m.txs))
	}

	// Balances (G1: equal to the reference, and — checked by the reference — within
	// limits).
	for _, a := range simAccounts {
		var bal int64
		if err := pool.QueryRow(ctx,
			`SELECT confirmed_balance FROM accounts WHERE owner_id=$1 AND external_id=$2`,
			simOwner, a.ext).Scan(&bal); err != nil {
			t.Fatalf("seed %d: read balance %s: %v", seed, a.ext, err)
		}
		if want := m.AccountBalance(simOwner, a.ext); bal != want {
			t.Fatalf("seed %d: account %s balance db=%d reference=%d (G1)", seed, a.ext, bal, want)
		}
	}
	if err := m.CheckG1(); err != nil {
		t.Fatalf("seed %d: reference %v", seed, err)
	}
}
