package model

// OpStatus is an operation's lifecycle state (spec §5.3). The only transitions
// are PENDING → CONFIRMED and PENDING → INVALID; INVALID is terminal. Facts are
// never mutated after they are set (CLAUDE.md inviolables).
type OpStatus string

const (
	OpPending   OpStatus = "PENDING"
	OpConfirmed OpStatus = "CONFIRMED"
	OpInvalid   OpStatus = "INVALID"
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
