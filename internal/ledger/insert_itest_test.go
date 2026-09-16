//go:build itest

package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// Edit registration itests (IT-003 … IT-009): every sentinel, owner scoping,
// replay and payload conflict for edit items. Nothing is decided here — rows
// land PENDING for the processor.

var t1 = time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

func i64(v int64) *int64 { return &v }
func i32(v int32) *int32 { return &v }

// key returns a distinct valid UUID per n.
func key(n int) string {
	const hexdigits = "0123456789abcdef"
	buf := []byte("000000000000")
	for i := len(buf) - 1; n > 0 && i >= 0; i-- {
		buf[i] = hexdigits[n&0xf]
		n >>= 4
	}
	return "00000000-0000-4000-8000-" + string(buf)
}

// insert runs ledger.Insert inside a real transaction and commits it; an error
// rolls back, exactly as db.WithTx would.
func insert(t *testing.T, pool *pgxpool.Pool, req ledger.InsertRequest) (*ledger.InsertResult, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := ledger.Insert(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return res, nil
}

// seedOp registers one regular operation and returns its id.
func seedOp(t *testing.T, pool *pgxpool.Pool, n int, owner int64, account string, amount int64) int64 {
	t.Helper()
	res, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(n),
		Operations:     []ledger.InsertOp{{OwnerID: owner, ExternalID: account, Amount: amount, EffectiveAt: t1}},
	})
	if err != nil {
		t.Fatalf("seed op: %v", err)
	}
	return res.Operations[0].ID
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

type opRow struct {
	AccountID        int64
	Amount           int64
	EffectiveAt      time.Time
	Status           string
	EditOf           *int64
	ExpectedRevision *int32
	ReversalOf       *int64
	TxID             *int64
	HasKey           bool
	Revision         int32
}

func readOp(t *testing.T, pool *pgxpool.Pool, id int64) opRow {
	t.Helper()
	var r opRow
	err := pool.QueryRow(context.Background(), `SELECT account_id, amount, effective_at, status, edit_of, expected_revision,
		reversal_of, transaction_id, idempotency_key IS NOT NULL, revision FROM operations WHERE id = $1`, id).
		Scan(&r.AccountID, &r.Amount, &r.EffectiveAt, &r.Status, &r.EditOf, &r.ExpectedRevision, &r.ReversalOf, &r.TxID, &r.HasKey, &r.Revision)
	if err != nil {
		t.Fatalf("read op %d: %v", id, err)
	}
	return r
}

func accountExt(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var ext string
	if err := pool.QueryRow(context.Background(), `SELECT external_id FROM accounts WHERE id = $1`, id).Scan(&ext); err != nil {
		t.Fatalf("read account %d: %v", id, err)
	}
	return ext
}

// Happy path the sentinel tests are measured against: a single edit registers a
// PENDING row carrying the full proposed state, resolved from the target, with
// the key on the row, EditOf in the outcome and the doorbell rung.
func TestInsertEditSingleRegistersPending(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	target := seedOp(t, pool, 1, 7, "cash", -1500)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN work_available"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	res, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &target, Amount: -1200, ExpectedRevision: i32(1)}},
	})
	if err != nil {
		t.Fatalf("insert edit: %v", err)
	}
	if res.Replayed || res.TransactionID != nil || len(res.Operations) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	out := res.Operations[0]
	if out.Status != string(model.OpPending) || out.EditOf == nil || *out.EditOf != target {
		t.Fatalf("unexpected outcome: %+v", out)
	}

	row := readOp(t, pool, out.ID)
	targetRow := readOp(t, pool, target)
	if row.Status != "PENDING" || row.EditOf == nil || *row.EditOf != target || row.ExpectedRevision == nil || *row.ExpectedRevision != 1 {
		t.Fatalf("edit row not registered as expected: %+v", row)
	}
	// Full proposed state: amount from the item, account and effective_at from the target.
	if row.Amount != -1200 || row.AccountID != targetRow.AccountID || !row.EffectiveAt.Equal(t1) {
		t.Fatalf("edit row state not resolved from target: %+v (target %+v)", row, targetRow)
	}
	if !row.HasKey || row.TxID != nil || row.ReversalOf != nil || row.Revision != 1 {
		t.Fatalf("edit row metadata wrong: %+v", row)
	}
	// The target is untouched at submission.
	if targetRow.Amount != -1500 || targetRow.Status != "PENDING" || targetRow.Revision != 1 {
		t.Fatalf("target mutated by registration: %+v", targetRow)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if n, err := conn.Conn().WaitForNotification(waitCtx); err != nil || n.Channel != "work_available" {
		t.Fatalf("expected doorbell, got %v / %v", n, err)
	}
}

