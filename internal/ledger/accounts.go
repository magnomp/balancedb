package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/model"
)

// Queryer is the SQL read surface shared by pgx transactions and pools.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

var (
	ErrNotFound         = errors.New("ledger: record not found")
	ErrAccountExists    = errors.New("ledger: account already exists")
	ErrInvalidArgument  = errors.New("ledger: invalid argument")
	ErrInvalidLimits    = errors.New("ledger: invalid balance limits")
	ErrConcurrentUpdate = errors.New("ledger: account modified concurrently")
)

type Account = model.Account

// Limits replaces both bounds. A nil bound means unbounded.
type Limits struct{ MinBalance, MaxBalance *int64 }

func validateAccount(owner int64, externalID string) error {
	if owner <= 0 || externalID == "" {
		return fmt.Errorf("%w: positive owner and nonempty account required", ErrInvalidArgument)
	}
	return nil
}

func validateLimits(limits Limits, balance int64) error {
	if limits.MinBalance != nil && limits.MaxBalance != nil && *limits.MinBalance > *limits.MaxBalance {
		return fmt.Errorf("%w: min_balance exceeds max_balance", ErrInvalidLimits)
	}
	if limits.MinBalance != nil && *limits.MinBalance > balance {
		return fmt.Errorf("%w: min_balance exceeds confirmed_balance %d", ErrInvalidLimits, balance)
	}
	if limits.MaxBalance != nil && *limits.MaxBalance < balance {
		return fmt.Errorf("%w: max_balance is below confirmed_balance %d", ErrInvalidLimits, balance)
	}
	return nil
}

func scanAccount(row pgx.Row) (*Account, error) {
	var a Account
	if err := row.Scan(&a.ID, &a.OwnerID, &a.ExternalID, &a.MinBalance, &a.MaxBalance, &a.ConfirmedBalance, &a.Version, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAccount creates a zero-balance account; limits must contain zero (G1).
func CreateAccount(ctx context.Context, q Queryer, owner int64, externalID string, limits Limits) (*Account, error) {
	if err := validateAccount(owner, externalID); err != nil {
		return nil, err
	}
	if err := validateLimits(limits, 0); err != nil {
		return nil, err
	}
	const stmt = `INSERT INTO accounts (owner_id, external_id, min_balance, max_balance)
VALUES ($1, $2, $3, $4) ON CONFLICT (owner_id, external_id) DO NOTHING
RETURNING id, owner_id, external_id, min_balance, max_balance, confirmed_balance, version, created_at`
	a, err := scanAccount(q.QueryRow(ctx, stmt, owner, externalID, limits.MinBalance, limits.MaxBalance))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccountExists
	}
	if err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	return a, nil
}

func GetAccount(ctx context.Context, q Queryer, owner int64, externalID string) (*Account, error) {
	if err := validateAccount(owner, externalID); err != nil {
		return nil, err
	}
	const stmt = `SELECT id, owner_id, external_id, min_balance, max_balance, confirmed_balance, version, created_at
FROM accounts WHERE owner_id=$1 AND external_id=$2`
	a, err := scanAccount(q.QueryRow(ctx, stmt, owner, externalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read account: %w", err)
	}
	return a, nil
}

// UpdateLimits preserves the spec §6 two-attempt read/validate/version-CAS path.
// afterRead is a test seam; production passes nil.
func UpdateLimits(ctx context.Context, q Queryer, owner int64, externalID string, limits Limits, afterRead func(context.Context)) (*Account, error) {
	for range 2 {
		a, err := GetAccount(ctx, q, owner, externalID)
		if err != nil {
			return nil, err
		}
		if err := validateLimits(limits, a.ConfirmedBalance); err != nil {
			return nil, err
		}
		if afterRead != nil {
			afterRead(ctx)
		}
		const stmt = `UPDATE accounts SET min_balance=$1, max_balance=$2, version=version+1
WHERE id=$3 AND version=$4
RETURNING id, owner_id, external_id, min_balance, max_balance, confirmed_balance, version, created_at`
		updated, err := scanAccount(q.QueryRow(ctx, stmt, limits.MinBalance, limits.MaxBalance, a.ID, a.Version))
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("update limits: %w", err)
		}
	}
	return nil, ErrConcurrentUpdate
}
