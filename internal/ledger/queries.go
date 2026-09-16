package ledger

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Balance struct {
	Account string
	Balance int64
	At      *time.Time
}

type StatementOptions struct {
	Limit  int // Zero selects 50; allowed range 1..500.
	Cursor string
}

type StatementEntry struct {
	ID             int64
	Amount         int64
	EffectiveAt    time.Time
	RunningBalance int64
	TransactionID  *int64
	ReversalOf     *int64
	Revision       int32 // 1 for a never-edited operation (ADR-0010).
}

type Statement struct {
	Account    string
	Entries    []StatementEntry
	NextCursor string
}

// GetBalance returns the final balance for nil at, or its projection at an instant.
// q must hold one consistent snapshot for a point-in-time projection (ADR-0009).
func GetBalance(ctx context.Context, q Queryer, owner int64, externalID string, at *time.Time) (*Balance, error) {
	a, err := GetAccount(ctx, q, owner, externalID)
	if err != nil {
		return nil, err
	}
	result := &Balance{Account: externalID, Balance: a.ConfirmedBalance}
	if at == nil {
		return result, nil
	}
	utc := at.UTC()
	result.At = &utc
	result.Balance, err = pointInTimeBalance(ctx, q, a.ID, utc)
	if err != nil {
		return nil, err
	}
	return result, nil
}

const (
	statementFirstPage = `SELECT id, amount, effective_at, transaction_id, reversal_of, revision
FROM operations
WHERE account_id = $1 AND status = 'CONFIRMED'
ORDER BY effective_at, id
LIMIT $2`

	statementAfterCursor = `SELECT id, amount, effective_at, transaction_id, reversal_of, revision
FROM operations
WHERE account_id = $1 AND status = 'CONFIRMED' AND (effective_at, id) > ($3, $4)
ORDER BY effective_at, id
LIMIT $2`
)

// GetStatement uses keyset pagination. q must provide a consistent snapshot for
// the page and its running-balance seed. Pages themselves are independent views.
func GetStatement(ctx context.Context, q Queryer, owner int64, externalID string, opts StatementOptions) (*Statement, error) {
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if opts.Limit < 1 || opts.Limit > 500 {
		return nil, fmt.Errorf("%w: statement limit must be 1..500", ErrInvalidArgument)
	}
	var eff time.Time
	var id int64
	if opts.Cursor != "" {
		var err error
		eff, id, err = decodeCursor(opts.Cursor)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
		}
	}
	a, err := GetAccount(ctx, q, owner, externalID)
	if err != nil {
		return nil, err
	}
	var rows pgx.Rows
	if opts.Cursor == "" {
		rows, err = q.Query(ctx, statementFirstPage, a.ID, opts.Limit+1)
	} else {
		rows, err = q.Query(ctx, statementAfterCursor, a.ID, opts.Limit+1, eff, id)
	}
	if err != nil {
		return nil, fmt.Errorf("read statement: %w", err)
	}
	defer rows.Close()
	entries := make([]StatementEntry, 0, opts.Limit+1)
	for rows.Next() {
		var entry StatementEntry
		if err := rows.Scan(&entry.ID, &entry.Amount, &entry.EffectiveAt, &entry.TransactionID, &entry.ReversalOf, &entry.Revision); err != nil {
			return nil, fmt.Errorf("scan statement: %w", err)
		}
		entry.EffectiveAt = entry.EffectiveAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate statement: %w", err)
	}
	rows.Close() // Release the transaction connection before computing the seed.
	result := &Statement{Account: externalID, Entries: entries}
	if len(entries) == 0 {
		return result, nil
	}
	if len(entries) > opts.Limit {
		result.Entries = entries[:opts.Limit]
		last := result.Entries[len(result.Entries)-1]
		result.NextCursor = encodeCursor(last.EffectiveAt, last.ID)
	}
	first := result.Entries[0]
	running, err := cumulativeBefore(ctx, q, a.ID, first.EffectiveAt, first.ID)
	if err != nil {
		return nil, err
	}
	for i := range result.Entries {
		running, err = addBalance(running, result.Entries[i].Amount)
		if err != nil {
			return nil, err
		}
		result.Entries[i].RunningBalance = running
	}
	return result, nil
}

