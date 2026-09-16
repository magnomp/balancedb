package simtest

import (
	"fmt"
	"sort"

	"github.com/magnomp/balancedb/internal/model"
)

// This file extends the sequential reference model with the processor's decision
// semantics — singles (spec §6 validation, §8.2 processing), groups (§8.3),
// edits, single or grouped (ADR-0010), and deletes (ADR-0011) — and
// confirmed-balance evolution. It mirrors internal/processor in memory so the
// DB-backed tiers can assert the database against it and check G1 (final balance
// within limits) after every decision.

// Decision is the outcome the reference model computes for one processing step —
// either a single operation or a whole group, decided at its first leg by
// registration order.
//
// For a single: OpID is the operation, TxID is nil, Status is CONFIRMED|INVALID
// (APPLIED|INVALID for an edit or delete registration). For a group: TxID is the
// transaction, OpID is 0, LegIDs lists the legs in registration order, Status is
// the terminal status of its regular legs (CONFIRMED|INVALID; edit legs of a
// committed group read APPLIED), and TxStatus is the transaction status
// (COMMITTED|REJECTED).
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

// ProcessNext decides the lowest-id visible PENDING operation (spec §8.1,
// ADR-0008) and returns its decision. A single is decided on its own;
// a group leg triggers the whole group's decision at that first leg, and the
// group's remaining legs are then no longer PENDING (skipped by status). A unit
// with an edit or delete whose target is still PENDING — a single, or a group
// with such a leg — is deferred as a whole: skipped, left PENDING, exactly as
// the processor skips it and moves on (ADR-0010/0011). It reports ok=false when no
// decidable PENDING operation remains. Ids are dense (identity columns), so
// scanning 1..nextOpID visits operations exactly in registration order.
func (m *Model) ProcessNext() (Decision, bool) {
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || m.uncommitted[id] || op.Status != model.OpPending {
			continue
		}
		if op.TransactionID == nil {
			if m.targetPending(op) {
				continue // deferred: the target has not been decided yet
			}
			return m.decide(op), true
		}
		tx := m.txs[*op.TransactionID]
		deferred := false
		for _, legID := range tx.LegIDs {
			if m.targetPending(m.ops[legID]) {
				deferred = true
				break
			}
		}
		if deferred {
			continue // the whole group waits for the target
		}
		return m.decideGroup(tx), true
	}
	return Decision{}, false
}

// targetPending reports whether op is an edit or delete whose target is still
// PENDING.
func (m *Model) targetPending(op *refOp) bool {
	return op.EditOf != nil && m.ops[*op.EditOf].Status == model.OpPending
}

