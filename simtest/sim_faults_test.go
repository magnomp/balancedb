//go:build simtest

package simtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
)

// This file is the M10 fault-injection tier: competing-leader failover and
// zombie-leader stale-lease writes, both across the full scenario space. In each,
// several processors and/or direct lease tampering create windows where a node acts
// on a lease view that has moved under it; the three guards (spec §7.2) must catch
// every such stale attempt. The proof is correctness: after all the churn the
// database must still equal the sequential reference model exactly (Guard 3, the
// account-version CAS, is what a doubled or stale balance write would defeat — the
// mutation smoke test in the README disables it and this tier catches it), and G2 is
// checked at every failover boundary. Every failure prints its seed.

// proc bundles a running processor with the means to crash it.
type proc struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stop crashes one processor and waits for its goroutine to exit.
func (p *proc) stop() {
	p.cancel()
	<-p.done
}

// spawn starts one processor with its own lease/owner and returns its handle.
func spawn(t *testing.T, pool *pgxpool.Pool) *proc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := startProcessor(t, pool, ctx)
	return &proc{cancel: cancel, done: done}
}

// TestSimCompetingLeaders runs several processors at once against a short lease TTL
// so leadership genuinely contends and flaps, and repeatedly crashes and restarts
// individual nodes mid-flight (failover). Only one node may hold the lease at a time
// (Guard 1 fences the rest); after the churn the database must equal the sequential
// reference model. G2 is checked at every failover boundary.
func TestSimCompetingLeaders(t *testing.T) {
	seeds := envInt("SIMTEST_FAULT_SEEDS", 5)
	for i := 0; i < seeds; i++ {
		seed := int64(5_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			// Short TTL + tight loop → frequent leadership handoffs.
			setSimConfig(t, pool, 5, 150, 8)

			for r := 0; r < dbRounds; r++ {
				for _, a := range h.g.Round(h.m, dbMovesPerRound) {
					h.apply(t, a)
				}
				runCompetingChurn(t, pool, h.g, seed, 3)
				runProcessorToDrain(t, pool, 30*time.Second)
				h.m.ProcessAll()
				assertG2(t, pool, seed)
				h.assertMatches(t, seed)
				h.assertG4(t, seed)
			}
		})
	}
}

// runCompetingChurn starts n processors and repeatedly crashes and restarts a
// random one while work drains, checking G2 at each failover boundary. It stops
// early once the queue is empty; a clean drain follows at the call site.
func runCompetingChurn(t *testing.T, pool *pgxpool.Pool, g *Generator, seed int64, n int) {
	t.Helper()
	procs := make([]*proc, n)
	for i := range procs {
		procs[i] = spawn(t, pool)
	}
	defer func() {
		for _, p := range procs {
			p.stop()
		}
	}()

	for i := 0; i < 30; i++ {
		if pendingCount(t, pool) == 0 {
			return
		}
		time.Sleep(time.Duration(1+g.rng.Intn(8)) * time.Millisecond)
		j := g.rng.Intn(len(procs))
		procs[j].stop()
		assertG2(t, pool, seed) // failover boundary: no group left partially applied
		procs[j] = spawn(t, pool)
	}
}

// phantomOwner is a valid UUID that no real processor uses. The zombie churner
// installs it as the lease owner to simulate another node (or a GC-paused former
// leader) holding the lease while the real processors believe otherwise.
const phantomOwner = "ffffffff-ffff-4fff-8fff-ffffffffffff"

// TestSimZombieLeader runs real processors while a churner directly tampers with the
// lease row — expiring it and stealing it for a phantom owner with a very short
// horizon — to manufacture stale-lease-write windows. Guard 1 (the in-transaction
// lease fence) and Guard 3 (the account-version CAS) must reject every write a node
// attempts once its lease has moved; the proof is that the drained database still
// equals the sequential reference model exactly. G2 is checked throughout.
func TestSimZombieLeader(t *testing.T) {
	seeds := envInt("SIMTEST_FAULT_SEEDS", 5)
	for i := 0; i < seeds; i++ {
		seed := int64(6_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			// Short TTL so a stolen lease is reclaimable quickly; small batches so a
			// steal can land mid-decision.
			setSimConfig(t, pool, 5, 200, 4)

			for r := 0; r < dbRounds; r++ {
				for _, a := range h.g.Round(h.m, dbMovesPerRound) {
					h.apply(t, a)
				}
				runZombieChurn(t, pool, h.g, seed, 2)
				runProcessorToDrain(t, pool, 30*time.Second)
				h.m.ProcessAll()
				assertG2(t, pool, seed)
				h.assertMatches(t, seed)
				h.assertG4(t, seed)
			}
		})
	}
}

