package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

// AccountBody is the account representation returned by the account endpoints.
// Limits are null when unbounded (spec §5.1).
type AccountBody struct {
	Account          string  `json:"account" doc:"Account external id (unique per owner)."`
	MinBalance       *Amount `json:"min_balance" doc:"Minimum final balance; null = unbounded."`
	MaxBalance       *Amount `json:"max_balance" doc:"Maximum final balance; null = unbounded."`
	ConfirmedBalance Amount  `json:"confirmed_balance" doc:"Final balance: the sum of all CONFIRMED operations (the object of guarantee G1)."`
	Version          int64   `json:"version" doc:"Optimistic-lock version; bumped by the processor on every confirmation and by limit updates."`
}

// --- POST /accounts ---------------------------------------------------------

// CreateAccountInput is the POST /accounts request.
type CreateAccountInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	Body    struct {
		ExternalID string  `json:"external_id" minLength:"1" doc:"Client-defined account external id (unique per owner)."`
		MinBalance *Amount `json:"min_balance,omitempty" doc:"Minimum final balance; omit or null for unbounded."`
		MaxBalance *Amount `json:"max_balance,omitempty" doc:"Maximum final balance; omit or null for unbounded."`
	}
}

// CreateAccountOutput / AccountOutput carry the created/updated account (201/200).
type AccountOutput struct {
	Body AccountBody
}

const insertAccount = `INSERT INTO accounts (owner_id, external_id, min_balance, max_balance)
VALUES ($1, $2, $3, $4)
ON CONFLICT (owner_id, external_id) DO NOTHING
RETURNING external_id, min_balance, max_balance, confirmed_balance, version`

func (s *Server) createAccount(ctx context.Context, in *CreateAccountInput) (*AccountOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	if err := validateLimitRange(in.Body.MinBalance, in.Body.MaxBalance); err != nil {
		return nil, err
	}

	var body AccountBody
	err = s.pool.QueryRow(ctx, insertAccount, ownerID, in.Body.ExternalID,
		amountPtr(in.Body.MinBalance), amountPtr(in.Body.MaxBalance)).
		Scan(&body.Account, &body.MinBalance, &body.MaxBalance, &body.ConfirmedBalance, &body.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned no row → the account already exists.
		return nil, huma.Error409Conflict("account already exists")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("create account", err)
	}
	return &AccountOutput{Body: body}, nil
}

// --- PUT /accounts/{ext}/limits ---------------------------------------------

// UpdateLimitsInput replaces an account's limits. PUT semantics: both fields are
// set; an omitted/null field means unbounded.
type UpdateLimitsInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	Ext     string `path:"ext" doc:"Account external id."`
	Body    struct {
		MinBalance *Amount `json:"min_balance,omitempty" doc:"New minimum final balance; omit or null for unbounded. Replaces the existing value (PUT)."`
		MaxBalance *Amount `json:"max_balance,omitempty" doc:"New maximum final balance; omit or null for unbounded. Replaces the existing value (PUT)."`
	}
}

const (
	selectAccountForUpdate = `SELECT id, confirmed_balance, version FROM accounts WHERE owner_id = $1 AND external_id = $2`
	updateAccountLimits    = `UPDATE accounts SET min_balance = $1, max_balance = $2, version = version + 1
WHERE id = $3 AND version = $4
RETURNING external_id, min_balance, max_balance, confirmed_balance, version`
)

func (s *Server) updateAccountLimits(ctx context.Context, in *UpdateLimitsInput) (*AccountOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	if err := validateLimitRange(in.Body.MinBalance, in.Body.MaxBalance); err != nil {
		return nil, err
	}

	// Version-guarded update with a single retry (spec §6): the version is the same
	// counter the processor bumps on confirmation, so an API-vs-processor race is
	// detected on either side. Read → validate against the read balance → CAS on
	// version; on a lost race re-read once, then surface 409.
	const attempts = 2
	for attempt := 0; attempt < attempts; attempt++ {
		var (
			id          int64
			confirmed   int64
			readVersion int64
		)
		err = s.pool.QueryRow(ctx, selectAccountForUpdate, ownerID, in.Ext).Scan(&id, &confirmed, &readVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("account not found")
		}
		if err != nil {
			return nil, huma.Error500InternalServerError("read account", err)
		}

		// §6: a new min is accepted only if min <= confirmed_balance; a new max only
		// if confirmed_balance <= max. Validated against the balance just read; the
		// version CAS below rejects the update if that balance has since changed.
		if in.Body.MinBalance != nil && int64(*in.Body.MinBalance) > confirmed {
			return nil, huma.Error422UnprocessableEntity(
				fmt.Sprintf("min_balance %d exceeds current confirmed_balance %d", int64(*in.Body.MinBalance), confirmed))
		}
		if in.Body.MaxBalance != nil && confirmed > int64(*in.Body.MaxBalance) {
			return nil, huma.Error422UnprocessableEntity(
				fmt.Sprintf("max_balance %d is below current confirmed_balance %d", int64(*in.Body.MaxBalance), confirmed))
		}

		if s.afterLimitsRead != nil {
			s.afterLimitsRead(ctx)
		}

		var body AccountBody
		err = s.pool.QueryRow(ctx, updateAccountLimits,
			amountPtr(in.Body.MinBalance), amountPtr(in.Body.MaxBalance), id, readVersion).
			Scan(&body.Account, &body.MinBalance, &body.MaxBalance, &body.ConfirmedBalance, &body.Version)
		if err == nil {
			return &AccountOutput{Body: body}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error500InternalServerError("update limits", err)
		}
		// pgx.ErrNoRows → the version CAS matched no row: a concurrent write bumped
		// the version. Retry (re-read) once; if it happens again, report the conflict.
	}
	return nil, huma.Error409Conflict("account was modified concurrently; limits not updated")
}

