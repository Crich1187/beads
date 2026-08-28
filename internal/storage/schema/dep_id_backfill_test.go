package schema

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/storage/depid"
)

// The re-key scan reads the typed target columns rather than COALESCEing them,
// because the duplicate-edge merge has to know which column held the target
// (gastownhall/beads#5268).
const depScanPattern = `SELECT id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external FROM dependencies`

func depScanRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "issue_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"})
}

// expectDepScan primes the id-column probe and the table scan. mock is in
// sqlmock's default ordered mode, so the Exec expectations each test adds after
// this also pin the statement ORDER — which is load-bearing here: a DELETE that
// runs after its UPDATE re-creates the duplicate-primary-key abort.
func expectDepScan(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM INFORMATION_SCHEMA\.COLUMNS`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(depScanPattern)).WillReturnRows(rows)
}

func expectDepDelete(mock sqlmock.Sqlmock, id string) {
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM dependencies WHERE id = ?")).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectDepUpdate(mock sqlmock.Sqlmock, newID, oldID string) {
	mock.ExpectExec(regexp.QuoteMeta("UPDATE dependencies SET id = ? WHERE id = ?")).
		WithArgs(newID, oldID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestRekeyDependencyTableRewritesOnlyDivergentRows verifies the backfill that
// converges existing rows after the #4259 fix: it re-keys a row whose id is not
// the deterministic value, and leaves an already-deterministic row untouched.
func TestRekeyDependencyTableRewritesOnlyDivergentRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Two rows: one carrying a legacy random id, one already deterministic.
	randomRow := "random-uuid-aaaa"
	deterministicRow := depid.New("c", "d")
	expectDepScan(mock, depScanRows().
		AddRow(randomRow, "a", "b", nil, nil).
		AddRow(deterministicRow, "c", "d", nil, nil))

	// Only the divergent row is re-keyed, to its deterministic value.
	expectDepUpdate(mock, depid.New("a", "b"), randomRow)

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true when a row was re-keyed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableSkipsMissingTable verifies the backfill no-ops cleanly
// when the table/id column is absent (older or partial schema).
func TestRekeyDependencyTableSkipsMissingTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM INFORMATION_SCHEMA\.COLUMNS`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false when the id column is absent")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableIdempotent verifies that when every row already carries
// its deterministic id, no UPDATE is issued (so re-running is a cheap no-op).
func TestRekeyDependencyTableIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectDepScan(mock, depScanRows().AddRow(depid.New("a", "b"), "a", "b", nil, nil))
	// No ExpectExec: zero writes expected.

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false when all rows already deterministic")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableMergesFreshDuplicatePair is the reported abort
// (gastownhall/beads#5268) in its simplest form: the same logical edge recorded
// once as an issue ref and once as an external ref, both still carrying random
// ids. Both derive the same deterministic id, which is what made the old
// row-at-a-time rewrite die on the second UPDATE. The external twin is dropped
// and the issue row is re-keyed.
func TestRekeyDependencyTableMergesFreshDuplicatePair(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	want := depid.New("hq-cv-vvnmq", "ops-w05")
	expectDepScan(mock, depScanRows().
		AddRow("random-issue-row", "hq-cv-vvnmq", "ops-w05", nil, nil).
		AddRow("random-external-row", "hq-cv-vvnmq", nil, nil, "ops-w05"))

	expectDepDelete(mock, "random-external-row")
	expectDepUpdate(mock, want, "random-issue-row")

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true when a duplicate pair was merged")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableMergesHalfRekeyedPairLoserHoldsID covers the database
// a pre-1.3.0 binary left half-re-keyed with the LOSER moved first: the external
// twin already sits on the deterministic id and the surviving issue row does
// not. The twin still loses — the survivor rule is the typed column, not who got
// there first — so the DELETE has to run before the UPDATE that reclaims the id.
func TestRekeyDependencyTableMergesHalfRekeyedPairLoserHoldsID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	want := depid.New("a", "t")
	expectDepScan(mock, depScanRows().
		AddRow("random-issue-row", "a", "t", nil, nil).
		AddRow(want, "a", nil, nil, "t"))

	expectDepDelete(mock, want)
	expectDepUpdate(mock, want, "random-issue-row")

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true when a half-re-keyed pair was merged")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableMergesHalfRekeyedPairSurvivorHoldsID is the other
// half-re-keyed orientation: the surviving issue row already holds the
// deterministic id, so only the duplicate needs dropping and no UPDATE is
// planned at all.
func TestRekeyDependencyTableMergesHalfRekeyedPairSurvivorHoldsID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectDepScan(mock, depScanRows().
		AddRow(depid.New("a", "t"), "a", "t", nil, nil).
		AddRow("random-external-row", "a", nil, nil, "t"))

	expectDepDelete(mock, "random-external-row")

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true when a duplicate row was dropped")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableMergesTripleDuplicate exercises the full survivor
// priority: the same target recorded in all three typed columns collapses to the
// issue ref, with the wisp and external rows dropped.
func TestRekeyDependencyTableMergesTripleDuplicate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectDepScan(mock, depScanRows().
		AddRow("row-issue", "a", "t", nil, nil).
		AddRow("row-wisp", "a", nil, "t", nil).
		AddRow("row-external", "a", nil, nil, "t"))

	// DELETEs are emitted in sorted id order, so "row-external" precedes
	// "row-wisp"; both precede the survivor's UPDATE.
	expectDepDelete(mock, "row-external")
	expectDepDelete(mock, "row-wisp")
	expectDepUpdate(mock, depid.New("a", "t"), "row-issue")

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true when a triple duplicate was merged")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableRefusesForeignIDCollision pins the corruption guard:
// when a row that is NOT a duplicate already occupies an id the plan wants to
// move another row onto, the pass names both rows and writes nothing. Merging
// them would silently discard a distinct edge, and re-keying into the collision
// is exactly the abort this change exists to remove.
func TestRekeyDependencyTableRefusesForeignIDCollision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// The id "a -> t" wants is squatted on by the edge "b -> u", which is a
	// different logical edge, so it is not a duplicate that can be merged.
	squatted := depid.New("a", "t")
	expectDepScan(mock, depScanRows().
		AddRow("random-row", "a", "t", nil, nil).
		AddRow(squatted, "b", "u", nil, nil))
	// No ExpectExec at all: validation runs before the first write.

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err == nil {
		t.Fatal("expected an error when a foreign row squats a planned id")
	}
	if wrote {
		t.Error("expected wrote=false when the plan was rejected before any write")
	}
	for _, want := range []string{"random-row", squatted, "bd doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableIdempotentAfterMerge is the steady state a merged
// table lands in: one row per edge, each already deterministic. A re-run plans
// nothing, which is what makes ignored migration 0026's forced pass free on an
// already-converged clone.
func TestRekeyDependencyTableIdempotentAfterMerge(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectDepScan(mock, depScanRows().
		AddRow(depid.New("a", "t"), "a", "t", nil, nil).
		AddRow(depid.New("a", "external:e1"), "a", nil, nil, "external:e1").
		AddRow(depid.New("b", "w1"), "b", nil, "w1", nil))
	// No ExpectExec: zero writes expected.

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false on a converged table")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRekeyDependencyTableLeavesTargetlessRows keeps the pre-existing contract
// for a row with no target at all (ck_dep_one_target should make it
// unreachable): it is not re-keyed, not deleted, and not guessed about — it is
// left for `bd doctor` to surface.
func TestRekeyDependencyTableLeavesTargetlessRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	expectDepScan(mock, depScanRows().AddRow("orphan-row", "a", nil, nil, nil))
	// No ExpectExec: zero writes expected.

	wrote, err := rekeyDependencyTable(context.Background(), db, "dependencies")
	if err != nil {
		t.Fatalf("rekeyDependencyTable: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false when the only row has no target")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
