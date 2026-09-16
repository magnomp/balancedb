//go:build simtest

// Package simtest DB harness (M10): the fidelity tier of the spec §15 simulation.
// It drives the real processor over a real throwaway schema and asserts the
// database against the sequential reference model across the full scenario space
// (the shared seeded generator in generate.go), interleaved with fault injection —
// batch-boundary crashes (this file), competing-leader failover and zombie-leader
// stale-lease writes (sim_faults_test.go) — with edits (single, grouped, mixed;
// ADR-0010) in every schedule. The reference model is the oracle; an exact match
// after churn — statuses, current columns, revisions and the append-only
// operation_revisions rows included — proves the three guards (spec §7.2) plus
// the revision CAS hold the consistency contract G1–G6. Run via `make simtest`
// against TEST_DATABASE_URL; each test self-isolates in a throwaway schema.
// Every failure prints its seed.
//
// Identity ids do NOT line up between the reference and the database: idempotent
// replays and payload conflicts run `INSERT ... ON CONFLICT DO NOTHING`, which
// consumes (skips) a Postgres IDENTITY value while the reference, checking the key
// first, assigns none. So the harness maintains an explicit reference→database id
// map (built from each fresh insert's returned ids, in registration order) and
// compares through it — the approach the M6 handoff flagged as the alternative to id
// alignment. The map also translates a reversal's reversal_of and an edit's edit_of
// (reference op ids) to the real operations they must reference in the database.
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
	"github.com/magnomp/balancedb/internal/model"
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
	mix         actionMix        // optional tally of the applied actions
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