// A changed account is resolved through the on-demand upsert, and a changed
// effective_at is carried verbatim.
func TestInsertEditChangesAccountAndInstant(t *testing.T) {
	pool := dbtest.NewSchema(t)
	target := seedOp(t, pool, 1, 7, "cash", -1500)
	moved := time.Date(2026, 9, 1, 8, 0, 0, 0, time.FixedZone("BRT", -3*3600))

	res, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &target, ExternalID: "wallet", EffectiveAt: moved}},
	})
	if err != nil {
		t.Fatalf("insert edit: %v", err)
	}
	row := readOp(t, pool, res.Operations[0].ID)
	if accountExt(t, pool, row.AccountID) != "wallet" {
		t.Fatalf("edit row account = %d, want the upserted wallet", row.AccountID)
	}
	if row.Amount != -1500 || !row.EffectiveAt.Equal(moved) {
		t.Fatalf("edit row state: %+v", row)
	}
	if count(t, pool, "accounts") != 2 {
		t.Fatalf("expected wallet to be created on demand")
	}
}

// IT-003: structural sentinels — nothing changed, reversal_of on an edit,
// expected_revision < 1 — fire before any write.
func TestInsertEditStructuralSentinels(t *testing.T) {
	pool := dbtest.NewSchema(t)
	target := seedOp(t, pool, 1, 7, "cash", -1500)

	cases := []struct {
		name string
		op   ledger.InsertOp
		want error
	}{
		{"changes nothing", ledger.InsertOp{OwnerID: 7, EditOf: &target}, ledger.ErrEditChangesNothing},
		{"reversal on edit", ledger.InsertOp{OwnerID: 7, EditOf: &target, Amount: -1, ReversalOf: i64(target)}, ledger.ErrEditWithReversal},
		{"expected_revision 0", ledger.InsertOp{OwnerID: 7, EditOf: &target, Amount: -1, ExpectedRevision: i32(0)}, ledger.ErrInvalidExpectedRevision},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := insert(t, pool, ledger.InsertRequest{IdempotencyKey: key(10 + i), Operations: []ledger.InsertOp{tc.op}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if n := count(t, pool, "operations"); n != 1 {
		t.Fatalf("operations = %d, want only the seeded target", n)
	}
}

// IT-004: unknown and foreign targets are the same "not found"; no rows.
func TestInsertEditTargetNotFoundOwnerScoped(t *testing.T) {
	pool := dbtest.NewSchema(t)
	target := seedOp(t, pool, 1, 7, "cash", -1500)

	_, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations:     []ledger.InsertOp{{OwnerID: 8, EditOf: &target, Amount: -1200}},
	})
	if !errors.Is(err, ledger.ErrEditTargetNotFound) {
		t.Fatalf("foreign target: err = %v, want ErrEditTargetNotFound", err)
	}
	// The message names only the id: foreign and unknown are indistinguishable
	// apart from the id itself.
	if want := fmt.Sprintf("%v: %d", ledger.ErrEditTargetNotFound, target); err.Error() != want {
		t.Fatalf("foreign target message = %q, want %q", err, want)
	}

	unknown := target + 1000
	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &unknown, Amount: -1200}},
	})
	if !errors.Is(err, ledger.ErrEditTargetNotFound) {
		t.Fatalf("unknown target: err = %v, want ErrEditTargetNotFound", err)
	}
	if want := fmt.Sprintf("%v: %d", ledger.ErrEditTargetNotFound, unknown); err.Error() != want {
		t.Fatalf("unknown target message = %q, want %q", err, want)
	}
	if n := count(t, pool, "operations"); n != 1 {
		t.Fatalf("operations = %d, want 1", n)
	}
	if n := count(t, pool, "accounts"); n != 1 {
		t.Fatalf("accounts = %d, want 1 (no upsert for the foreign owner)", n)
	}
}

