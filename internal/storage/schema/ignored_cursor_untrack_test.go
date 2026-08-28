package schema

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/debug"
)

// headTables is a plausible HEAD table listing for the mocked world: a handful
// of versioned tables and, deliberately, none of the dolt_ignore'd ones. That
// IS the healthy shape — an ignored table that never got committed is absent
// from HEAD, which is the whole property #4356 is about.
var headTables = []string{"issues", "dependencies", "config", "schema_migrations"}

// expectHeadTableListing mocks the tracked-at-HEAD probe. qualifier is the
// identifier-quoted database name when a selector had to put the session on
// the database, empty when it was already there.
func expectHeadTableListing(mock sqlmock.Sqlmock, qualifier string, tables ...string) {
	query := "SHOW TABLES AS OF 'HEAD'"
	if qualifier != "" {
		query = "SHOW TABLES FROM " + qualifier + " AS OF 'HEAD'"
	}
	rows := sqlmock.NewRows([]string{"Tables_in_testdb"})
	for _, table := range tables {
		rows.AddRow(table)
	}
	mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(rows)
}

// expectIgnoredCursorUntracked mocks the #4356 gate on a healthy database: one
// round trip proves the cursor table is not at HEAD and nothing follows. This
// is the steady-state cost of the whole fix.
func expectIgnoredCursorUntracked(mock sqlmock.Sqlmock, qualifier string) {
	expectHeadTableListing(mock, qualifier, headTables...)
}

// expectIgnoredCursorTracked mocks the legacy shape: the cursor table IS at
// HEAD, so the gate goes on to ask whether an active ignore pattern covers it
// (an operator override recorded with ignored=0 vetoes the heal).
func expectIgnoredCursorTracked(mock sqlmock.Sqlmock, qualifier string, activelyIgnored bool) {
	expectHeadTableListing(mock, qualifier, append(append([]string(nil), headTables...), ignoredSource.cursorTable)...)
	doltIgnore := "dolt_ignore"
	if qualifier != "" {
		doltIgnore = qualifier + ".dolt_ignore"
	}
	matches := 0
	if activelyIgnored {
		matches = 1
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM " + doltIgnore + " WHERE ignored = 1 AND ? LIKE pattern")).
		WithArgs(ignoredSource.cursorTable).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(matches))
}

// expectIgnoredCursorHealNoop mocks the whole open-time heal on a healthy
// database, as MigrateUp calls it: the HEAD probe says untracked, and the
// scratch-table probe says there is no interrupted run to resume. Two reads,
// no writes — the property that keeps this safe to run on a fenced client.
func expectIgnoredCursorHealNoop(mock sqlmock.Sqlmock) {
	expectIgnoredCursorUntracked(mock, "")
	expectCursorProbe(mock, ignoredCursorUntrackTempTable, false)
}

