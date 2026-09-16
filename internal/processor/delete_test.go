package processor

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/model"
)

// Unit tests for the pure pieces of the single-delete path (ADR-0011): the one
// virtual leg, decision-time values, netting against the §6 check, the
// target-state rules with a DELETED target, and the source-level guard that
// nothing is ever physically deleted. The DB-backed path is covered by
// delete_itest_test.go.

// UT-003: expandDelete returns exactly one virtual leg — −amount on the target's
// current account at its current instant.
func TestExpandDeleteOneLeg(t *testing.T) {
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	target := targetState{AccountID: 7, Amount: -1500, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}

	legs := expandDelete(target)
	if len(legs) != 1 {
		t.Fatalf("expandDelete returned %d legs, want exactly 1", len(legs))
	}
	if legs[0] != (virtualLeg{AccountID: 7, Amount: 1500, EffectiveAt: t1}) {
		t.Fatalf("virtual leg = %+v, want {7, +1500, t1}", legs[0])
	}
}

// UT-004: expandDelete uses the target's *current* row (revision 2, after an
// applied edit), never the delete row's informational copy of the values at
// submission — so a delete registered before an edit still removes the edited
// amount from the edited account when decided after it.
func TestExpandDeleteUsesCurrentValues(t *testing.T) {
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	// The delete row copied {cash 1, -1500, t1} at submission; the target has since
	// been edited to {wallet 2, -1200, t3} at revision 2.
	del := pendingOp{ID: 99, AccountID: 1, Amount: -1500, EffectiveAt: t1, EditOf: ptr64(41), IsDelete: true}
	current := targetState{AccountID: 2, Amount: -1200, EffectiveAt: t3, Status: "CONFIRMED", Revision: 2}

	legs := expandDelete(current)
	if len(legs) != 1 || legs[0].AccountID != 2 || legs[0].Amount != 1200 || !legs[0].EffectiveAt.Equal(t3) {
		t.Fatalf("virtual leg = %+v, want the current row {wallet, +1200, t3}, not the copy %+v", legs, del)
	}
	if legs[0].Amount == -del.Amount && legs[0].AccountID == del.AccountID {
		t.Fatalf("expandDelete used the delete row's informational copy")
	}
}

// UT-010: netting a delete leg against limits. Account min 0, balance 1000,
// target +1500 → removing it lands at −500: violates reports min short by 500.
// At balance 1500 the removal lands on 0 and passes.
func TestNetDeleteLegAgainstLimits(t *testing.T) {
	const cash = int64(7)
	minZero := int64(0)
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	target := targetState{AccountID: cash, Amount: 1500, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}

	ids, net := netLegs(expandDelete(target))
	if len(ids) != 1 || ids[0] != cash || net[cash] != -1500 {
		t.Fatalf("net = ids %v %v, want cash −1500", ids, net)
	}
	side, shortfall, bad := violates(1000+net[cash], &minZero, nil)
	if !bad || side != model.LimitMin || shortfall != 500 {
		t.Fatalf("violates(1000-1500, min 0) = (%q, %d, %v), want (min, 500, true)", side, shortfall, bad)
	}
	if _, _, bad := violates(1500+net[cash], &minZero, nil); bad {
		t.Fatalf("violates(1500-1500, min 0) reported a violation")
	}
}

