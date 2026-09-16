package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
)

// defaultPollInterval is the status-poll cadence of the synchronous wait path — the
// durability fallback that resolves a wait even when the outcome NOTIFY is lost (the
// listen connection was down during the decision). Spec §10.1 names 1–2 s; 1 s here.
const defaultPollInterval = time.Second

// SQL kept as consts beside their single call site (plan §0), unqualified.
const (
	selectAPIMaxWait = `SELECT api_max_wait_ms FROM config`

	// Owner-scoped current status of a single operation — used by the wait path to
	// re-read the decision on each poll/notification. Joined through accounts so it
	// never reads across owners (§2).
	selectOpStatusForOwner = `SELECT o.status, o.invalidation_reason
FROM operations o JOIN accounts a ON a.id = o.account_id
WHERE o.id = $1 AND a.owner_id = $2`
)

// waitForOutcome implements the opt-in synchronous wait for POST /transactions
// (spec §10.1, ADR-0002): one key and one loader per unit — a group waits on
// 'tx:<id>' and reads the transaction row; a single waits on 'op:<id>' and reads
// the operation row. Both notification keys match exactly what the processor emits
// inside its commit. The decision → 200; expiry → 202 with the current state.
func (s *Server) waitForOutcome(ctx context.Context, ownerID int64, res *InsertResult, waitMs int) (*CreateTransactionOutput, error) {
	var (
		key  string
		load func(context.Context) (*CreateTransactionOutput, bool, error)
	)
	if res.TransactionID != nil {
		txID := *res.TransactionID
		key = fmt.Sprintf("tx:%d", txID)
		load = func(c context.Context) (*CreateTransactionOutput, bool, error) {
			return s.loadGroupOutcome(c, txID, ownerID, res.Replayed)
		}
	} else {
		single := res.Operations[0]
		key = fmt.Sprintf("op:%d", single.ID)
		load = func(c context.Context) (*CreateTransactionOutput, bool, error) {
			return s.loadSingleOutcome(c, single, ownerID, res.Replayed)
		}
	}
	out, status, err := awaitDecision(ctx, s, key, waitMs, load)
	if err != nil {
		return nil, err
	}
	out.Status = status
	return out, nil
}

// awaitDecision is the synchronous-wait loop shared by every waiting endpoint
// (POST /transactions and PATCH /operations/{id}, ADR-0010): register a waiter for
// key → one immediate status check (closes the lost-wakeup race where the decision
// committed before registration) → select over the outcome notification, a
// status-poll ticker (durability fallback), and the wait deadline. load reads the
// current state into the endpoint's response body and reports whether it is
// terminal. Returns the body plus the HTTP status the caller must set: 200 when
// decided in time, 202 (capped by api_max_wait_ms) on expiry with the current,
// still-pending state. A returned error is already a Huma error.
func awaitDecision[T any](ctx context.Context, s *Server, key string, waitMs int, load func(context.Context) (*T, bool, error)) (*T, int, error) {
	// Time the whole wait (register to return); the source label separates the
	// ADR-0002 NOTIFY fast path from the durability poll fallback (§13 wait health).
	waitStart := time.Now()

	// Register BEFORE the first status check so a notification that fires between the
	// insert commit and now is captured (the buffered channel holds it) rather than
	// lost. A nil notifier (poll-only deployment / spec export) leaves signal nil,
	// which simply never selects — the poll ticker carries the wait.
	var signal <-chan struct{}
	if s.notifier != nil {
		ch, cancel := s.notifier.Register(key)
		defer cancel()
		signal = ch
	}

	budget, err := s.waitBudget(ctx, waitMs)
	if err != nil {
		return nil, 0, huma.Error500InternalServerError("read api_max_wait_ms", err)
	}

	// Immediate check once — the outcome may already be decided.
	out, terminal, err := load(ctx)
	if err != nil {
		return nil, 0, huma.Error500InternalServerError("read outcome", err)
	}
	if terminal {
		s.observeWait(waitStart, obs.WaitSourceImmediate, obs.WaitResultDecided)
		return out, http.StatusOK, nil
	}

	poll := s.pollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	deadline := time.NewTimer(budget)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-deadline.C:
			// Expiry: return 202 with the latest state (still PENDING). "Wait up to
			// T", never a guaranteed outcome (spec §10.1).
			out, _, err := load(ctx)
			if err != nil {
				return nil, 0, huma.Error500InternalServerError("read outcome", err)
			}
			s.observeWait(waitStart, obs.WaitSourceTimeout, obs.WaitResultPending)
			return out, http.StatusAccepted, nil
		case <-signal:
			out, terminal, err := load(ctx)
			if err != nil {
				return nil, 0, huma.Error500InternalServerError("read outcome", err)
			}
			if terminal {
				s.observeWait(waitStart, obs.WaitSourceNotify, obs.WaitResultDecided)
				return out, http.StatusOK, nil
			}
		case <-ticker.C:
			out, terminal, err := load(ctx)
			if err != nil {
				return nil, 0, huma.Error500InternalServerError("read outcome", err)
			}
			if terminal {
				s.observeWait(waitStart, obs.WaitSourcePoll, obs.WaitResultDecided)
				return out, http.StatusOK, nil
			}
		}
	}
}

