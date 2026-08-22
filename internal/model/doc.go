// Package model is BalanceDB's domain model (spec §5): the row types that mirror
// the per-cell schema, the operation/transaction status and reason-code
// vocabularies, and the two conversions the inviolables single out — the one
// amount helper (JSON number → int64 minor units) and the canonical payload hash
// used for idempotency.
//
// Money lives here and only here as far as parsing is concerned: ParseAmount is
// THE designated entry point (plan §0, CLAUDE.md inviolables). Nothing else in
// the codebase turns client JSON into a money value, so floating point can never
// leak in. The package holds types and pure functions only — no database access.
package model
