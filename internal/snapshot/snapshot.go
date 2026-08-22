package snapshot

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// execer is the subset of a pgx querier Apply needs. Both pgx.Tx and *pgxpool.Pool
// satisfy it; the processor always passes the transaction that is flipping the
// operation to CONFIRMED, so the snapshot writes commit atomically with it.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	// upsertDay writes the operation's own UTC-day snapshot. On a fresh day it is
	// seeded from the balance carried by the most recent earlier snapshot (0 if
	// none) plus v, so the new day starts at the correct cumulative total; on an
	// existing day it simply gains v. $1 account, $2 day (as 'YYYY-MM-DD', cast to
	// date), $3 amount.
	upsertDay = `INSERT INTO balance_snapshots (account_id, day, balance)
VALUES ($1, $2::date,
        COALESCE((SELECT balance FROM balance_snapshots
                   WHERE account_id = $1 AND day < $2::date
                   ORDER BY day DESC LIMIT 1), 0) + $3)
ON CONFLICT (account_id, day) DO UPDATE SET balance = balance_snapshots.balance + $3`

	// cascadeLater is the sparse cascade (spec §8.4): add v to every snapshot day
	// strictly after the operation's day. Only days that already exist are touched.
	cascadeLater = `UPDATE balance_snapshots SET balance = balance + $3
 WHERE account_id = $1 AND day > $2::date`
)

// dayFormat renders the UTC calendar day for the DATE column. The day bucket is
// the UTC bucket of effective_at, fixed forever (spec §5.2). Computing it from the
// operation's own effective_at is a pure data transformation — not a clock read —
// so it is done in Go and passed as a literal date, independent of the database
// session's timezone.
const dayFormat = "2006-01-02"

// Apply records a confirmed operation of amount v against account a on the UTC day
// of effectiveAt: it upserts that day's cumulative snapshot and cascades v into
// every existing later day (spec §8.4). It must run inside the transaction that
// confirms the operation.
func Apply(ctx context.Context, db execer, accountID int64, effectiveAt time.Time, amount int64) error {
	day := effectiveAt.UTC().Format(dayFormat)
	if _, err := db.Exec(ctx, upsertDay, accountID, day, amount); err != nil {
		return fmt.Errorf("snapshot upsert (account %d, day %s): %w", accountID, day, err)
	}
	if _, err := db.Exec(ctx, cascadeLater, accountID, day, amount); err != nil {
		return fmt.Errorf("snapshot cascade (account %d, day %s): %w", accountID, day, err)
	}
	return nil
}
