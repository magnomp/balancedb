// Package balancedb embeds the PostgreSQL-backed BalanceDB ledger in Go hosts.
//
// Call Migrate explicitly, then Open. Every application replica may call DB.Run;
// a database lease elects one processor per schema and fences stale leaders.
// DB.Insert registers operations inside an existing pgx transaction in the same
// database, including when the host's tables occupy another schema. Only the host
// commits or rolls back that transaction. Registration is atomic with host writes;
// balance validation and confirmation happen asynchronously after commit.
// Insert near the end of a short host transaction: concurrent commits can make
// higher operation IDs visible and decided before lower IDs (ADR-0008).
//
// CreateAccount and UpdateLimits also use the host transaction. GetAccount,
// GetBalance, GetStatement, GetOperation and GetTransaction read committed state
// through the handle's own pool, with one consistent snapshot per call.
//
// The host controls contexts, logging, signals and shutdown. No HTTP listener or
// background goroutine starts until the host calls Run. See docs/embedding.md and
// ADR-0007 for transaction and deployment contracts.
package balancedb