// observeWait records one synchronous-wait latency into the wait-health metric,
// if an observer is wired (§13, ADR-0002). No-op when unmeasured.
func (s *Server) observeWait(start time.Time, source, result string) {
	if s.waitObs != nil {
		s.waitObs.ObserveWait(time.Since(start).Seconds(), source, result)
	}
}

// waitBudget caps the client's requested wait_ms by api_max_wait_ms from the config
// row (hot-reloaded, read per request) and returns it as a duration.
func (s *Server) waitBudget(ctx context.Context, waitMs int) (time.Duration, error) {
	var maxWaitMs int
	if err := s.pool.QueryRow(ctx, selectAPIMaxWait).Scan(&maxWaitMs); err != nil {
		return 0, err
	}
	if maxWaitMs > 0 && waitMs > maxWaitMs {
		waitMs = maxWaitMs
	}
	return time.Duration(waitMs) * time.Millisecond, nil
}

// loadSingleOutcome reads a single operation's current status (owner-scoped) into a
// response body and reports whether it is terminal (decided). Replayed carries the
// insert's replay flag through unchanged, as does the item's edit_of (immutable).
func (s *Server) loadSingleOutcome(ctx context.Context, single OpOutcome, ownerID int64, replayed bool) (*CreateTransactionOutput, bool, error) {
	var (
		status string
		reason *string
	)
	err := s.pool.QueryRow(ctx, selectOpStatusForOwner, single.ID, ownerID).Scan(&status, &reason)
	if err != nil {
		return nil, false, err
	}
	out := &CreateTransactionOutput{}
	out.Body.Replayed = replayed
	out.Body.Operations = []OperationOutcome{{ID: single.ID, Status: status, EditOf: single.EditOf}}
	return out, status != string(model.OpPending), nil
}

// loadGroupOutcome reads a group's current status and its per-leg statuses
// (owner-scoped) into a response body and reports whether the group is terminal
// (COMMITTED or REJECTED). Legs carry edit_of so an edit item's outcome names its
// target exactly as the insert response did.
func (s *Server) loadGroupOutcome(ctx context.Context, txID, ownerID int64, replayed bool) (*CreateTransactionOutput, bool, error) {
	value, err := db.Read(ctx, s.pool, func(tx pgx.Tx) (*ledger.TransactionOutcome, error) {
		return ledger.GetTransaction(ctx, tx, ownerID, txID)
	})
	if err != nil {
		return nil, false, err
	}
	out := &CreateTransactionOutput{}
	out.Body.TransactionID = &value.ID
	out.Body.TransactionStatus = string(value.Status)
	out.Body.Replayed = replayed
	out.Body.Operations = make([]OperationOutcome, len(value.Operations))
	for i, op := range value.Operations {
		out.Body.Operations[i] = OperationOutcome{ID: op.ID, Status: string(op.Status), EditOf: op.EditOf}
	}
	return out, value.Status != model.TxPending, nil
}