// --- GET /accounts/{ext}/balance --------------------------------------------

// GetBalanceInput is the balance query. With At set it is a point-in-time
// projection (§9); without it, the final balance.
type GetBalanceInput struct {
	OwnerID string    `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	Ext     string    `path:"ext" doc:"Account external id."`
	At      time.Time `query:"at" doc:"Optional RFC 3339 instant. When set, returns the derived balance at that instant (a projection that may violate limits, N2) instead of the final balance."`
}

// GetBalanceOutput carries the balance and echoes the instant it is measured at
// (absent means the final balance).
type GetBalanceOutput struct {
	Body struct {
		Account string     `json:"account"`
		Balance Amount     `json:"balance" doc:"Final balance, or the point-in-time projection when at was supplied."`
		At      *time.Time `json:"at,omitempty" doc:"The instant the balance is measured at; absent for the final balance."`
	}
}

const selectAccountBalance = `SELECT id, confirmed_balance FROM accounts WHERE owner_id = $1 AND external_id = $2`

func (s *Server) getBalance(ctx context.Context, in *GetBalanceInput) (*GetBalanceOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	var (
		accountID int64
		confirmed int64
	)
	err = s.pool.QueryRow(ctx, selectAccountBalance, ownerID, in.Ext).Scan(&accountID, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, huma.Error404NotFound("account not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("read balance", err)
	}

	out := &GetBalanceOutput{}
	out.Body.Account = in.Ext
	if in.At.IsZero() {
		out.Body.Balance = Amount(confirmed)
		return out, nil
	}

	balance, err := s.pointInTimeBalance(ctx, accountID, in.At)
	if err != nil {
		return nil, huma.Error500InternalServerError("project balance", err)
	}
	at := in.At.UTC()
	out.Body.Balance = Amount(balance)
	out.Body.At = &at
	return out, nil
}

// --- GET /accounts/{ext}/statement ------------------------------------------

// StatementInput is a keyset-paginated statement query.
type StatementInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	Ext     string `path:"ext" doc:"Account external id."`
	Limit   int    `query:"limit" default:"50" minimum:"1" maximum:"500" doc:"Maximum entries per page."`
	Cursor  string `query:"cursor" doc:"Opaque keyset cursor from a previous page's next_cursor. Omit for the first page."`
}

// StatementEntry is one CONFIRMED operation with the running balance at its
// timeline position.
type StatementEntry struct {
	ID             int64     `json:"id"`
	Amount         Amount    `json:"amount"`
	EffectiveAt    time.Time `json:"effective_at"`
	RunningBalance Amount    `json:"running_balance" doc:"Cumulative confirmed balance up to and including this operation, in timeline order."`
	TransactionID  *int64    `json:"transaction_id,omitempty"`
	ReversalOf     *int64    `json:"reversal_of,omitempty"`
}

// StatementOutput is a page of the statement plus the cursor for the next page.
type StatementOutput struct {
	Body struct {
		Account    string           `json:"account"`
		Entries    []StatementEntry `json:"entries"`
		NextCursor string           `json:"next_cursor,omitempty" doc:"Pass as ?cursor= to fetch the next page; absent on the last page."`
	}
}

const (
	selectAccountID2 = `SELECT id FROM accounts WHERE owner_id = $1 AND external_id = $2`

	statementFirstPage = `SELECT id, amount, effective_at, transaction_id, reversal_of
FROM operations
WHERE account_id = $1 AND status = 'CONFIRMED'
ORDER BY effective_at, id
LIMIT $2`

	statementAfterCursor = `SELECT id, amount, effective_at, transaction_id, reversal_of
FROM operations
WHERE account_id = $1 AND status = 'CONFIRMED' AND (effective_at, id) > ($3, $4)
ORDER BY effective_at, id
LIMIT $2`
)

func (s *Server) getStatement(ctx context.Context, in *StatementInput) (*StatementOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	var accountID int64
	err = s.pool.QueryRow(ctx, selectAccountID2, ownerID, in.Ext).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, huma.Error404NotFound("account not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("read account", err)
	}

	// Fetch one more than the page size to know whether a next page exists.
	fetch := in.Limit + 1

	var rows pgx.Rows
	if in.Cursor == "" {
		rows, err = s.pool.Query(ctx, statementFirstPage, accountID, fetch)
	} else {
		curEff, curID, cerr := decodeCursor(in.Cursor)
		if cerr != nil {
			return nil, huma.Error400BadRequest("invalid cursor")
		}
		rows, err = s.pool.Query(ctx, statementAfterCursor, accountID, fetch, curEff, curID)
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("read statement", err)
	}
	defer rows.Close()

	type rawEntry struct {
		id     int64
		amount int64
		eff    time.Time
		txID   *int64
		revOf  *int64
	}
	var raw []rawEntry
	for rows.Next() {
		var e rawEntry
		if err := rows.Scan(&e.id, &e.amount, &e.eff, &e.txID, &e.revOf); err != nil {
			return nil, huma.Error500InternalServerError("scan statement entry", err)
		}
		raw = append(raw, e)
	}
	if err := rows.Err(); err != nil {
		return nil, huma.Error500InternalServerError("iterate statement", err)
	}

	out := &StatementOutput{}
	out.Body.Account = in.Ext
	out.Body.Entries = []StatementEntry{}
	if len(raw) == 0 {
		return out, nil
	}

	// Trim the look-ahead row and, if it was present, emit the next cursor.
	hasNext := len(raw) > in.Limit
	if hasNext {
		raw = raw[:in.Limit]
	}

	// Seed the running balance from the cumulative confirmed balance strictly before
	// the first row of this page (§9: snapshot preceding the page + same-day prefix),
	// then prefix-sum within the page.
	running, err := s.cumulativeBefore(ctx, accountID, raw[0].eff, raw[0].id)
	if err != nil {
		return nil, huma.Error500InternalServerError("seed running balance", err)
	}
	for _, e := range raw {
		running += e.amount
		out.Body.Entries = append(out.Body.Entries, StatementEntry{
			ID:             e.id,
			Amount:         Amount(e.amount),
			EffectiveAt:    e.eff.UTC(),
			RunningBalance: Amount(running),
			TransactionID:  e.txID,
			ReversalOf:     e.revOf,
		})
	}
	if hasNext {
		last := raw[len(raw)-1]
		out.Body.NextCursor = encodeCursor(last.eff, last.id)
	}
	return out, nil
}

// --- read-path helpers ------------------------------------------------------

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
func (s *Server) pointInTimeBalance(ctx context.Context, accountID int64, at time.Time) (int64, error) {
	utc := at.UTC()
	dayStr := utc.Format("2006-01-02")
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)

	seed, err := s.snapshotBeforeDay(ctx, accountID, dayStr)
	if err != nil {
		return 0, err
	}
	var sameDay int64
	if err := s.pool.QueryRow(ctx, sumConfirmedUpTo, accountID, dayStart, utc).Scan(&sameDay); err != nil {
		return 0, fmt.Errorf("sum same-day operations: %w", err)
	}
	return seed + sameDay, nil
}

// cumulativeBefore returns the cumulative confirmed balance strictly before the
// timeline position (eff, id): the snapshot before eff's UTC day plus the sum of
// same-day CONFIRMED operations that precede the position.
func (s *Server) cumulativeBefore(ctx context.Context, accountID int64, eff time.Time, id int64) (int64, error) {
	utc := eff.UTC()
	dayStr := utc.Format("2006-01-02")
	dayStart := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)

	seed, err := s.snapshotBeforeDay(ctx, accountID, dayStr)
	if err != nil {
		return 0, err
	}
	var prefix int64
	if err := s.pool.QueryRow(ctx, sumConfirmedBeforePos, accountID, dayStart, utc, id).Scan(&prefix); err != nil {
		return 0, fmt.Errorf("sum same-day prefix: %w", err)
	}
	return seed + prefix, nil
}

// snapshotBeforeDay returns the cumulative snapshot balance for the latest day
// strictly before dayStr, or 0 if there is none.
func (s *Server) snapshotBeforeDay(ctx context.Context, accountID int64, dayStr string) (int64, error) {
	var balance int64
	err := s.pool.QueryRow(ctx, selectSnapshotBeforeDay, accountID, dayStr).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read snapshot: %w", err)
	}
	return balance, nil
}

// --- small helpers ----------------------------------------------------------

// amountPtr converts an optional Amount to the *int64 the SQL layer binds (NULL
// when unbounded).
func amountPtr(a *Amount) *int64 {
	if a == nil {
		return nil
	}
	v := int64(*a)
	return &v
}

// validateLimitRange rejects an inverted [min, max] range before it reaches the DB.
func validateLimitRange(minB, maxB *Amount) error {
	if minB != nil && maxB != nil && int64(*minB) > int64(*maxB) {
		return huma.Error422UnprocessableEntity(
			fmt.Sprintf("min_balance %d is greater than max_balance %d", int64(*minB), int64(*maxB)))
	}
	return nil
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
