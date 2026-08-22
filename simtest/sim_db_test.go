//go:build simtest

// Package simtest DB harness (M10): the fidelity tier of the spec §15 simulation.
// It drives the real processor over a real throwaway schema and asserts the
// database against the sequential reference model across the full scenario space
// (the shared seeded generator in generate.go), interleaved with fault injection —
// batch-boundary crashes (this file), competing-leader failover and zombie-leader
// stale-lease writes (sim_faults_test.go). The reference model is the oracle; an
// exact match after churn proves the three guards (spec §7.2) hold the consistency
// contract G1–G6. Run via `make simtest` against TEST_DATABASE_URL; each test
// self-isolates in a throwaway schema. Every failure prints its seed.
//
// Identity ids do NOT line up between the reference and the database: idempotent
// replays and payload conflicts run `INSERT ... ON CONFLICT DO NOTHING`, which
// consumes (skips) a Postgres IDENTITY value while the reference, checking the key
// first, assigns none. So the harness maintains an explicit reference→database id
// map (built from each fresh insert's returned ids, in registration order) and
// compares through it — the approach the M6 handoff flagged as the alternative to id
// alignment. The map also translates a reversal's reversal_of (a reference op id) to
// the real operation it must reference in the database.
package simtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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

// dbRounds and dbMovesPerRound shape one DB-backed schedule: generate a round of
// moves into the database and the reference model in lockstep, drain it through the
// real processor, then assert the two agree. Draining between rounds is what lets
// reversals target confirmed operations and limit changes bracket a settled
// balance.
const (
	dbRounds        = 5
	dbMovesPerRound = 20

	// crashRounds is fewer than dbRounds because each crash round pays for many
	// processor restarts; it still spans several drains so reversals and limit changes
	// act on settled state.
	crashRounds = 3
)

// harness runs one DB-backed schedule: the reference model, the database pool, and
// the reference→database id maps that let the two be compared despite id skew.
type harness struct {
	pool        *pgxpool.Pool
	g           *Generator
	m           *Model
	refToDBOp   map[int64]int64  // reference op id → database op id
	refToDBTx   map[int64]int64  // reference transaction id → database transaction id
	dbAcctByExt map[string]int64 // external id → database account id
}

