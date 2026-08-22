package simtest

import (
	"fmt"
	"sort"

	"github.com/magnomp/balancedb/internal/model"
)

// This file extends the sequential reference model with the processor's single-op
// decision semantics (spec §6 validation, §8.2 processing) and confirmed-balance
// evolution. It mirrors internal/processor.processSingle in memory so later
// milestones (and M5's own property tests) can assert the database against it and
// check G1 (final balance within limits) after every decision.

// Decision is the outcome the reference model computes for one processing step —
// either a single operation or a whole group, decided at its first leg by
// registration order.
//
// For a single: OpID is the operation, TxID is nil, Status is CONFIRMED|INVALID.
// For a group: TxID is the transaction, OpID is 0, LegIDs lists the legs in
// registration order, Status is the terminal status shared by every leg
// (CONFIRMED|INVALID), and TxStatus is the transaction status (COMMITTED|REJECTED).
type Decision struct {
	OpID     int64
	TxID     *int64
	Status   model.OpStatus
	TxStatus model.TxStatus
	Reason   *model.Rejection // set only when the decision rejects
	LegIDs   []int64
}

// SetLimits sets an account's limits, creating the account on demand. It enforces
// the spec §6 rule for limit changes — a new min is accepted only if
// min <= confirmed_balance, a new max only if confirmed_balance <= max — returning
// an error otherwise, so a schedule cannot install limits the confirmed balance
// already violates. nil means unbounded.
func (m *Model) SetLimits(ownerID int64, externalID string, minBalance, maxBalance *int64) error {
	id := m.upsertAccount(ownerID, externalID)
	a := m.accountsByID[id]
	if minBalance != nil && *minBalance > a.Balance {
		return fmt.Errorf("simtest: new min %d exceeds confirmed balance %d", *minBalance, a.Balance)
	}
	if maxBalance != nil && *maxBalance < a.Balance {
		return fmt.Errorf("simtest: new max %d below confirmed balance %d", *maxBalance, a.Balance)
	}
	a.Min, a.Max = minBalance, maxBalance
	return nil
}

// AccountBalance returns an account's confirmed (final) balance, or 0 if it does
// not exist.
func (m *Model) AccountBalance(ownerID int64, externalID string) int64 {
	id, ok := m.accounts[accountKey{ownerID: ownerID, externalID: externalID}]
	if !ok {
		return 0
	}
	return m.accountsByID[id].Balance
}

// ProcessNext decides the lowest-id PENDING operation in registration order (spec
// §8.1: strict id order) and returns its decision. A single is decided on its own;
// a group leg triggers the whole group's decision at that first leg, and the
// group's remaining legs are then no longer PENDING (skipped by status). It reports
// ok=false when no PENDING operation remains. Ids are dense (identity columns), so
// scanning 1..nextOpID visits operations exactly in registration order.
func (m *Model) ProcessNext() (Decision, bool) {
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || op.Status != model.OpPending {
			continue
		}
		if op.TransactionID == nil {
			return m.decide(op), true
		}
		return m.decideGroup(m.txs[*op.TransactionID]), true
	}
	return Decision{}, false
}

// ProcessAll decides every PENDING operation in registration order and returns the
// decisions in that order — the sequence G3 compares the database against.
func (m *Model) ProcessAll() []Decision {
	var out []Decision
	for {
		d, ok := m.ProcessNext()
		if !ok {
			return out
		}
		out = append(out, d)
	}
}

// decide applies the binary validation (spec §6) to one operation, flips its
// status (once — facts are never mutated afterwards), and evolves the account's
// confirmed balance on accept.
func (m *Model) decide(op *refOp) Decision {
	a := m.accountsByID[op.AccountID]
	newBal := a.Balance + op.Amount

	switch {
	case a.Max != nil && newBal > *a.Max:
		op.Status = model.OpInvalid
		return Decision{OpID: op.ID, Status: model.OpInvalid, Reason: &model.Rejection{
			Code:      model.ReasonLimitViolated,
			LimitSide: model.LimitMax,
			Shortfall: newBal - *a.Max,
		}}
	case a.Min != nil && newBal < *a.Min:
		op.Status = model.OpInvalid
		return Decision{OpID: op.ID, Status: model.OpInvalid, Reason: &model.Rejection{
			Code:      model.ReasonLimitViolated,
			LimitSide: model.LimitMin,
			Shortfall: *a.Min - newBal,
		}}
	default:
		a.Balance = newBal
		op.Status = model.OpConfirmed
		return Decision{OpID: op.ID, Status: model.OpConfirmed}
	}
}