// runZombieChurn runs n real processors while, for a bounded number of ticks, it
// steals or expires the lease from under them. The steal always installs a very
// short horizon so a real processor reclaims the lease within a few loop intervals —
// otherwise the queue could never drain. After the churn the processors are left
// running to reclaim and finish (the caller then drains cleanly).
func runZombieChurn(t *testing.T, pool *pgxpool.Pool, g *Generator, seed int64, n int) {
	t.Helper()
	ctx := context.Background()
	procs := make([]*proc, n)
	for i := range procs {
		procs[i] = spawn(t, pool)
	}
	defer func() {
		for _, p := range procs {
			p.stop()
		}
	}()

	for i := 0; i < 50; i++ {
		if pendingCount(t, pool) == 0 {
			return
		}
		switch g.rng.Intn(3) {
		case 0:
			// Expire the lease outright — every node's next Guard 1 fence fails.
			if _, err := pool.Exec(ctx, `UPDATE leader_lease SET owner=NULL, lease_until=NULL`); err != nil {
				t.Fatalf("seed %d: expire lease: %v", seed, err)
			}
		case 1:
			// Steal the lease for a phantom owner with a short horizon: a real leader
			// mid-transaction becomes a zombie whose Guard 1/Guard 3 must catch it.
			if _, err := pool.Exec(ctx,
				`UPDATE leader_lease SET owner=$1::uuid, lease_until=now() + interval '40 milliseconds'`,
				phantomOwner); err != nil {
				t.Fatalf("seed %d: steal lease: %v", seed, err)
			}
		default:
			// A quiet tick — let the processors make progress.
		}
		time.Sleep(time.Duration(1+g.rng.Intn(4)) * time.Millisecond)
		assertG2(t, pool, seed)
	}
}

// TestSimVersionRaceGuard3 stresses the guard Guard 3 exists for: an API-style write
// racing the processor's decision on the same account (spec §7.2 — the version CAS
// "arbitrates API races" and closes the fence-to-commit window). A churner bumps
// account versions while the processor drains, so a version routinely moves between
// the processor's account read and its version CAS. Guard 3 must then match zero rows
// and roll the (whole batch) transaction back, leaving the operation PENDING for a
// clean retry; the drained database must still equal the sequential reference model
// exactly. This is the mutation the README's smoke test targets: disabling Guard 3's
// rowcount check lets such a missed CAS commit the status flip without applying the
// balance, and this test catches it (a confirmed operation whose amount never reached
// the balance). Every failure prints its seed.
func TestSimVersionRaceGuard3(t *testing.T) {
	// A targeted guard stressor (the churn forces many rollback-retries, so it is
	// slower per round than the plain schedules); a couple of seeds over a couple of
	// rounds reliably opens the read-to-CAS window. CI can scale it via SIMTEST_GUARD_SEEDS.
	seeds := envInt("SIMTEST_GUARD_SEEDS", 3)
	const guardRaceRounds = 2
	for i := 0; i < seeds; i++ {
		seed := int64(7_000_000 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			pool := dbtest.NewSchema(t)
			h := newHarness(t, pool, NewGenerator(seed))
			// batch_size 1 → one account read + CAS per transaction, the tightest
			// read-to-CAS window for the churner to slip a version bump into.
			setSimConfig(t, pool, 5, 5000, 1)

			for r := 0; r < guardRaceRounds; r++ {
				for _, a := range h.g.Round(h.m, dbMovesPerRound) {
					h.apply(t, a)
				}
				runWithVersionRace(t, pool, h, seed)
				// Finish cleanly once the churn stops (correct guards make every missed
				// CAS a rollback-and-retry, so all work eventually drains).
				runProcessorToDrain(t, pool, 30*time.Second)
				h.m.ProcessAll()
				assertG2(t, pool, seed)
				h.assertMatches(t, seed)
				h.assertG4(t, seed)
				h.assertSnapshots(t, seed)
			}
		})
	}
}

// runWithVersionRace runs one processor while, for a bounded number of ticks, it
// bumps every account's version out from under it (an out-of-band write, exactly
// what Guard 3 arbitrates). The bump changes only the version, never the balance, so
// with correct guards the final state is unaffected — a missed CAS simply rolls the
// batch back and it is retried. It stops early once the queue drains.
func runWithVersionRace(t *testing.T, pool *pgxpool.Pool, h *harness, seed int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := startProcessor(t, pool, ctx)
	defer func() { cancel(); <-done }()

	for i := 0; i < 40; i++ {
		if pendingCount(t, pool) == 0 {
			return
		}
		for _, a := range h.g.Accounts() {
			if _, err := pool.Exec(ctx,
				`UPDATE accounts SET version = version + 1 WHERE owner_id=$1 AND external_id=$2`,
				simOwnerID, a.ext); err != nil && ctx.Err() == nil {
				t.Fatalf("seed %d: version bump on %s: %v", seed, a.ext, err)
			}
		}
		time.Sleep(time.Millisecond)
		assertG2(t, pool, seed)
	}
}

