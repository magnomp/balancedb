package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/model"
)

// OperationOutcome is one operation's current state as both HTTP and embedded
// callers read it. For a regular operation Account/Amount/EffectiveAt are the
// current (possibly edited) values — its last values once DELETED, when
// DeletedAt/DeletedBy name the instant and the APPLIED delete registration
// (ADR-0011) — and Revision counts applied edits from 1. For an edit
// registration (EditOf set, ADR-0010) they are the proposed state as resolved at
// submission; for a delete registration (DeleteOf set) the target's values as
// they stood at submission, informational only. On either, ExpectedRevision is
// the optional guard and Revision is the meaningless column default. EditOf and
// DeleteOf are exclusive.
type OperationOutcome struct {
	ID               int64
	Status           model.OpStatus
	TransactionID    *int64
	Rejection        *model.Rejection
	Account          string
	Amount           int64
	EffectiveAt      time.Time
	Revision         int32
	EditOf           *int64
	DeleteOf         *int64
	ExpectedRevision *int32
	DeletedAt        *time.Time
	DeletedBy        *int64
}

// setTarget fills EditOf / DeleteOf exclusively from the row's edit_of and
// is_delete, exactly as OpOutcome does for insert results.
func (o *OperationOutcome) setTarget(editOf *int64, isDelete bool) {
	o.EditOf, o.DeleteOf = nil, nil
	if isDelete {
		o.DeleteOf = editOf
		return
	}
	o.EditOf = editOf
}

type TransactionOutcome struct {
	ID         int64
	Status     model.TxStatus
	OpCount    int
	Rejection  *model.Rejection
	Operations []OperationOutcome
}

func GetOperation(ctx context.Context, q Queryer, owner, id int64) (*OperationOutcome, error) {
	if owner <= 0 || id <= 0 {
		return nil, fmt.Errorf("%w: positive owner and operation ID required", ErrInvalidArgument)
	}
	const stmt = `SELECT o.id, o.status, o.transaction_id, o.invalidation_reason,
       a.external_id, o.amount, o.effective_at, o.revision, o.edit_of, o.is_delete, o.expected_revision,
       o.deleted_by, o.deleted_at
FROM operations o JOIN accounts a ON a.id=o.account_id WHERE o.id=$1 AND a.owner_id=$2`
	var (
		result   OperationOutcome
		reason   *string
		editOf   *int64
		isDelete bool
	)
	err := q.QueryRow(ctx, stmt, id, owner).Scan(&result.ID, &result.Status, &result.TransactionID, &reason,
		&result.Account, &result.Amount, &result.EffectiveAt, &result.Revision, &editOf, &isDelete, &result.ExpectedRevision,
		&result.DeletedBy, &result.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read operation: %w", err)
	}
	result.Rejection = parseRejection(reason)
	result.setTarget(editOf, isDelete)
	return &result, nil
}

// GetTransaction scopes both the parent and its legs to the requested owner.
// q must provide one consistent snapshot so terminal parent/leg states agree.
func GetTransaction(ctx context.Context, q Queryer, owner, id int64) (*TransactionOutcome, error) {
	if owner <= 0 || id <= 0 {
		return nil, fmt.Errorf("%w: positive owner and transaction ID required", ErrInvalidArgument)
	}
	const parent = `SELECT t.id, t.status, t.op_count, t.reject_reason FROM transactions t
WHERE t.id=$1 AND EXISTS (SELECT 1 FROM operations o JOIN accounts a ON a.id=o.account_id
WHERE o.transaction_id=t.id AND a.owner_id=$2)`
	var result TransactionOutcome
	var reason *string
	err := q.QueryRow(ctx, parent, id, owner).Scan(&result.ID, &result.Status, &result.OpCount, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read transaction: %w", err)
	}
	result.Rejection = parseRejection(reason)
	const legs = `SELECT o.id, o.status, o.transaction_id, o.invalidation_reason,
       a.external_id, o.amount, o.effective_at, o.revision, o.edit_of, o.is_delete, o.expected_revision,
       o.deleted_by, o.deleted_at
FROM operations o JOIN accounts a ON a.id=o.account_id
WHERE o.transaction_id=$1 AND a.owner_id=$2 ORDER BY o.id`
	rows, err := q.Query(ctx, legs, id, owner)
	if err != nil {
		return nil, fmt.Errorf("read transaction legs: %w", err)
	}
	defer rows.Close()
	result.Operations = []OperationOutcome{}
	for rows.Next() {
		var (
			op       OperationOutcome
			reason   *string
			editOf   *int64
			isDelete bool
		)
		if err := rows.Scan(&op.ID, &op.Status, &op.TransactionID, &reason,
			&op.Account, &op.Amount, &op.EffectiveAt, &op.Revision, &editOf, &isDelete, &op.ExpectedRevision,
			&op.DeletedBy, &op.DeletedAt); err != nil {
			return nil, fmt.Errorf("scan transaction leg: %w", err)
		}
		op.Rejection = parseRejection(reason)
		op.setTarget(editOf, isDelete)
		result.Operations = append(result.Operations, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transaction legs: %w", err)
	}
	if len(result.Operations) != result.OpCount {
		return nil, fmt.Errorf("transaction op_count mismatch")
	}
	return &result, nil
}

// Preserve the HTTP contract: malformed stored reason text yields no invented detail.
func parseRejection(reason *string) *model.Rejection {
	if reason == nil || *reason == "" {
		return nil
	}
	r, err := model.ParseRejection(*reason)
	if err != nil {
		return nil
	}
	return &r
}
