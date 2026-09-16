package model

// OpStatus is an operation's lifecycle state (spec §5.3). A regular operation
// transitions PENDING → CONFIRMED → DELETED or PENDING → INVALID; an edit-class
// registration (operations.edit_of set — an edit, ADR-0010, or a delete when
// is_delete is set, ADR-0011) transitions PENDING → APPLIED or PENDING → INVALID.
// DELETED, APPLIED and INVALID are terminal. An operation's history is
// append-only (CLAUDE.md inviolables): only the leader applying an edit may
// overwrite a CONFIRMED row's current columns, and only the leader applying a
// delete may flip CONFIRMED → DELETED — both under the revision CAS.
type OpStatus string

const (
	OpPending   OpStatus = "PENDING"
	OpConfirmed OpStatus = "CONFIRMED"
	OpInvalid   OpStatus = "INVALID"
	// OpApplied is reached by edit registrations only, never by regular
	// operations, so every read that selects CONFIRMED rows excludes edits
	// without a query change.
	OpApplied OpStatus = "APPLIED"
	// OpDeleted is reached by regular operations only (CONFIRMED → DELETED when
	// the leader applies a delete registration, ADR-0011) and is terminal: a
	// DELETED row is never CONFIRMED again, so every read that selects CONFIRMED
	// rows excludes deleted work without a query change. The row keeps its last
	// values; deleted_by/deleted_at record the applied delete and its instant.
	OpDeleted OpStatus = "DELETED"
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