// TestSimReversalConstraints is the targeted check for the reversal metadata rules
// the ledger enforces at insert (idx_ops_reversal, spec §5.4): at most one live
// (non-INVALID) reversal per operation, but a rejected reversal frees the original
// for a retry. It also confirms a reversal is decided as an ordinary operation
// against limits (N3) — a reversal that would breach a limit is INVALID, not
// special-cased.
func TestSimReversalConstraints(t *testing.T) {
	pool := dbtest.NewSchema(t)

	mkAccount(t, pool, "acct", ptr(0), ptr(1000))

	// Confirm X = +100 (balance 100) and Z = -80 (balance 20).
	xID := insertSingle(t, pool, "acct", 100, nil, 1)
	insertSingle(t, pool, "acct", -80, nil, 2)
	runProcessorToDrain(t, pool, 15*time.Second)
	requireStatus(t, pool, xID, "CONFIRMED")
	requireBalance(t, pool, "acct", 20)

	// One live reversal per op: reverse X while it has no reversal — this inserts.
	r1 := insertSingle(t, pool, "acct", -100, &xID, 3)
	// A second reversal of X while r1 is still live (PENDING) must be rejected by the
	// unique index — the insert transaction fails.
	if err := tryInsertSingle(t, pool, "acct", -100, &xID, 4); !isUniqueViolation(err) {
		t.Fatalf("second live reversal of op %d: want unique violation, got %v", xID, err)
	}

	// Decide r1: reversing to 20-100 = -80 breaches min 0, so it is INVALID (N3), not
	// special-cased. Balance unchanged.
	runProcessorToDrain(t, pool, 15*time.Second)
	requireStatus(t, pool, r1, "INVALID")
	requireBalance(t, pool, "acct", 20)

	// Reversal-after-reject: r1 is INVALID, so the original X is eligible again — a
	// fresh reversal of X now inserts cleanly. Lift the balance first (W = +100,
	// registered before the retry) so this reversal, decided in registration order,
	// can succeed.
	insertSingle(t, pool, "acct", 100, nil, 5)
	r2 := insertSingle(t, pool, "acct", -100, &xID, 6)
	runProcessorToDrain(t, pool, 15*time.Second)
	requireStatus(t, pool, r2, "CONFIRMED")
	requireBalance(t, pool, "acct", 20) // 120 - 100

	// With a live (CONFIRMED) reversal on X, a further reversal of X is blocked again.
	if err := tryInsertSingle(t, pool, "acct", -100, &xID, 7); !isUniqueViolation(err) {
		t.Fatalf("reversal of op %d with a live confirmed reversal: want unique violation, got %v", xID, err)
	}
}

// --- targeted-test helpers ---

func mkAccount(t *testing.T, pool *pgxpool.Pool, ext string, min, max *int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (owner_id, external_id, min_balance, max_balance) VALUES ($1,$2,$3,$4)`,
		simOwnerID, ext, min, max); err != nil {
		t.Fatalf("create account %s: %v", ext, err)
	}
}

// insertSingle inserts one single via the real inserter and returns its id, failing
// the test on any error.
func insertSingle(t *testing.T, pool *pgxpool.Pool, ext string, amount int64, reversalOf *int64, key int) int64 {
	t.Helper()
	id, err := doInsertSingle(pool, ext, amount, reversalOf, key)
	if err != nil {
		t.Fatalf("insert single (amount %d) on %s: %v", amount, ext, err)
	}
	return id
}

// tryInsertSingle attempts an insert and returns the error (nil on success) without
// failing the test — used to assert the reversal unique index rejects a duplicate.
func tryInsertSingle(t *testing.T, pool *pgxpool.Pool, ext string, amount int64, reversalOf *int64, key int) error {
	t.Helper()
	_, err := doInsertSingle(pool, ext, amount, reversalOf, key)
	return err
}

func doInsertSingle(pool *pgxpool.Pool, ext string, amount int64, reversalOf *int64, key int) (int64, error) {
	ctx := context.Background()
	var id int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		res, e := api.Insert(ctx, tx, api.InsertRequest{
			IdempotencyKey: uuid(key),
			Operations: []api.InsertOp{{
				OwnerID:     simOwnerID,
				ExternalID:  ext,
				Amount:      amount,
				EffectiveAt: time.Unix(0, 0).UTC(),
				ReversalOf:  reversalOf,
			}},
		})
		if e != nil {
			return e
		}
		id = res.Operations[0].ID
		return nil
	})
	return id, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func requireStatus(t *testing.T, pool *pgxpool.Pool, opID int64, want string) {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM operations WHERE id=$1`, opID).Scan(&got); err != nil {
		t.Fatalf("read status of op %d: %v", opID, err)
	}
	if got != want {
		t.Fatalf("op %d status = %s, want %s", opID, got, want)
	}
}

func requireBalance(t *testing.T, pool *pgxpool.Pool, ext string, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(context.Background(),
		`SELECT confirmed_balance FROM accounts WHERE owner_id=$1 AND external_id=$2`, simOwnerID, ext).Scan(&got); err != nil {
		t.Fatalf("read balance of %s: %v", ext, err)
	}
	if got != want {
		t.Fatalf("account %s balance = %d, want %d", ext, got, want)
	}
}