// TestHealSkipsHealthyDatabaseInTwoReads is the hot-path contract. Every bd
// invocation that opens a writable store pays this, so it must be two reads
// and nothing else — no write, no DDL, no commit. sqlmock in ordered mode
// fails on any unexpected statement, which is exactly the fence a
// SELECT/DML-only wire client presents.
func TestHealSkipsHealthyDatabaseInTwoReads(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorHealNoop(mock)

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v, want nil", err)
	}
	if healed {
		t.Fatal("healTrackedIgnoredCursorTable() reported a heal on a healthy database")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// An operator who deliberately put the cursor table back on the versioned
// plane (a dolt_ignore row with ignored=0) has made a choice this heal must
// not overrule — seedDoltIgnorePatterns respects the same override. The gate
// stops at the dolt_ignore read: no backup, no drop, no commit.
func TestHealRespectsOperatorIgnoreOverride(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", false)

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v, want nil", err)
	}
	if healed {
		t.Fatal("healTrackedIgnoredCursorTable() untracked a table an operator had explicitly un-ignored")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The #5816 lesson: a heal that cannot run must never be the reason a database
// stops opening. A client with no read on the HEAD listing (or a transient
// failure) leaves the caller exactly where it was — open works, pull stays
// wedged — and says so where BD_DEBUG can see it.
func TestHealIsNonFatalWhenTheProbeFails(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta("SHOW TABLES AS OF 'HEAD'")).
		WillReturnError(errors.New("command denied to user 'fenced'@'%'"))

	var healed bool
	logged := captureStderr(t, func() {
		debug.SetVerbose(true)
		defer debug.SetVerbose(false)
		var err error
		healed, err = healTrackedIgnoredCursorTable(context.Background(), db)
		if err != nil {
			t.Fatalf("healTrackedIgnoredCursorTable() error = %v, want nil (a failed probe must not brick the open)", err)
		}
	})
	if healed {
		t.Fatal("healTrackedIgnoredCursorTable() reported a heal after its probe failed")
	}
	if !strings.Contains(logged, "command denied") {
		t.Fatalf("debug output = %q, want the discarded probe error reported", logged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// Same contract one step later: the backup is the last reversible step, so a
// client that can read but cannot CREATE stops there with nothing destroyed.
func TestHealIsNonFatalWhenTheBackupFails(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS " + ignoredCursorUntrackTempTable)).
		WillReturnError(errors.New("command denied to user 'fenced'@'%'"))

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v, want nil (a failed backup must not brick the open)", err)
	}
	if healed {
		t.Fatal("healTrackedIgnoredCursorTable() reported a heal after its backup failed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// Past the DROP the policy inverts: the pass must not continue without a
// cursor table, so the error is returned and the open fails. It must NOT be a
// *DirtyTablesError — the embedded lenient intents and MigrateUpWithLock's
// bootstrap heal both branch on that type, and a heal failure is neither a
// dirty working set nor something DOLT_RESET('--hard') should be pointed at.
func TestHealIsFatalAfterTheDropAndIsNotADirtyTablesError(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	expectIgnoredCursorBackup(mock, true)
	expectDoltStatusRows(mock)
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS " + ignoredSource.cursorTable)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("COMMIT")).
		WillReturnError(errors.New("connection reset"))

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err == nil {
		t.Fatal("healTrackedIgnoredCursorTable() error = nil after the drop failed to commit, want the failure returned")
	}
	if healed {
		t.Fatal("healTrackedIgnoredCursorTable() reported success on a failed untrack")
	}
	var dirtyErr *DirtyTablesError
	if errors.As(err, &dirtyErr) {
		t.Fatalf("heal failure satisfies errors.As(*DirtyTablesError): %v — lenient opens would swallow it and the bootstrap heal would hard-reset on it", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// expectIgnoredCursorBackup mocks the row-preservation half of Phase A.
func expectIgnoredCursorBackup(mock sqlmock.Sqlmock, hasContentHash bool) {
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS " + ignoredCursorUntrackTempTable)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectCursorProbe(mock, ignoredSource.cursorTable, true)
	if hasContentHash {
		expectContentHashColumnExists(mock)
	} else {
		mock.ExpectQuery(`SHOW COLUMNS FROM \w+ LIKE 'content_hash'`).
			WillReturnRows(sqlmock.NewRows([]string{"Field", "Type", "Null", "Key", "Default", "Extra"}))
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT IGNORE INTO " + ignoredCursorUntrackTempTable)).
		WillReturnResult(sqlmock.NewResult(0, 25))
}

// expectIgnoredCursorUntrackCommit mocks the irreversible half of Phase A.
func expectIgnoredCursorUntrackCommit(mock sqlmock.Sqlmock) {
	expectDoltStatusRows(mock)
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS " + ignoredSource.cursorTable)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("COMMIT")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_ADD('-f', ?)")).
		WithArgs(ignoredSource.cursorTable).
		WillReturnRows(sqlmock.NewRows([]string{"status"}))
	mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_COMMIT('--skip-empty', '-m', ?)")).
		WithArgs(ignoredCursorUntrackCommitMessage).
		WillReturnRows(sqlmock.NewRows([]string{"hash"}))
}

// expectIgnoredCursorRestore mocks Phase B, including the straggler sweep that
// only ever fires if a concurrent commit swept the scratch table into HEAD.
func expectIgnoredCursorRestore(mock sqlmock.Sqlmock, tempSweptIntoHead bool) {
	mock.ExpectExec("(?s)^CREATE TABLE IF NOT EXISTS " + ignoredSource.cursorTable).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectContentHashColumnExists(mock)
	mock.ExpectExec(regexp.QuoteMeta("INSERT IGNORE INTO " + ignoredSource.cursorTable)).
		WillReturnResult(sqlmock.NewResult(0, 25))
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS " + ignoredCursorUntrackTempTable)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if tempSweptIntoHead {
		expectHeadTableListing(mock, "", append(append([]string(nil), headTables...), ignoredCursorUntrackTempTable)...)
		mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_ADD(?)")).
			WithArgs(ignoredCursorUntrackTempTable).
			WillReturnRows(sqlmock.NewRows([]string{"status"}))
		mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_COMMIT('--skip-empty', '-m', ?)")).
			WithArgs(ignoredCursorTempSweepCommitMessage).
			WillReturnRows(sqlmock.NewRows([]string{"hash"}))
		return
	}
	expectHeadTableListing(mock, "", headTables...)
}

// TestHealUntracksAndRestoresInOrder pins the statement contract of the whole
// repair. The order is not cosmetic: the backup must precede the drop, the
// bare COMMIT must precede the staging call (on the sql-server path Dolt's
// procedures read a working set an open transaction has not published), the
// add must carry '-f' (a plain add of an ignored table is a silent no-op, so
// without it the drop never stages and the recreate nets the whole repair back
// out), and the commit must carry '--skip-empty' so a replay is not fatal.
func TestHealUntracksAndRestoresInOrder(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	expectIgnoredCursorBackup(mock, true)
	expectIgnoredCursorUntrackCommit(mock)
	expectIgnoredCursorRestore(mock, false)

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v", err)
	}
	if !healed {
		t.Fatal("healTrackedIgnoredCursorTable() = false on a legacy tracked database, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// A lineage old enough to be tracked may also predate content_hash (#4259):
// the copy reads the column list off the schema rather than assuming it, or
// the backup dies on an unknown column and the heal never runs.
func TestHealCopiesLegacyShapeWithoutContentHash(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	expectIgnoredCursorBackup(mock, false)
	expectIgnoredCursorUntrackCommit(mock)
	expectIgnoredCursorRestore(mock, false)

	if _, err := healTrackedIgnoredCursorTable(context.Background(), db); err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The scratch table carries a perfectly committable name, so a concurrent
// writer's blanket commit can sweep it into HEAD during the repair window.
// Dropping it locally would then leave a permanent delete delta — the same
// class of tracked residue this whole fix exists to remove — so the deletion
// is committed, scoped to that table.
func TestHealCommitsAStrayScratchTableOutOfHead(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	expectIgnoredCursorBackup(mock, true)
	expectIgnoredCursorUntrackCommit(mock)
	expectIgnoredCursorRestore(mock, true)

	if _, err := healTrackedIgnoredCursorTable(context.Background(), db); err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The resume path: a previous open committed the drop and died before putting
// the rows back. The cursor table now reads as untracked, so the gate would
// happily call the database healthy — the surviving scratch table is the only
// evidence that it is not, and skipping this probe would lose the migration
// cursor and replay the whole ignored series from zero.
func TestHealResumesFromASurvivingScratchTable(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorUntracked(mock, "")
	expectCursorProbe(mock, ignoredCursorUntrackTempTable, true)
	expectIgnoredCursorRestore(mock, false)

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v", err)
	}
	if !healed {
		t.Fatal("healTrackedIgnoredCursorTable() = false with an interrupted run to resume, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The other crash window: the drop ran but its commit did not, so HEAD still
// carries the table while the working set does not. Phase A re-runs from the
// top, and the copy must treat the missing source as "already saved" rather
// than as an error — otherwise the pre-mutation policy swallows it and the
// database never converges.
func TestHealResumesWhenTheDroppedTableIsGoneFromTheWorkingSet(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS " + ignoredCursorUntrackTempTable)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectCursorProbe(mock, ignoredSource.cursorTable, false)
	expectIgnoredCursorUntrackCommit(mock)
	expectIgnoredCursorRestore(mock, false)

	healed, err := healTrackedIgnoredCursorTable(context.Background(), db)
	if err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v", err)
	}
	if !healed {
		t.Fatal("healTrackedIgnoredCursorTable() = false resuming a committed-drop crash, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The repair's own commit is staged-only, so anything a caller left staged
// would ride into it. Unstage first — the hazard 0040/0041's
// commitNonlocalRepair documents.
func TestHealUnstagesPreExistingStagedTablesBeforeItsCommit(t *testing.T) {
	db, mock := newMockDB(t)

	expectIgnoredCursorTracked(mock, "", true)
	expectIgnoredCursorBackup(mock, true)
	mock.ExpectQuery("(?s)SELECT s\\.table_name, s\\.staged\\s+FROM dolt_status s").
		WillReturnRows(sqlmock.NewRows([]string{"table_name", "staged"}).AddRow("issues", true))
	mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_RESET(?)")).
		WithArgs("issues").
		WillReturnRows(sqlmock.NewRows([]string{"status"}))
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS " + ignoredSource.cursorTable)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("COMMIT")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_ADD('-f', ?)")).
		WithArgs(ignoredSource.cursorTable).
		WillReturnRows(sqlmock.NewRows([]string{"status"}))
	mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_COMMIT('--skip-empty', '-m', ?)")).
		WithArgs(ignoredCursorUntrackCommitMessage).
		WillReturnRows(sqlmock.NewRows([]string{"hash"}))
	expectIgnoredCursorRestore(mock, false)

	if _, err := healTrackedIgnoredCursorTable(context.Background(), db); err != nil {
		t.Fatalf("healTrackedIgnoredCursorTable() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The probe reads a LISTING of HEAD rather than selecting from the table
// itself. Both spellings answer the question, but the direct one FAILS on
// every healthy database, and a Dolt session that issues a failing statement
// stays pinned to its pre-statement catalog snapshot for the rest of its
// pooled life (be-bv7x). Pin the always-succeeding spelling: a mock that
// offers nothing but the listing would reject anything else.
func TestTrackedAtHeadProbeIssuesNoFailingStatement(t *testing.T) {
	for _, tt := range []struct {
		name      string
		qualifier string
	}{
		{name: "session already on the database", qualifier: ""},
		{name: "database selected for us", qualifier: "`testdb`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := newMockDB(t)

			expectHeadTableListing(mock, tt.qualifier, headTables...)

			tracked, err := tableTrackedAtHead(context.Background(), db, tt.qualifier, ignoredSource.cursorTable)
			if err != nil {
				t.Fatalf("tableTrackedAtHead() error = %v", err)
			}
			if tracked {
				t.Fatal("tableTrackedAtHead() = true with the cursor table absent from HEAD")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet SQL expectations: %v", err)
			}
		})
	}
}

// TestAlreadyConvergedDeclinesOnALegacyTrackedCursor is the server-mode half
// of the fix. A wedged legacy database is at-latest and fully seeded — its
// dirt is the residue of a COMPLETED pass — so every predicate the fast path
// checked before this one says "converged", MigrateUp never runs, and the heal
// placed inside it would be unreachable on exactly the mode that has the most
// of these databases.
func TestAlreadyConvergedDeclinesOnALegacyTrackedCursor(t *testing.T) {
	db, mock := newMockDB(t)

	expectCurrentDatabase(mock, "testdb")
	expectNoMigrationWorkNeeded(mock)
	expectDoltIgnoreRead(mock, unqualifiedDoltIgnore, seededIgnorePatterns(LatestVersion()))
	expectIgnoredCursorTracked(mock, "", true)

	converged, err := alreadyConverged(context.Background(), db, "testdb", nil)
	if err != nil {
		t.Fatalf("alreadyConverged() error = %v", err)
	}
	if converged {
		t.Fatal("alreadyConverged() = true on a legacy tracked cursor table, want false: the locked path is the only one that heals it")
	}
	// The lock probe must never be reached — the mock is not primed for it, so
	// an unexpected IS_FREE_LOCK would surface here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The mirror image, and the reason the tracked probe also reads the ignore
// flag: an operator who un-ignored the cursor table has a legitimately
// tracked, legitimately converged database. Declining it would hand that fleet
// a GET_LOCK on every single invocation, forever — the exact saturation the
// fast path was built to remove.
func TestAlreadyConvergedAcceptsATrackedCursorTheOperatorUnignored(t *testing.T) {
	db, mock := newMockDB(t)

	expectCurrentDatabase(mock, "testdb")
	expectNoMigrationWorkNeeded(mock)
	expectDoltIgnoreRead(mock, unqualifiedDoltIgnore, seededIgnorePatterns(LatestVersion()))
	expectIgnoredCursorTracked(mock, "", false)
	expectMigrationLockProbe(mock, "testdb", 1)

	converged, err := alreadyConverged(context.Background(), db, "testdb", nil)
	if err != nil {
		t.Fatalf("alreadyConverged() error = %v", err)
	}
	if !converged {
		t.Fatal("alreadyConverged() = false on a database whose operator deliberately versions the cursor table, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// The fast path's probes fail closed, and this one is no exception: an
// unreadable HEAD listing surfaces as an error, which MigrateUpWithLock logs
// and turns into an ordinary locked pass.
func TestAlreadyConvergedFailsClosedOnAnUnreadableHeadListing(t *testing.T) {
	db, mock := newMockDB(t)

	expectCurrentDatabase(mock, "testdb")
	expectNoMigrationWorkNeeded(mock)
	expectDoltIgnoreRead(mock, unqualifiedDoltIgnore, seededIgnorePatterns(LatestVersion()))
	mock.ExpectQuery(regexp.QuoteMeta("SHOW TABLES AS OF 'HEAD'")).
		WillReturnError(sql.ErrConnDone)

	converged, err := alreadyConverged(context.Background(), db, "testdb", nil)
	if err == nil {
		t.Fatal("alreadyConverged() error = nil, want the HEAD listing failure")
	}
	if converged {
		t.Fatal("alreadyConverged() = true on an unreadable HEAD listing, want false")
	}
}

// The lock probe's comment requires it to stay LAST — everything above it is
// the cheap steady-state question, and the tracked probe is now part of that.
// A database in the legacy shape must decline without ever asking for the
// lock's state.
func TestAlreadyConvergedKeepsTheLockProbeLast(t *testing.T) {
	db, mock := newMockDB(t)

	expectCurrentDatabase(mock, "testdb")
	expectNoMigrationWorkNeeded(mock)
	expectDoltIgnoreRead(mock, unqualifiedDoltIgnore, seededIgnorePatterns(LatestVersion()))
	expectIgnoredCursorTracked(mock, "", true)

	if _, err := alreadyConverged(context.Background(), db, "testdb", nil); err != nil {
		t.Fatalf("alreadyConverged() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations (IS_FREE_LOCK must not run once a cheaper term has already declined): %v", err)
	}
}
