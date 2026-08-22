package simtest

import (
	"fmt"

	"github.com/magnomp/balancedb/internal/model"
)

// This file extends the sequential reference model with the processor's single-op
// decision semantics (spec §6 validation, §8.2 processing) and confirmed-balance
// evolution. It mirrors internal/processor.processSingle in memory so later
// milestones (and M5's own property tests) can assert the database against it and
// check G1 (final balance within limits) after every decision.

// Decision is the outcome the reference model computes for one operation.
type Decision struct {
	OpID   int64
	Status model.OpStatus
	Reason *model.Rejection // set only when Status is INVALID
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

// ProcessNext decides the lowest-id PENDING single operation in registration order
// (spec §8.1: strict id order) and returns its decision. It reports ok=false when
// no PENDING single remains. Group legs (transaction_id set) are left PENDING —
// group processing is M6. Ids are dense (identity columns), so scanning 1..nextOpID
// visits operations exactly in registration order.
func (m *Model) ProcessNext() (Decision, bool) {
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || op.Status != model.OpPending || op.TransactionID != nil {
			continue
		}
		return m.decide(op), true
	}
	return Decision{}, false
}

// ProcessAll decides every PENDING single in registration order and returns the
// decisions in that order.
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