// UT-011: mixed-group expansion and netting with a delete leg. Legs
// [delete 41: cash −1500 as read, edit 42: cash +800 → +900 at t3, new: cash
// +300] expand to 1 + 2 + 1 virtual legs in leg order — +1500, −800, +900, +300
// — netting to +1900 on cash. A delete of a target that has since moved to
// wallet (its current row, revision 2) lands its one leg on wallet, so a group
// of two deletes across two accounts nets each account separately, ids
// ascending.
func TestExpandGroupWithDeleteLegs(t *testing.T) {
	const cash, wallet = int64(1), int64(2)
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)

	legs := []groupLeg{
		{ID: 64, AccountID: cash, Amount: -1500, EffectiveAt: t1, EditOf: ptr64(41), IsDelete: true}, // informational copy
		{ID: 65, AccountID: cash, Amount: 900, EffectiveAt: t3, EditOf: ptr64(42)},
		{ID: 66, AccountID: cash, Amount: 300, EffectiveAt: t2},
	}
	edits := []groupEdit{
		{leg: legs[0], target: targetState{AccountID: cash, Amount: -1500, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}},
		{leg: legs[1], target: targetState{AccountID: cash, Amount: 800, EffectiveAt: t2, Status: "CONFIRMED", Revision: 1}},
	}
	virtual := expandGroup(legs, edits)
	want := []virtualLeg{
		{AccountID: cash, Amount: 1500, EffectiveAt: t1},
		{AccountID: cash, Amount: -800, EffectiveAt: t2},
		{AccountID: cash, Amount: 900, EffectiveAt: t3},
		{AccountID: cash, Amount: 300, EffectiveAt: t2},
	}
	if len(virtual) != len(want) {
		t.Fatalf("virtual legs = %+v, want %+v (1 + 2 + 1)", virtual, want)
	}
	for i := range want {
		if virtual[i] != want[i] {
			t.Fatalf("virtual leg %d = %+v, want %+v", i, virtual[i], want[i])
		}
	}
	ids, net := netLegs(virtual)
	if len(ids) != 1 || ids[0] != cash || net[cash] != 1900 {
		t.Fatalf("mixed group: ids=%v net=%v, want cash +1900", ids, net)
	}

	// Two deletes: 41 as it stands on cash, 44 edited onto wallet at revision 2
	// — each leg lands on the target's *current* account, never the copy's.
	dels := []groupLeg{
		{ID: 70, AccountID: cash, Amount: -1500, EffectiveAt: t1, EditOf: ptr64(41), IsDelete: true},
		{ID: 71, AccountID: cash, Amount: -200, EffectiveAt: t1, EditOf: ptr64(44), IsDelete: true, ExpectedRevision: ptr32(2)},
	}
	delEdits := []groupEdit{
		{leg: dels[0], target: targetState{AccountID: cash, Amount: -1500, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}},
		{leg: dels[1], target: targetState{AccountID: wallet, Amount: -250, EffectiveAt: t3, Status: "CONFIRMED", Revision: 2}},
	}
	virtual = expandGroup(dels, delEdits)
	if len(virtual) != 2 || virtual[0] != (virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: t1}) || virtual[1] != (virtualLeg{AccountID: wallet, Amount: 250, EffectiveAt: t3}) {
		t.Fatalf("delete group virtual legs = %+v, want [{cash +1500 t1} {wallet +250 t3}]", virtual)
	}
	ids, net = netLegs(virtual)
	if len(ids) != 2 || ids[0] != cash || ids[1] != wallet || net[cash] != 1500 || net[wallet] != 250 {
		t.Fatalf("delete group: ids=%v net=%v, want cash +1500 / wallet +250", ids, net)
	}
	// The leg predicates: a delete leg is edit-class (the Guard 2 split) and a
	// delete; a plain edit leg is edit-class only; a regular leg is neither.
	if !dels[0].isEdit() || !dels[0].isDelete() || !legs[1].isEdit() || legs[1].isDelete() || legs[2].isEdit() || legs[2].isDelete() {
		t.Fatalf("leg predicates: delete=%v/%v edit=%v/%v regular=%v/%v", dels[0].isEdit(), dels[0].isDelete(), legs[1].isEdit(), legs[1].isDelete(), legs[2].isEdit(), legs[2].isDelete())
	}
}