// TestSimFullScenarioMatchReference (SIM-002) is the primary fidelity check:
// randomized schedules across the full scenario space (limits, singles, groups,
// aggressive back/future-dating with day crossings and timestamp ties, reversals
// including double-reversals and reversal-after-reject retries, edits — single,
// grouped and mixed — limit changes, idempotent replays/conflicts) run through
// the real batched processor, then the database is asserted equal to the
// sequential reference model: G1 (every confirmed balance within limits and
// equal to the reference), G2 (no group partially applied), G3 (every operation
// and transaction reaches the reference outcome; every operation's current
// columns, revision and append-only history match), G4 (timeline order stable
// and identical to the reference, current effective_at included), plus the
// snapshot cascade invariant (the latest daily snapshot equals the confirmed
// balance) and G6 (no duplicate rows). It prints the action mix. Every failure
// prints its seed.
func TestSimFullScenarioMatchReference(t *testing.T) {
	seeds := envInt("SIMTEST_DB_SEEDS", 12)
	mix := actionMix{}
	for i := 0; i < seeds; i++ {
		seed := int64(4_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			h.mix = mix
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
	reportMix(t, seeds, mix)
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
	mix := actionMix{}
	defer func() { reportMix(t, seeds, mix) }()
	for i := 0; i < seeds; i++ {
		seed := int64(9_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			h.mix = mix
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
	if h.mix != nil {
		h.mix.add(a)
	}

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

// translate returns a copy of req with each op's reversal_of, edit_of and
// delete_of remapped from the reference op id the generator chose to the real
// database op id it must reference.
func (h *harness) translate(req api.InsertRequest) api.InsertRequest {
	ops := make([]api.InsertOp, len(req.Operations))
	copy(ops, req.Operations)
	for i := range ops {
		if ops[i].ReversalOf != nil {
			dbID := h.refToDBOp[*ops[i].ReversalOf]
			ops[i].ReversalOf = &dbID
		}
		if ops[i].EditOf != nil {
			dbID := h.refToDBOp[*ops[i].EditOf]
			ops[i].EditOf = &dbID
		}
		if ops[i].DeleteOf != nil {
			dbID := h.refToDBOp[*ops[i].DeleteOf]
			ops[i].DeleteOf = &dbID
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

// assertG2 checks the group-atomicity invariant directly against the database:
// every leg of every transaction holds exactly the status its transaction's
// status implies — PENDING ↔ PENDING; COMMITTED ↔ CONFIRMED for a regular leg,
// APPLIED for an edit or delete leg (ADR-0010/0011); REJECTED ↔ INVALID. A
// regular leg of a COMMITTED group may since have been DELETED by a later delete
// (group membership is not a deletion unit, ADR-0011): a DELETED leg must then
// carry its deletion stamps, and a DELETED leg is never allowed under any other
// transaction status. This is the property invariant queries protect (spec §4.1
// G2) — impossible to observe a partially applied group, including one whose
// edit legs applied without its regular legs confirming or vice versa.
func assertG2(t *testing.T, pool *pgxpool.Pool, seed int64) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT t.id, t.status, o.id, o.status, o.edit_of IS NOT NULL
		   FROM transactions t JOIN operations o ON o.transaction_id = t.id
		  WHERE o.status <> CASE t.status
		                      WHEN 'PENDING'   THEN 'PENDING'
		                      WHEN 'COMMITTED' THEN CASE WHEN o.edit_of IS NULL THEN 'CONFIRMED' ELSE 'APPLIED' END
		                      WHEN 'REJECTED'  THEN 'INVALID'
		                    END
		    AND NOT (t.status = 'COMMITTED' AND o.edit_of IS NULL AND o.status = 'DELETED'
		             AND o.deleted_by IS NOT NULL AND o.deleted_at IS NOT NULL)
		  ORDER BY t.id, o.id`)
	if err != nil {
		t.Fatalf("seed %d: G2 query: %v", seed, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			txID, opID int64
			ts, os     string
			isEdit     bool
		)
		if err := rows.Scan(&txID, &ts, &opID, &os, &isEdit); err != nil {
			t.Fatalf("seed %d: G2 scan: %v", seed, err)
		}
		t.Fatalf("seed %d: G2 violated — transaction %d (%s) has leg %d (edit=%v) in status %s", seed, txID, ts, opID, isEdit, os)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seed %d: G2 iterate: %v", seed, err)
	}
}

// dbOp is one operations row as the fidelity comparison reads it: status plus
// the current columns an applied edit overwrites (ADR-0010), the edit-class
// linkage and kind, and the deletion stamps (ADR-0011). EffectiveAt is compared
// with Equal, so it lives outside the comparable core.
type dbOp struct {
	status    string
	accountID int64
	amount    int64
	effective time.Time
	revision  int32
	editOf    *int64
	isDelete  bool
	deletedBy *int64
	deletedAt *time.Time
}

// dbRevision is one operation_revisions row as the fidelity comparison reads it.
type dbRevision struct {
	revision     int32
	accountID    int64
	amount       int64
	effective    time.Time
	supersededBy int64
}

// assertMatches compares the drained database against the sequential reference
// model, through the id map: every operation's status (DELETED included),
// current columns (account, amount, effective_at), revision, edit-class linkage
// and kind (edit_of, is_delete) and deletion stamps (deleted_by mapped through
// refToDBOp, deleted_at present exactly on DELETED rows) (G3, ADR-0010/0011),
// every operation's append-only history in operation_revisions (dense,
// superseded by the mapped APPLIED edit), every transaction's status (G3), row
// counts (G6), and every account's confirmed balance (G1). The reference is the
// oracle, so an exact match proves the batched, possibly-crashed,
// possibly-contended run produced the same outcomes as a sequential unbatched
// run — including that no applied edit or delete was lost or doubled and no
// rejected edit or delete touched its target.
func (h *harness) assertMatches(t *testing.T, seed int64) {
	t.Helper()
	ctx := context.Background()

	// Operations: read every real row, compare each reference op through the map.
	dbOps, err := h.scanOps(ctx)
	if err != nil {
		t.Fatalf("seed %d: read operations: %v", seed, err)
	}
	if len(dbOps) != len(h.m.ops) {
		t.Fatalf("seed %d: db has %d operations, reference has %d (G6)", seed, len(dbOps), len(h.m.ops))
	}
	dbRevs, err := h.scanRevisions(ctx)
	if err != nil {
		t.Fatalf("seed %d: read operation_revisions: %v", seed, err)
	}
	nRevs := 0
	for refID, op := range h.m.ops {
		dbID, ok := h.refToDBOp[refID]
		if !ok {
			t.Fatalf("seed %d: reference op %d has no database mapping", seed, refID)
		}
		got, ok := dbOps[dbID]
		if !ok {
			t.Fatalf("seed %d: reference op %d maps to db op %d, which does not exist", seed, refID, dbID)
		}
		if got.status != string(op.Status) {
			t.Fatalf("seed %d: operation ref=%d db=%d status db=%s reference=%s (G3)", seed, refID, dbID, got.status, op.Status)
		}
		wantAcct := h.dbAcctByExt[h.m.accountsByID[op.AccountID].ExternalID]
		if got.accountID != wantAcct || got.amount != op.Amount || !got.effective.Equal(op.EffectiveAt) || got.revision != op.Revision {
			t.Fatalf("seed %d: operation ref=%d db=%d current state db={acct %d, %d, %v, rev %d} reference={acct %d, %d, %v, rev %d}",
				seed, refID, dbID, got.accountID, got.amount, got.effective, got.revision, wantAcct, op.Amount, op.EffectiveAt, op.Revision)
		}
		switch {
		case op.EditOf == nil && got.editOf != nil:
			t.Fatalf("seed %d: operation ref=%d db=%d is an edit in the db (of %d) but not in the reference", seed, refID, dbID, *got.editOf)
		case op.EditOf != nil && (got.editOf == nil || *got.editOf != h.refToDBOp[*op.EditOf]):
			t.Fatalf("seed %d: edit ref=%d db=%d edit_of db=%v reference(mapped)=%d", seed, refID, dbID, got.editOf, h.refToDBOp[*op.EditOf])
		case got.isDelete != op.isDelete():
			t.Fatalf("seed %d: operation ref=%d db=%d is_delete db=%v reference=%v", seed, refID, dbID, got.isDelete, op.isDelete())
		}
		// Deletion stamps (ADR-0011): deleted_by is the mapped APPLIED delete on a
		// DELETED row and absent otherwise; deleted_at travels with it.
		switch {
		case op.DeletedBy == nil && (got.deletedBy != nil || got.deletedAt != nil):
			t.Fatalf("seed %d: operation ref=%d db=%d carries deletion stamps (by %v at %v) but the reference has none", seed, refID, dbID, got.deletedBy, got.deletedAt)
		case op.DeletedBy != nil && (got.deletedBy == nil || *got.deletedBy != h.refToDBOp[*op.DeletedBy] || got.deletedAt == nil):
			t.Fatalf("seed %d: operation ref=%d db=%d deleted_by db=%v deleted_at db=%v reference(mapped)=%d", seed, refID, dbID, got.deletedBy, got.deletedAt, h.refToDBOp[*op.DeletedBy])
		case (got.status == string(model.OpDeleted)) != (got.deletedBy != nil):
			t.Fatalf("seed %d: operation ref=%d db=%d status %s with deleted_by %v", seed, refID, dbID, got.status, got.deletedBy)
		}

		// History: dense 1..revision−1, each row's state and superseding edit equal.
		want := h.m.revisions[refID]
		gotRevs := dbRevs[dbID]
		nRevs += len(gotRevs)
		if len(gotRevs) != len(want) {
			t.Fatalf("seed %d: operation ref=%d db=%d has %d history rows, reference %d", seed, refID, dbID, len(gotRevs), len(want))
		}
		for i, r := range want {
			g := gotRevs[i]
			wantAcct := h.dbAcctByExt[h.m.accountsByID[r.AccountID].ExternalID]
			if g.revision != r.Revision || g.accountID != wantAcct || g.amount != r.Amount || !g.effective.Equal(r.EffectiveAt) || g.supersededBy != h.refToDBOp[r.SupersededBy] {
				t.Fatalf("seed %d: operation ref=%d db=%d revision %d db=%+v reference={rev %d, acct %d, %d, %v, by %d}",
					seed, refID, dbID, r.Revision, g, r.Revision, wantAcct, r.Amount, r.EffectiveAt, h.refToDBOp[r.SupersededBy])
			}
		}
	}
	total := 0
	for _, rs := range dbRevs {
		total += len(rs)
	}
	if total != nRevs {
		t.Fatalf("seed %d: db has %d operation_revisions rows, %d belong to mapped operations", seed, total, nRevs)
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

// assertG4 checks the deterministic ordering guarantee (spec §4.1): for every
// account the database's timeline — operations ordered by (effective_at, id) — is
// identical to the reference model's (mapped through the id table). Identity ids
// are monotonic in insertion order, so the map preserves relative order and the
// tie break on equal effective_at agrees; statuses change during processing and
// an applied edit may move an operation's effective_at or account (ADR-0010), but
// both sides apply the same edits, so the current timelines must still agree.
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

// scanOps reads every operations row into a map by id.
func (h *harness) scanOps(ctx context.Context) (map[int64]dbOp, error) {
	rows, err := h.pool.Query(ctx, `SELECT id, status, account_id, amount, effective_at, revision, edit_of, is_delete, deleted_by, deleted_at FROM operations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]dbOp)
	for rows.Next() {
		var (
			id int64
			o  dbOp
		)
		if err := rows.Scan(&id, &o.status, &o.accountID, &o.amount, &o.effective, &o.revision, &o.editOf, &o.isDelete, &o.deletedBy, &o.deletedAt); err != nil {
			return nil, err
		}
		out[id] = o
	}
	return out, rows.Err()
}

// scanRevisions reads every operation_revisions row, grouped by operation and
// ordered by revision.
func (h *harness) scanRevisions(ctx context.Context) (map[int64][]dbRevision, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT operation_id, revision, account_id, amount, effective_at, superseded_by
		   FROM operation_revisions ORDER BY operation_id, revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]dbRevision)
	for rows.Next() {
		var (
			opID int64
			r    dbRevision
		)
		if err := rows.Scan(&opID, &r.revision, &r.accountID, &r.amount, &r.effective, &r.supersededBy); err != nil {
			return nil, err
		}
		out[opID] = append(out[opID], r)
	}
	return out, rows.Err()
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
