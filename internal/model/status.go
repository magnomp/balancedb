package model

// OpStatus is an operation's lifecycle state (spec §5.3). A regular operation
// transitions PENDING → CONFIRMED or PENDING → INVALID; an edit registration
// (operations.edit_of set, ADR-0010) transitions PENDING → APPLIED or
// PENDING → INVALID. APPLIED and INVALID are terminal. An operation's history is
// append-only (CLAUDE.md inviolables): only the leader applying an edit may
// overwrite a CONFIRMED row's current columns, under the revision CAS.
type OpStatus string

const (
	OpPending   OpStatus = "PENDING"
	OpConfirmed OpStatus = "CONFIRMED"
	OpInvalid   OpStatus = "INVALID"
	// OpApplied is reached by edit registrations only, never by regular
	// operations, so every read that selects CONFIRMED rows excludes edits
	// without a query change.
	OpApplied OpStatus = "APPLIED"
)

// TxStatus is a group transaction's lifecycle state (spec §5.3). PENDING →
// COMMITTED or PENDING → REJECTED; REJECTED is terminal. All legs of a group flip
// together with their transaction row in one DB transaction (G2).
type TxStatus string

const (
	TxPending   TxStatus = "PENDING"
	TxCommitted TxStatus = "COMMITTED"
	TxRejected  TxStatus = "REJECTED"
)
