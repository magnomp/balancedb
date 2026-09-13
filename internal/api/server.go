package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// outcomeWaiter is the slice of internal/notify the wait path (createTransaction,
// wait_ms > 0) depends on: register a waiter for an outcome key and get a signal
// channel plus an unregister func. Kept as an interface so the API package does not
// hard-depend on the concrete Notifier and tests can substitute a fake. A nil
// notifier disables the NOTIFY fast path; waiting then resolves purely by status
// poll (the durability fallback is always the correctness path, ADR-0002).
type outcomeWaiter interface {
	Register(key string) (<-chan struct{}, func())
}

// WaitObserver records synchronous-wait latency for the §13 "NOTIFY→outcome lag"
// metric (API wait health, ADR-0002). source is how the wait resolved
// (obs.WaitSource*) and result is decided|pending (obs.WaitResult*). Kept an
// interface so the api package does not depend on internal/obs; *obs.Metrics
// satisfies it. A nil observer disables the measurement.
type WaitObserver interface {
	ObserveWait(seconds float64, source, result string)
}

// apiTitle/apiVersion identify the generated contract. apiVersion is deliberately
// held stable and hand-bumped: it appears in api/openapi.yaml, so letting it drift
// automatically would make the committed spec churn on every build (ADR-0003).
const (
	apiTitle   = "BalanceDB API"
	apiVersion = "1.0.0"
)

// Server is the HTTP surface of a cell (spec §10), built on Huma v2 over a chi mux
// (ADR-0003). Huma is confined to this package (the ADR boundary rule): every
// endpoint is a typed operation whose request/response structs *are* the OpenAPI
// contract, so docs, validation, and the exported spec cannot drift from the code.
//
// The pool is used only by the query/insert handlers; the owner resolver scopes
// every request to one owner (§2). A Server built with a nil pool is still a
// complete contract — that is how cmd/openapigen exports the spec without a DB.
type Server struct {
	pool     *pgxpool.Pool
	resolver OwnerResolver
	notifier outcomeWaiter
	waitObs  WaitObserver
	api      huma.API
	mux      *chi.Mux

	// pollInterval is the wait path's status-poll cadence — the durability fallback
	// that resolves a synchronous wait even when the outcome NOTIFY is dropped
	// (spec §10.1 names 1–2 s). Zero means defaultPollInterval. Tests set it small.
	pollInterval time.Duration

	// afterLimitsRead, when non-nil, runs between the version read and the
	// version-guarded UPDATE in updateAccountLimits. Production leaves it nil; tests
	// use it to force the version CAS to miss deterministically, exercising the
	// retry-then-409 path (mirrors the processor's afterAccountRead seam, M5). It is
	// a test seam, not behavior.
	afterLimitsRead func(context.Context)
}

// NewServer builds the router, registers every §10 operation, and wires Huma's
// /docs (interactive, request-executing) and /openapi.{yaml,json} endpoints. A nil
// resolver defaults to the single-header implementation. A nil notifier is allowed
// (spec export and poll-only deployments): the wait path then relies solely on its
// status poll.
func NewServer(pool *pgxpool.Pool, resolver OwnerResolver, notifier outcomeWaiter) *Server {
	if resolver == nil {
		resolver = HeaderOwnerResolver{}
	}

	mux := chi.NewMux()
	cfg := huma.DefaultConfig(apiTitle, apiVersion)
	cfg.Info.Description = "BalanceDB — a Postgres-backed balance-maintenance engine (ledger). " +
		"Clients insert money operations (singles or atomic groups); a single elected " +
		"processor selects visible committed work in ID order and validates it so an account's " +
		"final balance never breaks its configured limits. Insertion is asynchronous: " +
		"POST /transactions returns 202 and outcomes are queried later."

	api := humachi.New(mux, cfg)
	s := &Server{pool: pool, resolver: resolver, notifier: notifier, api: api, mux: mux}
	s.register()
	return s
}

// SetWaitObserver wires the synchronous-wait latency metric (spec §13, ADR-0002).
// Call it before serving; nil (the default) leaves the wait path unmeasured. Kept
// a setter rather than a constructor parameter so existing NewServer callers and
// tests are unaffected.
func (s *Server) SetWaitObserver(o WaitObserver) { s.waitObs = o }

// Handler returns the http.Handler serving the API, /docs, and /openapi.*.
func (s *Server) Handler() http.Handler { return s.mux }