// decideGroup applies spec §8.3 to a whole group: it nets the legs per account,
// validates every involved account in ascending id order, and commits all-or-
// nothing. All pass → every account's net is applied, every leg flips to CONFIRMED
// and the transaction to COMMITTED. Any fail → the whole group flips to INVALID /
// REJECTED with the first offending account (ascending id, exactly as the
// processor picks it) and its shortfall; no balance changes. Netting per account is
// sound because the whole group applies atomically (spec §6/G2). Facts are flipped
// once and never mutated afterwards.
func (m *Model) decideGroup(tx *refTx) Decision {
	net := make(map[int64]int64, len(tx.LegIDs))
	ids := make([]int64, 0, len(tx.LegIDs))
	for _, legID := range tx.LegIDs {
		op := m.ops[legID]
		if _, seen := net[op.AccountID]; !seen {
			ids = append(ids, op.AccountID)
		}
		net[op.AccountID] += op.Amount
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	txID := tx.ID
	legs := append([]int64(nil), tx.LegIDs...)

	// Validate every involved account; the first violation rejects the group.
	for _, acctID := range ids {
		a := m.accountsByID[acctID]
		newBal := a.Balance + net[acctID]
		var (
			side      model.LimitSide
			shortfall int64
		)
		switch {
		case a.Max != nil && newBal > *a.Max:
			side, shortfall = model.LimitMax, newBal-*a.Max
		case a.Min != nil && newBal < *a.Min:
			side, shortfall = model.LimitMin, *a.Min-newBal
		default:
			continue
		}
		for _, legID := range legs {
			m.ops[legID].Status = model.OpInvalid
		}
		tx.Status = model.TxRejected
		return Decision{
			TxID:     &txID,
			Status:   model.OpInvalid,
			TxStatus: model.TxRejected,
			Reason: &model.Rejection{
				Code:      model.ReasonLimitViolated,
				Account:   a.ExternalID,
				LimitSide: side,
				Shortfall: shortfall,
			},
			LegIDs: legs,
		}
	}

	// All accounts pass: apply each net and confirm the group.
	for _, acctID := range ids {
		m.accountsByID[acctID].Balance += net[acctID]
	}
	for _, legID := range legs {
		m.ops[legID].Status = model.OpConfirmed
	}
	tx.Status = model.TxCommitted
	return Decision{
		TxID:     &txID,
		Status:   model.OpConfirmed,
		TxStatus: model.TxCommitted,
		LegIDs:   legs,
	}
}

// CheckG2 verifies the group-atomicity guarantee (spec §4.1): no group is ever
// partially applied — every transaction's legs share a single status, and that
// status agrees with the transaction's own status. It returns an error naming the
// first violation, or nil.
func (m *Model) CheckG2() error {
	for txID, tx := range m.txs {
		var legStatus model.OpStatus
		for i, legID := range tx.LegIDs {
			s := m.ops[legID].Status
			if i == 0 {
				legStatus = s
				continue
			}
			if s != legStatus {
				return fmt.Errorf("G2 violated: transaction %d has mixed leg statuses (%s and %s)", txID, legStatus, s)
			}
		}
		if !legStatusAgrees(legStatus, tx.Status) {
			return fmt.Errorf("G2 violated: transaction %d status %s disagrees with leg status %s", txID, tx.Status, legStatus)
		}
	}
	return nil
}

// legStatusAgrees maps a transaction status to the leg status it implies.
func legStatusAgrees(legStatus model.OpStatus, txStatus model.TxStatus) bool {
	switch txStatus {
	case model.TxPending:
		return legStatus == model.OpPending
	case model.TxCommitted:
		return legStatus == model.OpConfirmed
	case model.TxRejected:
		return legStatus == model.OpInvalid
	default:
		return false
	}
}

// CheckG1 verifies the consistency contract's first guarantee (spec §4.1): every
// account's confirmed balance is within its configured limits. It returns an error
// naming the first violation, or nil.
func (m *Model) CheckG1() error {
	for id, a := range m.accountsByID {
		if a.Min != nil && a.Balance < *a.Min {
			return fmt.Errorf("G1 violated: account %d balance %d below min %d", id, a.Balance, *a.Min)
		}
		if a.Max != nil && a.Balance > *a.Max {
			return fmt.Errorf("G1 violated: account %d balance %d above max %d", id, a.Balance, *a.Max)
		}
	}
	return nil
}
