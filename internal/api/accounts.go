package api

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/ledger"
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

// CreateAccountInput is the POST /accounts request.
type CreateAccountInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	Body    struct {
		ExternalID string  `json:"external_id" minLength:"1" doc:"Client-defined account external id (unique per owner)."`
		MinBalance *Amount `json:"min_balance,omitempty" doc:"Minimum final balance; omit or null for unbounded."`
		MaxBalance *Amount `json:"max_balance,omitempty" doc:"Maximum final balance; omit or null for unbounded."`
	}
}

// AccountOutput carries the created/updated account (201/200).
type AccountOutput struct {
	Body AccountBody
}

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

func accountBody(a *ledger.Account) AccountBody {
	return AccountBody{Account: a.ExternalID, MinBalance: toAmount(a.MinBalance), MaxBalance: toAmount(a.MaxBalance), ConfirmedBalance: Amount(a.ConfirmedBalance), Version: a.Version}
}

func toAmount(value *int64) *Amount {
	if value == nil {
		return nil
	}
	converted := Amount(*value)
	return &converted
}

func amountPtr(value *Amount) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}

func (s *Server) createAccount(ctx context.Context, in *CreateAccountInput) (*AccountOutput, error) {
	owner, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	a, err := ledger.CreateAccount(ctx, s.pool, owner, in.Body.ExternalID, ledger.Limits{MinBalance: amountPtr(in.Body.MinBalance), MaxBalance: amountPtr(in.Body.MaxBalance)})
	if err != nil {
		return nil, mapLedgerErr(err)
	}
	return &AccountOutput{Body: accountBody(a)}, nil
}

func (s *Server) updateAccountLimits(ctx context.Context, in *UpdateLimitsInput) (*AccountOutput, error) {
	owner, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	a, err := ledger.UpdateLimits(ctx, s.pool, owner, in.Ext, ledger.Limits{MinBalance: amountPtr(in.Body.MinBalance), MaxBalance: amountPtr(in.Body.MaxBalance)}, s.afterLimitsRead)
	if err != nil {
		return nil, mapLedgerErr(err)
	}
	return &AccountOutput{Body: accountBody(a)}, nil
}

func (s *Server) getBalance(ctx context.Context, in *GetBalanceInput) (*GetBalanceOutput, error) {
	owner, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	var at *time.Time
	if !in.At.IsZero() {
		at = &in.At
	}
	value, err := db.Read(ctx, s.pool, func(tx pgx.Tx) (*ledger.Balance, error) { return ledger.GetBalance(ctx, tx, owner, in.Ext, at) })
	if err != nil {
		return nil, mapLedgerErr(err)
	}
	out := &GetBalanceOutput{}
	out.Body.Account, out.Body.Balance, out.Body.At = value.Account, Amount(value.Balance), value.At
	return out, nil
}

func (s *Server) getStatement(ctx context.Context, in *StatementInput) (*StatementOutput, error) {
	owner, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	value, err := db.Read(ctx, s.pool, func(tx pgx.Tx) (*ledger.Statement, error) {
		return ledger.GetStatement(ctx, tx, owner, in.Ext, ledger.StatementOptions{Limit: in.Limit, Cursor: in.Cursor})
	})
	if err != nil {
		return nil, mapLedgerErr(err)
	}
	out := &StatementOutput{}
	out.Body.Account, out.Body.NextCursor = value.Account, value.NextCursor
	out.Body.Entries = make([]StatementEntry, len(value.Entries))
	for i, e := range value.Entries {
		out.Body.Entries[i] = StatementEntry{ID: e.ID, Amount: Amount(e.Amount), EffectiveAt: e.EffectiveAt, RunningBalance: Amount(e.RunningBalance), TransactionID: e.TransactionID, ReversalOf: e.ReversalOf}
	}
	return out, nil
}