// newHarness creates every schedule account (with limits) in both the database and
// the reference model, caches each account's database id, and returns a ready
// harness.
func newHarness(t *testing.T, pool *pgxpool.Pool, g *Generator) *harness {
	t.Helper()
	h := &harness{
		pool:        pool,
		g:           g,
		m:           NewModel(),
		refToDBOp:   make(map[int64]int64),
		refToDBTx:   make(map[int64]int64),
		dbAcctByExt: make(map[string]int64),
	}
	ctx := context.Background()
	for _, a := range g.Accounts() {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO accounts (owner_id, external_id, min_balance, max_balance) VALUES ($1,$2,$3,$4) RETURNING id`,
			g.owner, a.ext, a.min, a.max).Scan(&id); err != nil {
			t.Fatalf("create account %s: %v", a.ext, err)
		}
		h.dbAcctByExt[a.ext] = id
		if err := h.m.SetLimits(g.owner, a.ext, a.min, a.max); err != nil {
			t.Fatalf("reference SetLimits %s: %v", a.ext, err)
		}
	}
	return h
}

// TestSimFullScenarioMatchReference is the primary fidelity check: randomized
// schedules across the full scenario space (limits, singles, groups, aggressive
// back/future-dating with day crossings and timestamp ties, reversals including
// double-reversals and reversal-after-reject retries, limit changes, idempotent
// replays/conflicts) run through the real batched processor, then the database is
// asserted equal to the sequential reference model: G1 (every confirmed balance
// within limits and equal to the reference), G2 (no group partially applied), G3
// (every operation and transaction reaches the reference outcome), G4 (timeline
// order stable and identical to the reference), plus the snapshot cascade invariant
// (the latest daily snapshot equals the confirmed balance) and G6 (no duplicate
// rows). Every failure prints its seed.
func TestSimFullScenarioMatchReference(t *testing.T) {
	seeds := envInt("SIMTEST_DB_SEEDS", 12)
	for i := 0; i < seeds; i++ {
		seed := int64(4_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			setSimConfig(t, pool, 5, 5000, 50)

			for r := 0; r < dbRounds; r++ {
				// Apply all of the round's moves (inserts and out-of-band limit changes)
				// before any decision: a limit change is not a registered operation, so
				// its effect must be settled before the round's operations are decided —
				// exactly as the reference applies it before ProcessAll. Then one
				// processor drains the whole round in batches.
				for _, a := range h.g.Round(h.m, dbMovesPerRound) {
					h.apply(t, a)
				}
				runProcessorToDrain(t, pool, 20*time.Second)
				h.m.ProcessAll() // sequential oracle
				assertG2(t, pool, seed)
				h.assertMatches(t, seed)
				h.assertG4(t, seed)
				h.assertSnapshots(t, seed)
			}
		})
	}
}

// TestSimBatchCrashEqualsSequential proves batching is crash-safe under the full
// scenario space: it drives the processor with small batches while repeatedly
// crashing it mid-flight (cancelling its context, which rolls back any open batch),
// restarts it, and after the churn finishes draining. The final database state must
// equal the sequential reference model — a batch-boundary crash is simply
// reprocessed, idempotent by construction (spec §8.5). G2 is checked at every
// restart boundary. Every failure prints its seed.
func TestSimBatchCrashEqualsSequential(t *testing.T) {
	seeds := envInt("SIMTEST_CRASH_SEEDS", 4)
	for i := 0; i < seeds; i++ {
		seed := int64(9_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			// Small batches → more batch boundaries → more chances to crash mid-batch.
			setSimConfig(t, pool, 3, 5000, 4)

			for r := 0; r < crashRounds; r++ {
				for _, a := range h.g.Round(h.m, dbMovesPerRound) {
					h.apply(t, a)
				}
				runWithCrashChurn(t, pool, h.g, seed)
				// Finish cleanly (churn may leave a tail of PENDING work).
				runProcessorToDrain(t, pool, 20*time.Second)
				h.m.ProcessAll()
				assertG2(t, pool, seed)
				h.assertMatches(t, seed)
				h.assertG4(t, seed)
			}
		})
	}
}

// apply replays one generated move against the database and the reference model in
// lockstep, asserting they agree, and records the reference→database id map for
// every fresh leg. Fresh inserts must return legs in the same registration order on
// both sides; replays are flagged and add no rows; payload conflicts are refused on
// both sides; satisfiable limit changes apply on both. This is where G6 is enforced
// on the DB side (a replay must not duplicate; a conflict must not write).
func (h *harness) apply(t *testing.T, a Action) {
	t.Helper()
	ctx := context.Background()

	if a.Kind == actSetLimits {
		if err := h.m.SetLimits(simOwnerID, a.Limits.ExternalID, a.Limits.Min, a.Limits.Max); err != nil {
			t.Fatalf("reference limit change on %s: %v", a.Limits.ExternalID, err)
		}
		// Mirror the real PUT /limits: replace both bounds and bump the version.
		ct, err := h.pool.Exec(ctx,
			`UPDATE accounts SET min_balance=$1, max_balance=$2, version=version+1 WHERE owner_id=$3 AND external_id=$4`,
			a.Limits.Min, a.Limits.Max, simOwnerID, a.Limits.ExternalID)
		if err != nil {
			t.Fatalf("db limit change on %s: %v", a.Limits.ExternalID, err)
		}
		if ct.RowsAffected() != 1 {
			t.Fatalf("db limit change on %s affected %d rows", a.Limits.ExternalID, ct.RowsAffected())
		}
		return
	}

	// Insert-family action. The reference request carries reference op ids in
	// reversal_of; the database needs the mapped real op id, so translate a copy.
	refRes, refErr := h.m.Insert(a.Req)
	dbReq := h.translate(a.Req)
	var dbRes *api.InsertResult
	dbErr := db.WithTx(ctx, h.pool, func(tx pgx.Tx) error {
		r, e := api.Insert(ctx, tx, dbReq)
		dbRes = r
		return e
	})

	if a.ExpectConflict {
		if refErr == nil || dbErr == nil {
			t.Fatalf("expected payload conflict, ref=%v db=%v", refErr, dbErr)
		}
		return
	}
	if refErr != nil || dbErr != nil {
		t.Fatalf("insert errored ref=%v db=%v", refErr, dbErr)
	}
	if len(refRes.Operations) != len(dbRes.Operations) {
		t.Fatalf("leg count differs ref=%d db=%d", len(refRes.Operations), len(dbRes.Operations))
	}

	if a.ExpectReplay {
		if !refRes.Replayed || !dbRes.Replayed {
			t.Fatalf("expected replay, ref.Replayed=%v db.Replayed=%v", refRes.Replayed, dbRes.Replayed)
		}
		// A replay must return the same ids as the original insert (G6): verify the
		// established mapping still holds.
		for i := range refRes.Operations {
			if got := h.refToDBOp[refRes.Operations[i].ID]; got != dbRes.Operations[i].ID {
				t.Fatalf("replay op id remap changed: ref %d → db %d, was %d",
					refRes.Operations[i].ID, dbRes.Operations[i].ID, got)
			}
		}
		return
	}

	// Fresh insert: record the reference→database id map, leg by leg.
	for i := range refRes.Operations {
		h.refToDBOp[refRes.Operations[i].ID] = dbRes.Operations[i].ID
	}
	if refRes.TransactionID != nil && dbRes.TransactionID != nil {
		h.refToDBTx[*refRes.TransactionID] = *dbRes.TransactionID
	}
}

// translate returns a copy of req with each op's reversal_of remapped from the
// reference op id the generator chose to the real database op id it must reference.
func (h *harness) translate(req api.InsertRequest) api.InsertRequest {
	ops := make([]api.InsertOp, len(req.Operations))
	copy(ops, req.Operations)
	for i := range ops {
		if ops[i].ReversalOf != nil {
			dbID := h.refToDBOp[*ops[i].ReversalOf]
			ops[i].ReversalOf = &dbID
		}
	}
	req.Operations = ops
	return req
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
func runWithCrashChurn(t *testing.T, pool *pgxpool.Pool, g *Generator, seed int64) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if pendingCount(t, pool) == 0 {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := startProcessor(t, pool, ctx)
		time.Sleep(time.Duration(1+g.rng.Intn(12)) * time.Millisecond)
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

// assertMatches compares the drained database against the sequential reference
// model, through the id map: every operation's status (G3), every transaction's
// status (G3), row counts (G6), and every account's confirmed balance (G1). The
// reference is the oracle, so an exact match proves the batched, possibly-crashed,
// possibly-contended run produced the same outcomes as a sequential unbatched run.
func (h *harness) assertMatches(t *testing.T, seed int64) {
	t.Helper()
	ctx := context.Background()

	// Operations: read all real statuses, compare each reference op through the map.
	dbStatus, err := scanStatuses(ctx, h.pool, `SELECT id, status FROM operations`)
	if err != nil {
		t.Fatalf("seed %d: read operations: %v", seed, err)
	}
	if len(dbStatus) != len(h.m.ops) {
		t.Fatalf("seed %d: db has %d operations, reference has %d (G6)", seed, len(dbStatus), len(h.m.ops))
	}
	for refID, op := range h.m.ops {
		dbID, ok := h.refToDBOp[refID]
		if !ok {
			t.Fatalf("seed %d: reference op %d has no database mapping", seed, refID)
		}
		if got := dbStatus[dbID]; got != string(op.Status) {
			t.Fatalf("seed %d: operation ref=%d db=%d status db=%s reference=%s (G3)", seed, refID, dbID, got, op.Status)
		}
	}

	// Transactions.
	dbTxStatus, err := scanStatuses(ctx, h.pool, `SELECT id, status FROM transactions`)
	if err != nil {
		t.Fatalf("seed %d: read transactions: %v", seed, err)
	}
	if len(dbTxStatus) != len(h.m.txs) {
		t.Fatalf("seed %d: db has %d transactions, reference has %d", seed, len(dbTxStatus), len(h.m.txs))
	}
	for refID, tx := range h.m.txs {
		dbID, ok := h.refToDBTx[refID]
		if !ok {
			t.Fatalf("seed %d: reference transaction %d has no database mapping", seed, refID)
		}
		if got := dbTxStatus[dbID]; got != string(tx.Status) {
			t.Fatalf("seed %d: transaction ref=%d db=%d status db=%s reference=%s (G3)", seed, refID, dbID, got, tx.Status)
		}
	}

	// Balances (G1: equal to the reference — compared by the stable external id — and,
	// checked by the reference, within limits).
	for _, acctID := range h.m.AccountIDs() {
		ext := h.m.accountsByID[acctID].ExternalID
		var bal int64
		if err := h.pool.QueryRow(ctx,
			`SELECT confirmed_balance FROM accounts WHERE owner_id=$1 AND external_id=$2`,
			simOwnerID, ext).Scan(&bal); err != nil {
			t.Fatalf("seed %d: read balance %s: %v", seed, ext, err)
		}
		if want := h.m.accountsByID[acctID].Balance; bal != want {
			t.Fatalf("seed %d: account %s balance db=%d reference=%d (G1)", seed, ext, bal, want)
		}
	}
	if err := h.m.CheckG1(); err != nil {
		t.Fatalf("seed %d: reference %v", seed, err)
	}
}

// assertG4 checks the deterministic, immutable ordering guarantee (spec §4.1): for
// every account the database's timeline — operations ordered by (effective_at, id) —
// is identical to the reference model's (mapped through the id table). Identity ids
// are monotonic in insertion order, so the map preserves relative order and the tie
// break on equal effective_at agrees; statuses change during processing, but
// effective_at and id never do, so the order is stable.
func (h *harness) assertG4(t *testing.T, seed int64) {
	t.Helper()
	ctx := context.Background()
	for _, acctID := range h.m.AccountIDs() {
		ext := h.m.accountsByID[acctID].ExternalID
		dbAcct := h.dbAcctByExt[ext]
		rows, err := h.pool.Query(ctx,
			`SELECT id FROM operations WHERE account_id=$1 ORDER BY effective_at, id`, dbAcct)
		if err != nil {
			t.Fatalf("seed %d: G4 query %s: %v", seed, ext, err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatalf("seed %d: G4 scan %s: %v", seed, ext, err)
			}
			got = append(got, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("seed %d: G4 iterate %s: %v", seed, ext, err)
		}
		want := h.m.Timeline(acctID)
		if len(got) != len(want) {
			t.Fatalf("seed %d: G4 %s timeline length db=%d reference=%d", seed, ext, len(got), len(want))
		}
		for i := range got {
			if got[i] != h.refToDBOp[want[i]] {
				t.Fatalf("seed %d: G4 %s timeline differs at %d: db=%d reference(mapped)=%d",
					seed, ext, i, got[i], h.refToDBOp[want[i]])
			}
		}
	}
}

// assertSnapshots checks the daily-snapshot cascade (spec §8.4): the balance of an
// account's latest snapshot day equals its confirmed balance. Since every confirmed
// operation lands on or before the latest day, the cumulative snapshot there is the
// full final balance — so a cascade that dropped or double-counted a backdated
// operation would diverge here.
func (h *harness) assertSnapshots(t *testing.T, seed int64) {
	t.Helper()
	ctx := context.Background()
	for _, acctID := range h.m.AccountIDs() {
		ext := h.m.accountsByID[acctID].ExternalID
		dbAcct := h.dbAcctByExt[ext]
		var bal int64
		err := h.pool.QueryRow(ctx,
			`SELECT balance FROM balance_snapshots WHERE account_id=$1 ORDER BY day DESC LIMIT 1`, dbAcct).Scan(&bal)
		if err == pgx.ErrNoRows {
			continue // no confirmed operation on this account yet
		}
		if err != nil {
			t.Fatalf("seed %d: read latest snapshot %s: %v", seed, ext, err)
		}
		if want := h.m.accountsByID[acctID].Balance; bal != want {
			t.Fatalf("seed %d: account %s latest snapshot balance %d != confirmed balance %d", seed, ext, bal, want)
		}
	}
}

// scanStatuses reads an (id, status) query into a map.
func scanStatuses(ctx context.Context, pool *pgxpool.Pool, sql string) (map[int64]string, error) {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var id int64
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}
