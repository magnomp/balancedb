# ADR-0009 — Embedded account and query API

Status: accepted (2026-09-13, completing Go embedding)

## Context

Embedded hosts can insert work and run the processor, but account configuration,
balances, statements and outcome queries still require HTTP. Exposing these in Go
must preserve owner scoping, integer money, the account-version CAS, and the
caller-owned transaction contract of ADR-0008.

## Decision

Move account/query SQL and domain errors into internal/ledger and let both HTTP
handlers and the public Go API call that core. Public CreateAccount and UpdateLimits
accept the host's pgx.Tx and reuse savepoint/search_path isolation. Reads use the
handle's schema-bound pool, scoped by explicit owner ID; unknown and foreign-owner
records both return ErrNotFound. Expose typed account, balance, statement and
operation/group outcome values and errors.Is sentinels without HTTP dependencies.

Each public/HTTP composite read uses one read-only REPEATABLE READ transaction so
snapshot seeds, operation sums, statement rows and group statuses come from the
same database view. Separate statement pages are separate views; confirmations
and backdated operations between pages can change later projections. The cursor
remains the existing (effective_at, id) encoding. Projections that cannot fit in
int64 return an error rather than silently overflowing running-balance arithmetic.

Account creation must validate that its initial zero balance fits the requested
limits, as required by G1. The prior HTTP path checked only min <= max, allowing a
new account to start outside its own limits. Both transports now reject that with
the same domain error (HTTP 422). Updates retain the two-attempt version-CAS path,
including re-reading and revalidating after a miss. No processing guards change.

## Consequences

Hosts can manage accounts and read ledger state without REST, transaction wrappers
or before-commit hooks. Reads observe committed state; uncommitted host writes are
not visible through the handle's independent pool. Account writes remain provisional
until the host commits. Existing insertion ordering risks and owner boundaries
remain documented. Migrations, leases and notification behavior are unchanged.
No dependencies or migrations are added. OpenAPI shapes stay stable; account
creation rejects limit ranges that violate G1. Tests and the reference model cover
that refinement plus owner/schema isolation and read consistency.
