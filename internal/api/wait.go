package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/model"
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

// waitForOutcome implements the opt-in synchronous wait (spec §10.1, ADR-0002):
// register a waiter → one immediate status check (closes the lost-wakeup race where
// the decision committed before registration) → select over the outcome
// notification, a status-poll ticker (durability fallback), and the wait deadline.
// The decision → 200; expiry (capped by api_max_wait_ms) → 202 with current state.
func (s *Server) waitForOutcome(ctx context.Context, ownerID int64, res *InsertResult, waitMs int) (*CreateTransactionOutput, error) {
	// One key and one loader per unit: a group waits on 'tx:<id>' and reads the
	// transaction row; a single waits on 'op:<id>' and reads the operation row. Both
	// notification keys match exactly what the processor emits inside its commit.
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
		opID := res.Operations[0].ID
		key = fmt.Sprintf("op:%d", opID)
		load = func(c context.Context) (*CreateTransactionOutput, bool, error) {
			return s.loadSingleOutcome(c, opID, ownerID, res.Replayed)
		}
	}

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
		return nil, huma.Error500InternalServerError("read api_max_wait_ms", err)
	}

	// Immediate check once — the outcome may already be decided.
	out, terminal, err := load(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("read outcome", err)
	}
	if terminal {
		out.Status = http.StatusOK
		return out, nil
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
			return nil, ctx.Err()
		case <-deadline.C:
			// Expiry: return 202 with the latest state (still PENDING). "Wait up to
			// T", never a guaranteed outcome (spec §10.1).
			out, _, err := load(ctx)
			if err != nil {
				return nil, huma.Error500InternalServerError("read outcome", err)
			}
			out.Status = http.StatusAccepted
			return out, nil
		case <-signal:
			out, terminal, err := load(ctx)
			if err != nil {
				return nil, huma.Error500InternalServerError("read outcome", err)
			}
			if terminal {
				out.Status = http.StatusOK
				return out, nil
			}
		case <-ticker.C:
			out, terminal, err := load(ctx)
			if err != nil {
				return nil, huma.Error500InternalServerError("read outcome", err)
			}
			if terminal {
				out.Status = http.StatusOK
				return out, nil
			}
		}
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
// insert's replay flag through unchanged.
func (s *Server) loadSingleOutcome(ctx context.Context, opID, ownerID int64, replayed bool) (*CreateTransactionOutput, bool, error) {
	var (
		status string
		reason *string
	)
	err := s.pool.QueryRow(ctx, selectOpStatusForOwner, opID, ownerID).Scan(&status, &reason)
	if err != nil {
		return nil, false, err
	}
	out := &CreateTransactionOutput{}
	out.Body.Replayed = replayed
	out.Body.Operations = []OperationOutcome{{ID: opID, Status: status}}
	return out, status != string(model.OpPending), nil
}

// loadGroupOutcome reads a group's current status and its per-leg statuses
// (owner-scoped) into a response body and reports whether the group is terminal
// (COMMITTED or REJECTED).
func (s *Server) loadGroupOutcome(ctx context.Context, txID, ownerID int64, replayed bool) (*CreateTransactionOutput, bool, error) {
	var (
		status  string
		opCount int
	)
	err := s.pool.QueryRow(ctx, selectTransactionByID, txID).Scan(&status, &opCount, new(*string))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, err
	}

	rows, err := s.pool.Query(ctx, selectLegsForOwner, txID, ownerID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var legs []OperationOutcome
	for rows.Next() {
		var (
			leg    OperationOutcome
			reason *string
		)
		if err := rows.Scan(&leg.ID, &leg.Status, &reason); err != nil {
			return nil, false, err
		}
		legs = append(legs, leg)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	out := &CreateTransactionOutput{}
	out.Body.TransactionID = &txID
	out.Body.TransactionStatus = status
	out.Body.Replayed = replayed
	out.Body.Operations = legs
	return out, status != string(model.TxPending), nil
}
