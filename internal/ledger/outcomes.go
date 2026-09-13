package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/model"
)

type OperationOutcome struct {
	ID            int64
	Status        model.OpStatus
	TransactionID *int64
	Rejection     *model.Rejection
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
	const stmt = `SELECT o.id, o.status, o.transaction_id, o.invalidation_reason
FROM operations o JOIN accounts a ON a.id=o.account_id WHERE o.id=$1 AND a.owner_id=$2`
	var result OperationOutcome
	var reason *string
	err := q.QueryRow(ctx, stmt, id, owner).Scan(&result.ID, &result.Status, &result.TransactionID, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read operation: %w", err)
	}
	result.Rejection = parseRejection(reason)
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
	const legs = `SELECT o.id, o.status, o.transaction_id, o.invalidation_reason
FROM operations o JOIN accounts a ON a.id=o.account_id
WHERE o.transaction_id=$1 AND a.owner_id=$2 ORDER BY o.id`
	rows, err := q.Query(ctx, legs, id, owner)
	if err != nil {
		return nil, fmt.Errorf("read transaction legs: %w", err)
	}
	defer rows.Close()
	result.Operations = []OperationOutcome{}
	for rows.Next() {
		var op OperationOutcome
		var reason *string
		if err := rows.Scan(&op.ID, &op.Status, &op.TransactionID, &reason); err != nil {
			return nil, fmt.Errorf("scan transaction leg: %w", err)
		}
		op.Rejection = parseRejection(reason)
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