// ProcessAll decides currently visible PENDING operations in ID order. Later
// commits can expose lower IDs after higher ones were decided (ADR-0008).
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
// confirmed balance on accept. An edit registration is decided by decideEdit, a
// delete registration by decideDelete.
func (m *Model) decide(op *refOp) Decision {
	if op.isDelete() {
		return m.decideDelete(op)
	}
	if op.EditOf != nil {
		return m.decideEdit(op)
	}
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

// decideEdit mirrors internal/processor.processSingleEditTx (ADR-0010): the
// target must be CONFIRMED (INVALID → TARGET_NOT_EDITABLE; a PENDING target is
// deferred by ProcessNext and never reaches here) and match expected_revision
// (STALE_REVISION); then the edit is two virtual legs — −current on the current
// account, +proposed on the proposed account — netted per account and validated
// in ascending account id, the first violation rejecting with LIMIT_VIOLATED. On
// accept every net is applied, the superseded state is appended to the target's
// history, the target's current columns are overwritten at revision + 1, and the
// edit flips to APPLIED. On reject nothing but the edit's own status changes.
func (m *Model) decideEdit(op *refOp) Decision {
	if rej := m.checkTarget(op); rej != nil {
		op.Status = model.OpInvalid
		return Decision{OpID: op.ID, Status: model.OpInvalid, Reason: rej}
	}

	ids, net := m.netVirtualLegs([]*refOp{op})
	if rej := m.firstViolation(ids, net); rej != nil {
		op.Status = model.OpInvalid
		return Decision{OpID: op.ID, Status: model.OpInvalid, Reason: rej}
	}

	for _, acctID := range ids {
		m.accountsByID[acctID].Balance += net[acctID]
	}
	m.applyEdit(op)
	return Decision{OpID: op.ID, Status: model.OpApplied}
}

// decideDelete mirrors internal/processor.processSingleDeleteTx (ADR-0011): the
// target must be CONFIRMED (INVALID or DELETED → TARGET_NOT_EDITABLE; a PENDING
// target is deferred by ProcessNext and never reaches here) and match
// expected_revision (STALE_REVISION); then the delete is one virtual leg —
// −current on the target's current account — validated against that account's
// limits, a violation rejecting with LIMIT_VIOLATED. On accept the net is
// applied, the target flips CONFIRMED → DELETED stamped with this delete, and the
// delete flips to APPLIED; no revision is appended and the target's revision and
// last values stay. On reject nothing but the delete's own status changes.
func (m *Model) decideDelete(op *refOp) Decision {
	if rej := m.checkTarget(op); rej != nil {
		op.Status = model.OpInvalid
		return Decision{OpID: op.ID, Status: model.OpInvalid, Reason: rej}
	}

	ids, net := m.netVirtualLegs([]*refOp{op})
	if rej := m.firstViolation(ids, net); rej != nil {
		op.Status = model.OpInvalid
		return Decision{OpID: op.ID, Status: model.OpInvalid, Reason: rej}
	}

	for _, acctID := range ids {
		m.accountsByID[acctID].Balance += net[acctID]
	}
	m.applyDelete(op)
	return Decision{OpID: op.ID, Status: model.OpApplied}
}

// applyDelete records an accepted delete: the target flips CONFIRMED → DELETED
// with DeletedBy = the delete, its last values and Revision untouched, nothing
// appended to its history; the delete flips to APPLIED (the balance was already
// moved by the caller).
func (m *Model) applyDelete(op *refOp) {
	target := m.ops[*op.EditOf]
	deletedBy := op.ID
	target.Status = model.OpDeleted
	target.DeletedBy = &deletedBy
	op.Status = model.OpApplied
}

// checkTarget mirrors internal/processor.checkTarget for one edit or delete
// whose target is not PENDING: an INVALID or DELETED target rejects with
// TARGET_NOT_EDITABLE, a mismatched expected_revision with STALE_REVISION; nil
// means decidable on its limits.
func (m *Model) checkTarget(op *refOp) *model.Rejection {
	target := m.ops[*op.EditOf]
	targetID := target.ID
	if target.Status != model.OpConfirmed {
		return &model.Rejection{Code: model.ReasonTargetNotEditable, OperationID: &targetID}
	}
	if op.ExpectedRevision != nil && *op.ExpectedRevision != target.Revision {
		want, got := *op.ExpectedRevision, target.Revision
		return &model.Rejection{
			Code: model.ReasonStaleRevision, OperationID: &targetID, ExpectedRevision: &want, ActualRevision: &got,
		}
	}
	return nil
}

// netVirtualLegs nets a unit's virtual legs per account — one leg per regular
// item, two per edit item (−current on the target's current account, +proposed
// on the proposed one), one per delete item (−current only; its own row's copy
// is never read) — and returns the involved account ids ascending, the
// processor's deterministic validation order (internal/processor.netLegs).
func (m *Model) netVirtualLegs(items []*refOp) (ids []int64, net map[int64]int64) {
	net = make(map[int64]int64, len(items))
	add := func(acctID, amount int64) {
		if _, seen := net[acctID]; !seen {
			ids = append(ids, acctID)
		}
		net[acctID] += amount
	}
	for _, op := range items {
		if op.EditOf != nil {
			target := m.ops[*op.EditOf]
			add(target.AccountID, -target.Amount)
			if op.isDelete() {
				continue
			}
		}
		add(op.AccountID, op.Amount)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, net
}

// firstViolation validates every involved account's net in ascending id order
// (spec §6) and returns the first LIMIT_VIOLATED rejection, or nil.
func (m *Model) firstViolation(ids []int64, net map[int64]int64) *model.Rejection {
	for _, acctID := range ids {
		a := m.accountsByID[acctID]
		newBal := a.Balance + net[acctID]
		switch {
		case a.Max != nil && newBal > *a.Max:
			return &model.Rejection{Code: model.ReasonLimitViolated, Account: a.ExternalID, LimitSide: model.LimitMax, Shortfall: newBal - *a.Max}
		case a.Min != nil && newBal < *a.Min:
			return &model.Rejection{Code: model.ReasonLimitViolated, Account: a.ExternalID, LimitSide: model.LimitMin, Shortfall: *a.Min - newBal}
		}
	}
	return nil
}

// applyEdit records an accepted edit: the target's superseded state is appended
// to its history, its current columns are overwritten at revision + 1, and the
// edit flips to APPLIED (the balances were already moved by the caller).
func (m *Model) applyEdit(op *refOp) {
	target := m.ops[*op.EditOf]
	m.revisions[target.ID] = append(m.revisions[target.ID], refRevision{
		Revision: target.Revision, AccountID: target.AccountID, Amount: target.Amount,
		EffectiveAt: target.EffectiveAt, SupersededBy: op.ID,
	})
	target.AccountID, target.Amount, target.EffectiveAt = op.AccountID, op.Amount, op.EffectiveAt
	target.Revision++
	op.Status = model.OpApplied
}

// decideGroup applies spec §8.3 to a whole group, edit and delete legs included
// (ADR-0010/0011): every edit-class leg's target must be CONFIRMED and at the
// expected revision — the first failing leg, in leg order, rejects the whole
// group with TARGET_NOT_EDITABLE / STALE_REVISION (a PENDING target defers the
// group in ProcessNext and never reaches here); then the legs' virtual legs are
// netted per account and every involved account is validated in ascending id
// order, all-or-nothing. All pass → every account's net is applied, every edit
// leg applies to its target (history appended, current columns overwritten,
// APPLIED), every delete leg flips its target DELETED (APPLIED), every regular
// leg flips to CONFIRMED and the transaction to COMMITTED. Any fail → the
// whole group flips to INVALID / REJECTED with the one shared reason (the first
// offending account, ascending id, exactly as the processor picks it) and no
// balance or target changes. Netting per account is sound because the whole
// group applies atomically (spec §6/G2). Facts are flipped once and never
// mutated afterwards. The decision's Status is that of the regular legs
// (CONFIRMED on commit; edit legs read APPLIED).
func (m *Model) decideGroup(tx *refTx) Decision {
	txID := tx.ID
	legs := append([]int64(nil), tx.LegIDs...)
	items := make([]*refOp, len(legs))
	for i, legID := range legs {
		items[i] = m.ops[legID]
	}

	reject := func(rej *model.Rejection) Decision {
		for _, legID := range legs {
			m.ops[legID].Status = model.OpInvalid
		}
		tx.Status = model.TxRejected
		return Decision{TxID: &txID, Status: model.OpInvalid, TxStatus: model.TxRejected, Reason: rej, LegIDs: legs}
	}

	// Target-state rules first, in leg order.
	for _, op := range items {
		if op.EditOf == nil {
			continue
		}
		if rej := m.checkTarget(op); rej != nil {
			return reject(rej)
		}
	}

	// Validate every involved account; the first violation rejects the group.
	ids, net := m.netVirtualLegs(items)
	if rej := m.firstViolation(ids, net); rej != nil {
		return reject(rej)
	}

	// All accounts pass: apply each net, apply each edit, confirm the group.
	for _, acctID := range ids {
		m.accountsByID[acctID].Balance += net[acctID]
	}
	for _, op := range items {
		switch {
		case op.isDelete():
			m.applyDelete(op)
		case op.EditOf != nil:
			m.applyEdit(op)
		default:
			op.Status = model.OpConfirmed
		}
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
// partially applied — every leg of a transaction holds exactly the status its
// transaction's status implies (PENDING ↔ PENDING; COMMITTED ↔ CONFIRMED for a
// regular leg, APPLIED for an edit or delete leg; REJECTED ↔ INVALID). A regular
// leg of a COMMITTED group may since have been DELETED by a later delete (group
// membership is not a deletion unit, ADR-0011). It returns an error naming the
// first violation, or nil.
func (m *Model) CheckG2() error {
	for txID, tx := range m.txs {
		for _, legID := range tx.LegIDs {
			op := m.ops[legID]
			want := legStatusFor(tx.Status, op.EditOf != nil)
			if op.Status == model.OpDeleted && want == model.OpConfirmed {
				continue // a leg of a COMMITTED group deleted afterwards
			}
			if op.Status != want {
				return fmt.Errorf("G2 violated: transaction %d (%s) has leg %d in status %s, want %s", txID, tx.Status, legID, op.Status, want)
			}
		}
	}
	return nil
}

// legStatusFor maps a transaction status to the status it implies for one leg:
// a regular leg of a COMMITTED group is CONFIRMED, an edit or delete leg APPLIED.
func legStatusFor(txStatus model.TxStatus, isEdit bool) model.OpStatus {
	switch txStatus {
	case model.TxCommitted:
		if isEdit {
			return model.OpApplied
		}
		return model.OpConfirmed
	case model.TxRejected:
		return model.OpInvalid
	default:
		return model.OpPending
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