const (
	selectSnapshotBeforeDay = `SELECT balance FROM balance_snapshots
WHERE account_id = $1 AND day < $2::date
ORDER BY day DESC LIMIT 1`

	// Sum of CONFIRMED operations on the query day up to and including instant T.
	sumConfirmedUpTo = `SELECT COALESCE(SUM(amount), 0) FROM operations
WHERE account_id = $1 AND status = 'CONFIRMED' AND effective_at >= $2 AND effective_at <= $3`

	// Sum of CONFIRMED operations on a day strictly before a (effective_at, id)
	// timeline position — the same-day prefix used to seed a statement page.
	sumConfirmedBeforePos = `SELECT COALESCE(SUM(amount), 0) FROM operations
WHERE account_id = $1 AND status = 'CONFIRMED' AND effective_at >= $2 AND (effective_at, id) < ($3, $4)`
)

// pointInTimeBalance derives the balance at instant T (§9): the last snapshot
// before T's UTC day plus the sum of CONFIRMED operations of T's day with
// effective_at <= T. Day boundaries are computed in UTC so the snapshot buckets
// (UTC, fixed forever, spec §5.2) are honoured regardless of the DB session zone.
func pointInTimeBalance(ctx context.Context, q Queryer, accountID int64, at time.Time) (int64, error) {
	utc := at.UTC()
	dayStr := utc.Format("2006-01-02")
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)

	seed, err := snapshotBeforeDay(ctx, q, accountID, dayStr)
	if err != nil {
		return 0, err
	}
	var sameDay int64
	if err := q.QueryRow(ctx, sumConfirmedUpTo, accountID, dayStart, utc).Scan(&sameDay); err != nil {
		return 0, fmt.Errorf("sum same-day operations: %w", err)
	}
	return addBalance(seed, sameDay)
}

// cumulativeBefore returns the cumulative confirmed balance strictly before the
// timeline position (eff, id): the snapshot before eff's UTC day plus the sum of
// same-day CONFIRMED operations that precede the position.
func cumulativeBefore(ctx context.Context, q Queryer, accountID int64, eff time.Time, id int64) (int64, error) {
	utc := eff.UTC()
	dayStr := utc.Format("2006-01-02")
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)

	seed, err := snapshotBeforeDay(ctx, q, accountID, dayStr)
	if err != nil {
		return 0, err
	}
	var prefix int64
	if err := q.QueryRow(ctx, sumConfirmedBeforePos, accountID, dayStart, utc, id).Scan(&prefix); err != nil {
		return 0, fmt.Errorf("sum same-day prefix: %w", err)
	}
	return addBalance(seed, prefix)
}

// snapshotBeforeDay returns the cumulative snapshot balance for the latest day
// strictly before dayStr, or 0 if there is none.
func snapshotBeforeDay(ctx context.Context, q Queryer, accountID int64, dayStr string) (int64, error) {
	var balance int64
	err := q.QueryRow(ctx, selectSnapshotBeforeDay, accountID, dayStr).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read snapshot: %w", err)
	}
	return balance, nil
}

// encodeCursor / decodeCursor make an opaque keyset cursor from a timeline
// position (effective_at, id). The instant is RFC 3339 nanos in UTC so the cursor
// round-trips exactly.
func encodeCursor(eff time.Time, id int64) string {
	raw := eff.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("decode cursor: %w", err)
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("malformed cursor")
	}
	eff, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("cursor timestamp: %w", err)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("cursor id: %w", err)
	}
	return eff, id, nil
}

// Point-in-time projections may exceed int64 even when the final balance fits.
// Return an explicit error rather than silently wrapping money arithmetic.
var ErrBalanceOverflow = errors.New("ledger: projected balance exceeds int64")

func addBalance(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrBalanceOverflow
	}
	return a + b, nil
}