// OpenAPIYAML renders the generated OpenAPI 3.1 document as YAML. This is the
// exact artifact committed to api/openapi.yaml (ADR-0003, `make openapi`).
func (s *Server) OpenAPIYAML() ([]byte, error) { return s.api.OpenAPI().YAML() }

// OpenAPIYAML renders the contract without needing a database — the operations and
// their schemas are fixed at registration, independent of the pool. cmd/openapigen
// uses this so spec export never touches Postgres.
func OpenAPIYAML() ([]byte, error) {
	return NewServer(nil, nil, nil).OpenAPIYAML()
}

// register declares every §10 endpoint as a typed Huma operation. Kept in one
// place so the full contract surface is greppable.
func (s *Server) register() {
	huma.Register(s.api, huma.Operation{
		OperationID:   "createTransaction",
		Method:        http.MethodPost,
		Path:          "/transactions",
		Summary:       "Insert an operation or atomic group",
		Description:   "Registers one atomic unit: a single operation (no transaction row) or a group of 2..max_group_size legs, all belonging to one owner (§2). By default returns 202 immediately (fire-and-forget); the outcome is decided asynchronously and read back via GET /transactions/{id} or GET /operations/{id}. With wait_ms > 0 the call waits up to that budget (capped by api_max_wait_ms) for the decision via LISTEN/NOTIFY with a status-poll fallback: if the outcome is decided in time it returns 200 with the decided status, otherwise 202 with the current (still PENDING) state. Waiting is strictly optional and never guaranteed to return a final outcome.",
		Tags:          []string{"Insertion"},
		DefaultStatus: http.StatusAccepted,
	}, s.createTransaction)

	huma.Register(s.api, huma.Operation{
		OperationID: "getTransaction",
		Method:      http.MethodGet,
		Path:        "/transactions/{id}",
		Summary:     "Get a group transaction and its per-leg statuses",
		Tags:        []string{"Queries"},
	}, s.getTransaction)

	huma.Register(s.api, huma.Operation{
		OperationID: "getOperation",
		Method:      http.MethodGet,
		Path:        "/operations/{id}",
		Summary:     "Get an operation's status and, if INVALID, its rejection detail",
		Tags:        []string{"Queries"},
	}, s.getOperation)

	huma.Register(s.api, huma.Operation{
		OperationID: "getBalance",
		Method:      http.MethodGet,
		Path:        "/accounts/{ext}/balance",
		Summary:     "Get an account's final balance, or a point-in-time projection with ?at=T",
		Description: "Without ?at, returns the final balance (sum of all CONFIRMED operations; the object of guarantee G1). With ?at=T, returns the derived balance at instant T — a projection that may violate configured limits (non-guarantee N2), including at T=now() when future-dated operations exist (N5).",
		Tags:        []string{"Queries"},
	}, s.getBalance)

	huma.Register(s.api, huma.Operation{
		OperationID: "getStatement",
		Method:      http.MethodGet,
		Path:        "/accounts/{ext}/statement",
		Summary:     "Get a paged account statement with a derived running balance",
		Description: "Pages over the CONFIRMED timeline in (effective_at, id) order (guarantee G4), seeded from the balance snapshot preceding the page. Keyset pagination: pass the returned next_cursor to fetch the following page.",
		Tags:        []string{"Queries"},
	}, s.getStatement)

	huma.Register(s.api, huma.Operation{
		OperationID:   "createAccount",
		Method:        http.MethodPost,
		Path:          "/accounts",
		Summary:       "Create an account explicitly with optional limits",
		Description:   "Accounts are also created on demand at insertion; this endpoint creates one explicitly, with optional min/max balance limits (null = unbounded). Returns 409 if the account already exists.",
		Tags:          []string{"Accounts"},
		DefaultStatus: http.StatusCreated,
	}, s.createAccount)

	huma.Register(s.api, huma.Operation{
		OperationID: "updateAccountLimits",
		Method:      http.MethodPut,
		Path:        "/accounts/{ext}/limits",
		Summary:     "Replace an account's balance limits (§6 rules, version-guarded)",
		Description: "Sets both min_balance and max_balance (omit a field or send null for unbounded). Per §6 a new min is accepted only if min <= confirmed_balance and a new max only if confirmed_balance <= max. The update is optimistically guarded on the same version the processor bumps; a lost race is retried once, then returns 409.",
		Tags:        []string{"Accounts"},
	}, s.updateAccountLimits)
}