// IT-005: an edit registration (here already APPLIED) is not an editable target.
func TestInsertEditOfEditRowRejected(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	target := seedOp(t, pool, 1, 7, "cash", -1500)
	res, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &target, Amount: -1200}},
	})
	if err != nil {
		t.Fatalf("register edit: %v", err)
	}
	editID := res.Operations[0].ID
	// Stand in for the leader's decision (task 03): the edit row is APPLIED.
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'APPLIED', confirmed_at = now() WHERE id = $1`, editID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}

	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &editID, Amount: -1000}},
	})
	if !errors.Is(err, ledger.ErrEditTargetNotOperation) {
		t.Fatalf("err = %v, want ErrEditTargetNotOperation", err)
	}
	if n := count(t, pool, "operations"); n != 2 {
		t.Fatalf("operations = %d, want 2", n)
	}
}

// IT-006: group-level sentinels leave no transactions row behind.
func TestInsertEditGroupSentinelsWriteNothing(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)

	_, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, EditOf: &a, Amount: -1200},
			{OwnerID: 7, ExternalID: "cash", Amount: 300, EffectiveAt: t1},
			{OwnerID: 7, EditOf: &a, EffectiveAt: t1.Add(time.Hour)},
		},
	})
	if !errors.Is(err, ledger.ErrDuplicateEditTarget) {
		t.Fatalf("duplicate: err = %v, want ErrDuplicateEditTarget", err)
	}

	unknown := b + 1000
	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(4),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, EditOf: &a, Amount: -1200},
			{OwnerID: 7, ExternalID: "wallet", Amount: 300, EffectiveAt: t1},
			{OwnerID: 7, EditOf: &unknown, Amount: -100},
		},
	})
	if !errors.Is(err, ledger.ErrEditTargetNotFound) {
		t.Fatalf("unknown in group: err = %v, want ErrEditTargetNotFound", err)
	}

	if n := count(t, pool, "transactions"); n != 0 {
		t.Fatalf("transactions = %d, want 0", n)
	}
	if n := count(t, pool, "operations"); n != 2 {
		t.Fatalf("operations = %d, want 2 (seeds only)", n)
	}
	if n := count(t, pool, "accounts"); n != 1 {
		t.Fatalf("accounts = %d, want 1 (wallet not upserted before the failed lookup)", n)
	}
}

// Success criterion: a group mixing edits and new items registers atomically
// with one transactions row; an edit of a PENDING target registers (no status
// check at submission).
func TestInsertEditGroupMixedRegistersAtomically(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)

	res, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, EditOf: &a, Amount: -1200},
			{OwnerID: 7, EditOf: &b, ExternalID: "wallet", ExpectedRevision: i32(1)},
			{OwnerID: 7, ExternalID: "cash", Amount: 300, EffectiveAt: t1},
		},
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if res.TransactionID == nil || res.TransactionStatus != string(model.TxPending) || len(res.Operations) != 3 {
		t.Fatalf("unexpected group result: %+v", res)
	}
	wantEditOf := []*int64{&a, &b, nil}
	for i, o := range res.Operations {
		if o.Status != string(model.OpPending) {
			t.Fatalf("leg %d status %q", i, o.Status)
		}
		switch {
		case wantEditOf[i] == nil && o.EditOf != nil:
			t.Fatalf("leg %d: unexpected EditOf %d", i, *o.EditOf)
		case wantEditOf[i] != nil && (o.EditOf == nil || *o.EditOf != *wantEditOf[i]):
			t.Fatalf("leg %d: EditOf = %v, want %d", i, o.EditOf, *wantEditOf[i])
		}
	}

	r0 := readOp(t, pool, res.Operations[0].ID)
	r1 := readOp(t, pool, res.Operations[1].ID)
	r2 := readOp(t, pool, res.Operations[2].ID)
	for i, r := range []opRow{r0, r1, r2} {
		if r.TxID == nil || *r.TxID != *res.TransactionID || r.HasKey {
			t.Fatalf("leg %d not attached to the group: %+v", i, r)
		}
	}
	if r0.Amount != -1200 || accountExt(t, pool, r0.AccountID) != "cash" || !r0.EffectiveAt.Equal(t1) {
		t.Fatalf("leg 0 state: %+v", r0)
	}
	if r1.Amount != -300 || accountExt(t, pool, r1.AccountID) != "wallet" || r1.ExpectedRevision == nil || *r1.ExpectedRevision != 1 {
		t.Fatalf("leg 1 state: %+v", r1)
	}
	if r2.EditOf != nil || r2.Amount != 300 {
		t.Fatalf("leg 2 state: %+v", r2)
	}
	var opCount int
	if err := pool.QueryRow(context.Background(), `SELECT op_count FROM transactions WHERE id = $1`, *res.TransactionID).Scan(&opCount); err != nil || opCount != 3 {
		t.Fatalf("op_count = %d (%v), want 3", opCount, err)
	}
}

// IT-007: same key + same edit item twice → replay of the original id with
// EditOf set, no new row, for singles and groups.
func TestInsertEditIdempotentReplay(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)

	single := ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &a, Amount: -1200}},
	}
	first, err := insert(t, pool, single)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := insert(t, pool, single)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Operations[0].ID != first.Operations[0].ID {
		t.Fatalf("single replay mismatch: %+v vs %+v", second, first)
	}
	if second.Operations[0].EditOf == nil || *second.Operations[0].EditOf != a || second.Operations[0].Status != string(model.OpPending) {
		t.Fatalf("single replay outcome: %+v", second.Operations[0])
	}

	group := ledger.InsertRequest{
		IdempotencyKey: key(4),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, EditOf: &b, EffectiveAt: t1.Add(time.Hour)},
			{OwnerID: 7, ExternalID: "cash", Amount: 5, EffectiveAt: t1},
		},
	}
	gFirst, err := insert(t, pool, group)
	if err != nil {
		t.Fatalf("group first: %v", err)
	}
	gSecond, err := insert(t, pool, group)
	if err != nil {
		t.Fatalf("group replay: %v", err)
	}
	if !gSecond.Replayed || *gSecond.TransactionID != *gFirst.TransactionID || len(gSecond.Operations) != 2 {
		t.Fatalf("group replay mismatch: %+v vs %+v", gSecond, gFirst)
	}
	if gSecond.Operations[0].EditOf == nil || *gSecond.Operations[0].EditOf != b || gSecond.Operations[1].EditOf != nil {
		t.Fatalf("group replay EditOf: %+v", gSecond.Operations)
	}

	if n := count(t, pool, "operations"); n != 5 {
		t.Fatalf("operations = %d, want 5 (2 seeds + 1 single edit + 2 legs)", n)
	}
	if n := count(t, pool, "transactions"); n != 1 {
		t.Fatalf("transactions = %d, want 1", n)
	}
}

// IT-008: same key + a different edit item → ErrPayloadConflict, including the
// as-sent case where the resolved state would be identical.
func TestInsertEditSameKeyDifferentItemConflicts(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)

	base := ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &a, EffectiveAt: t1.Add(time.Hour)}},
	}
	if _, err := insert(t, pool, base); err != nil {
		t.Fatalf("first: %v", err)
	}

	variants := map[string]ledger.InsertOp{
		"different amount":       {OwnerID: 7, EditOf: &a, EffectiveAt: t1.Add(time.Hour), Amount: -1},
		"amount sent = current":  {OwnerID: 7, EditOf: &a, EffectiveAt: t1.Add(time.Hour), Amount: -1500},
		"account sent = current": {OwnerID: 7, EditOf: &a, EffectiveAt: t1.Add(time.Hour), ExternalID: "cash"},
		"guard added":            {OwnerID: 7, EditOf: &a, EffectiveAt: t1.Add(time.Hour), ExpectedRevision: i32(1)},
		"regular item, same key": {OwnerID: 7, ExternalID: "cash", Amount: -1500, EffectiveAt: t1.Add(time.Hour)},
	}
	for name, op := range variants {
		t.Run(name, func(t *testing.T) {
			req := base
			req.Operations = []ledger.InsertOp{op}
			if _, err := insert(t, pool, req); !errors.Is(err, ledger.ErrPayloadConflict) {
				t.Fatalf("err = %v, want ErrPayloadConflict", err)
			}
		})
	}
	if n := count(t, pool, "operations"); n != 2 {
		t.Fatalf("operations = %d, want 2", n)
	}
}

// IT-009: a key reused by another owner conflicts (owner is part of the hash).
// The other owner edits its own operation with an equivalent item; reusing the
// key against the first owner's target is refused earlier by owner scoping.
func TestInsertEditKeyReusedByOtherOwnerConflicts(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 8, "cash", -1500)

	if _, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &a, Amount: -1200}},
	}); err != nil {
		t.Fatalf("owner 7: %v", err)
	}

	_, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []ledger.InsertOp{{OwnerID: 8, EditOf: &b, Amount: -1200}},
	})
	if !errors.Is(err, ledger.ErrPayloadConflict) {
		t.Fatalf("owner 8 own target: err = %v, want ErrPayloadConflict", err)
	}

	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations:     []ledger.InsertOp{{OwnerID: 8, EditOf: &a, Amount: -1200}},
	})
	if !errors.Is(err, ledger.ErrEditTargetNotFound) {
		t.Fatalf("owner 8 foreign target: err = %v, want ErrEditTargetNotFound", err)
	}
	if n := count(t, pool, "operations"); n != 3 {
		t.Fatalf("operations = %d, want 3", n)
	}
}

// allow_edits = false refuses new edit registrations (singles and groups) but
// leaves plain inserts untouched; the policy is read per request.
func TestInsertEditsDisabledByConfig(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	if _, err := pool.Exec(ctx, `UPDATE config SET allow_edits = false`); err != nil {
		t.Fatalf("disable edits: %v", err)
	}

	_, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &a, Amount: -1200}},
	})
	if !errors.Is(err, ledger.ErrEditsDisabled) {
		t.Fatalf("single: err = %v, want ErrEditsDisabled", err)
	}
	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, ExternalID: "cash", Amount: 5, EffectiveAt: t1},
			{OwnerID: 7, EditOf: &a, Amount: -1200},
		},
	})
	if !errors.Is(err, ledger.ErrEditsDisabled) {
		t.Fatalf("group: err = %v, want ErrEditsDisabled", err)
	}
	if _, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(4),
		Operations:     []ledger.InsertOp{{OwnerID: 7, ExternalID: "cash", Amount: 5, EffectiveAt: t1}},
	}); err != nil {
		t.Fatalf("plain insert with edits disabled: %v", err)
	}
	if n := count(t, pool, "operations"); n != 2 {
		t.Fatalf("operations = %d, want 2", n)
	}

	// Hot: flipping the flag back takes effect on the next request.
	if _, err := pool.Exec(ctx, `UPDATE config SET allow_edits = true`); err != nil {
		t.Fatalf("enable edits: %v", err)
	}
	if _, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(5),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &a, Amount: -1200}},
	}); err != nil {
		t.Fatalf("edit after re-enable: %v", err)
	}
}