// UT-013: checkTarget with a DELETED target rejects with TARGET_NOT_EDITABLE{41}
// for an edit and a delete alike (the same function decides both, the
// registration kind does not enter), with or without an expected_revision; a
// PENDING target defers either kind. UT-012's CONFIRMED/STALE cases live in
// TestCheckTarget.
func TestCheckTargetDeleted(t *testing.T) {
	deleted := targetState{Status: string(model.OpDeleted), Revision: 2}
	for _, expected := range []*int32{nil, ptr32(2), ptr32(1)} {
		rej, err := checkTarget(41, deleted, expected)
		if err != nil || rej == nil {
			t.Fatalf("DELETED target (expected=%v): rej=%v err=%v, want a rejection", expected, rej, err)
		}
		if rej.Code != model.ReasonTargetNotEditable || rej.OperationID == nil || *rej.OperationID != 41 {
			t.Fatalf("DELETED target: rejection = %+v, want TARGET_NOT_EDITABLE{41}", *rej)
		}
		if rej.ExpectedRevision != nil || rej.ActualRevision != nil || rej.Account != "" {
			t.Fatalf("TARGET_NOT_EDITABLE must carry only the operation id: %+v", *rej)
		}
	}
	if _, err := checkTarget(41, targetState{Status: string(model.OpPending), Revision: 1}, nil); !errors.Is(err, errDeferred) {
		t.Fatalf("PENDING target: err=%v, want errDeferred", err)
	}
	// The dispatcher's kind predicate: a delete row is EditOf + IsDelete; a plain
	// edit or a regular row is not a delete.
	if !(pendingOp{EditOf: ptr64(41), IsDelete: true}).isDelete() {
		t.Fatalf("EditOf+IsDelete must be a delete")
	}
	if (pendingOp{EditOf: ptr64(41)}).isDelete() || (pendingOp{IsDelete: true}).isDelete() {
		t.Fatalf("a plain edit or a marker without edit_of must not be a delete")
	}
	if (pendingOp{EditOf: ptr64(41), IsDelete: true}).decisionKind() != "delete" || (pendingOp{EditOf: ptr64(41)}).decisionKind() != "edit" {
		t.Fatalf("decisionKind must follow the row's kind")
	}
}

// UT-014: a delete leg never goes through coalesceSnapshotLegs — the apply path
// takes expandDelete's single leg straight to one snapshot.Apply. Statically: the
// delete source has exactly one snapshot.Apply call and no coalesce call; the
// row-count half is IT-011.
func TestDeleteSnapshotSingleApply(t *testing.T) {
	src, err := os.ReadFile("delete.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if n := strings.Count(text, "snapshot.Apply("); n != 1 {
		t.Fatalf("delete.go calls snapshot.Apply %d times, want exactly 1", n)
	}
	if strings.Contains(text, "coalesceSnapshotLegs(") {
		t.Fatalf("delete.go coalesces snapshot legs; a delete has one leg")
	}
	if strings.Contains(text, "insertRevision") || strings.Contains(text, "guardRevisionCAS") {
		t.Fatalf("delete.go appends a revision or bumps revision; a delete does neither (ADR-0011)")
	}
}

// UT-030 (Safety Invariants 2 and 3, static half): scanning every non-test Go
// file of the module — internal/, the root package and simtest/ — no string
// literal issues a physical DELETE FROM operations or operation_revisions, and
// no literal outside internal/processor writes deleted_by, deleted_at or
// status = 'DELETED'. Every string literal is inspected, so a new const cannot
// slip past the check.
func TestNoPhysicalDeleteAndDeletedWritersConfined(t *testing.T) {
	// Word-bounded: `is_delete FROM operations` in a SELECT is not a DELETE.
	physicalDelete := regexp.MustCompile(`\bDELETE\s+FROM\s+OPERATION(S|_REVISIONS)\b`)
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name != "." && name != ".." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		inProcessor := filepath.Dir(rel) == filepath.Join("internal", "processor")
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			up := strings.ToUpper(strings.Join(strings.Fields(text), " "))
			if physicalDelete.MatchString(up) {
				t.Errorf("%s: string literal at %s physically deletes ledger rows:\n%s", rel, fset.Position(lit.Pos()), text)
			}
			if inProcessor {
				return true
			}
			isWrite := strings.Contains(up, "UPDATE ") || strings.Contains(up, "INSERT ")
			if isWrite && (strings.Contains(up, "DELETED_BY") || strings.Contains(up, "DELETED_AT") || strings.Contains(up, "STATUS = 'DELETED'")) {
				t.Errorf("%s: string literal at %s writes deletion columns outside internal/processor:\n%s", rel, fset.Position(lit.Pos()), text)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Go files under %s; the walk is not covering the module", scanned, root)
	}
}
