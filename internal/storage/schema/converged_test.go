package schema

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectConvergedFastPathMiss primes the pre-lock convergence probe to report
// a session with no selected database, so the fast path declines on its first
// statement and the locked path runs exactly as it did before the probe
// existed. Every MigrateUpWithLock test that exercises the locked path leads
// with it.
func expectConvergedFastPathMiss(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DATABASE()")).
		WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow(nil))
}

// expectCurrentDatabase mocks the fast path's opening question: which database
// is this pinned session actually on?
func expectCurrentDatabase(mock sqlmock.Sqlmock, name string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DATABASE()")).
		WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow(name))
}

// expectNoMigrationWorkNeeded mocks migrationWorkNeeded on a fully upgraded
// database: both cursors at latest, both content_hash columns present, no
// custom-status/type backfill pending.
func expectNoMigrationWorkNeeded(mock sqlmock.Sqlmock) {
	expectCursorProbe(mock, "schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", LatestVersion())
	expectCursorProbe(mock, "ignored_schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM ignored_schema_migrations", "version", LatestIgnoredVersion())
	expectIgnoredSentinelProbes(mock, true)
	expectContentHashColumnExists(mock)
	expectContentHashColumnExists(mock)
	expectScalar(mock, "SELECT COUNT(*) FROM custom_types", "count", 1)
	expectScalar(mock, "SELECT COUNT(*) FROM custom_statuses", "count", 1)
}

// expectDoltIgnoreRead mocks the read-only dolt_ignore probe, returning
// exactly the patterns given, plus the main-cursor read that qualifies the
// version-gated patterns.
func expectDoltIgnoreRead(mock sqlmock.Sqlmock, patterns []string, mainVersion int) {
	rows := sqlmock.NewRows([]string{"pattern"})
	for _, pattern := range patterns {
		rows.AddRow(pattern)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pattern FROM dolt_ignore")).WillReturnRows(rows)
	expectCursorProbe(mock, "schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", mainVersion)
}

// seededIgnorePatterns is every pattern seedDoltIgnorePatterns would assert on
// a database whose main cursor is at mainVersion.
func seededIgnorePatterns(mainVersion int) []string {
	patterns := append([]string(nil), doltIgnorePatterns...)
	for _, gated := range versionGatedDoltIgnorePatterns {
		if mainVersion >= gated.minMainVersion {
			patterns = append(patterns, gated.pattern)
		}
	}
	return patterns
}

// expectConvergedProbe mocks the whole fast path on a converged database.
func expectConvergedProbe(mock sqlmock.Sqlmock, database string) {
	expectCurrentDatabase(mock, database)
	expectNoMigrationWorkNeeded(mock)
	expectDoltIgnoreRead(mock, seededIgnorePatterns(LatestVersion()), LatestVersion())
}

func newMockConn(t *testing.T) (*sql.Conn, sqlmock.Sqlmock, func()) {
	t.Helper()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		t.Fatalf("pin mock connection: %v", err)
	}
	return conn, mock, func() {
		conn.Close()
		db.Close()
	}
}

// TestMigrateUpWithLockSkipsLockWhenAlreadyConverged is the point of the whole
// change: on a database that needs nothing, no GET_LOCK is issued at all. The
// mock is primed with the probe and nothing else, so a GET_LOCK — or the
// caller's locked preparation, whose CREATE DATABASE the fast path also skips
// — would surface as an unexpected statement and fail the call.
func TestMigrateUpWithLockSkipsLockWhenAlreadyConverged(t *testing.T) {
	conn, mock, cleanup := newMockConn(t)
	defer cleanup()

	expectConvergedProbe(mock, "testdb")

	prepared := 0
	applied, err := MigrateUpWithLock(context.Background(), conn, "testdb",
		WithLockedPreparation("tcp:test", func(context.Context, *sql.Conn) (*FreshBootstrapHealCapability, error) {
			prepared++
			return nil, nil
		}))
	if err != nil {
		t.Fatalf("MigrateUpWithLock() error = %v, want nil (converged database must short-circuit before GET_LOCK)", err)
	}
	if applied != 0 {
		t.Fatalf("MigrateUpWithLock() applied = %d, want 0", applied)
	}
	if prepared != 0 {
		t.Fatalf("locked preparation ran %d times on a converged database, want 0", prepared)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// TestMigrateUpWithLockTakesLockWhenBehind pins the other half of the
// contract: a database one migration behind still goes through GET_LOCK and
// the full pass. Without this, a fast path that answered "converged"
// unconditionally would still pass the test above.
func TestMigrateUpWithLockTakesLockWhenBehind(t *testing.T) {
	conn, mock, cleanup := newMockConn(t)
	defer cleanup()

	// Fast path: right database, but the main cursor is behind -> work needed.
	expectCurrentDatabase(mock, "testdb")
	expectCursorProbe(mock, "schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", LatestVersion()-1)

	lockName := MigrationLockName("testdb")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT GET_LOCK(?, ?)")).
		WithArgs(lockName, migrationLockAcquireTimeoutSeconds).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(1))
	expectOnePendingMigration(t, mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT RELEASE_LOCK(?)")).
		WithArgs(lockName).
		WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(1))

	applied, err := MigrateUpWithLock(context.Background(), conn, "testdb")
	if err != nil {
		t.Fatalf("MigrateUpWithLock() error = %v", err)
	}
	if applied != 1 {
		t.Fatalf("MigrateUpWithLock() applied = %d, want 1", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// TestMigrateUpWithLockKeepsLockWithBootstrapHeal pins that a caller carrying
// fresh-bootstrap reset authority never enters the fast path: the very first
// statement it issues is GET_LOCK, so the #5012 bootstrap sequence is
// byte-identical to what it was before the probe existed.
func TestMigrateUpWithLockKeepsLockWithBootstrapHeal(t *testing.T) {
	conn, mock, cleanup := newMockConn(t)
	defer cleanup()

	lockName := MigrationLockName("testdb")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT GET_LOCK(?, ?)")).
		WithArgs(lockName, migrationLockAcquireTimeoutSeconds).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(1))
	expectDirtyGuardRefusal(t, mock)
	expectFreshBootstrapIdentityMatch(mock)
	mock.ExpectQuery(regexp.QuoteMeta("CALL DOLT_RESET('--hard')")).
		WillReturnRows(sqlmock.NewRows([]string{"status"}))
	expectOnePendingMigration(t, mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT RELEASE_LOCK(?)")).
		WithArgs(lockName).
		WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(1))

	applied, err := MigrateUpWithLock(context.Background(), conn, "testdb",
		WithFreshBootstrapHeal(testFreshBootstrapHealCapability(), testBootstrapEndpoint))
	if err != nil {
		t.Fatalf("MigrateUpWithLock() error = %v", err)
	}
	if applied != 1 {
		t.Fatalf("MigrateUpWithLock() applied = %d, want 1", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestAlreadyConvergedRequiresTheTargetDatabase(t *testing.T) {
	tests := []struct {
		name    string
		current any
	}{
		{name: "no database selected", current: nil},
		{name: "different database", current: "otherdb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("create sql mock: %v", err)
			}
			defer db.Close()

			mock.ExpectQuery(regexp.QuoteMeta("SELECT DATABASE()")).
				WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow(tt.current))

			converged, err := alreadyConverged(context.Background(), db, "testdb")
			if err != nil {
				t.Fatalf("alreadyConverged() error = %v", err)
			}
			if converged {
				t.Fatal("alreadyConverged() = true, want false: the session is not on the target database, so nothing has been proved about it")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet SQL expectations (the database check must short-circuit before any schema probe): %v", err)
			}
		})
	}
}

// TestAlreadyConvergedRejectsUnderSeededDoltIgnore is the case
// seedDoltIgnorePatterns exists for: an out-of-band-materialized database
// arrives with its cursors at-latest and the ignore patterns missing. The fast
// path must refuse it so the locked pass can heal and commit the seed.
func TestAlreadyConvergedRejectsUnderSeededDoltIgnore(t *testing.T) {
	for _, missing := range seededIgnorePatterns(LatestVersion()) {
		t.Run(missing, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("create sql mock: %v", err)
			}
			defer db.Close()

			var present []string
			for _, pattern := range seededIgnorePatterns(LatestVersion()) {
				if pattern != missing {
					present = append(present, pattern)
				}
			}
			expectCurrentDatabase(mock, "testdb")
			expectNoMigrationWorkNeeded(mock)
			expectDoltIgnoreRead(mock, present, LatestVersion())

			converged, err := alreadyConverged(context.Background(), db, "testdb")
			if err != nil {
				t.Fatalf("alreadyConverged() error = %v", err)
			}
			if converged {
				t.Fatalf("alreadyConverged() = true with dolt_ignore pattern %q missing, want false", missing)
			}
		})
	}
}

// TestAlreadyConvergedAcceptsOverriddenIgnorePattern pins that presence is
// judged on the pattern alone. An operator override (the row exists with
// ignored=false) is what INSERT IGNORE would leave untouched, so treating it
// as missing would send every invocation down the locked path forever — the
// saturation this change removes.
func TestAlreadyConvergedAcceptsOverriddenIgnorePattern(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer db.Close()

	expectCurrentDatabase(mock, "testdb")
	expectNoMigrationWorkNeeded(mock)
	// The probe selects patterns only; the mock never offers the ignored
	// column, so a query that filtered on it would not match this expectation.
	expectDoltIgnoreRead(mock, seededIgnorePatterns(LatestVersion()), LatestVersion())

	converged, err := alreadyConverged(context.Background(), db, "testdb")
	if err != nil {
		t.Fatalf("alreadyConverged() error = %v", err)
	}
	if !converged {
		t.Fatal("alreadyConverged() = false on a fully seeded database, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

// TestAlreadyConvergedFailsClosedOnUnreadableState pins the fail-closed
// contract from both sides. A cursor probe that errors is swallowed by
// atLatest into "work needed", so the answer is a plain not-converged; a
// dolt_ignore read that errors surfaces the error. MigrateUpWithLock treats
// anything but (true, nil) as "take the lock", so both end at the locked path.
func TestAlreadyConvergedFailsClosedOnUnreadableState(t *testing.T) {
	t.Run("cursor probe fails", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("create sql mock: %v", err)
		}
		defer db.Close()

		expectCurrentDatabase(mock, "testdb")
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM information_schema\.tables`).
			WillReturnError(sql.ErrConnDone)

		converged, err := alreadyConverged(context.Background(), db, "testdb")
		if err != nil {
			t.Fatalf("alreadyConverged() error = %v", err)
		}
		if converged {
			t.Fatal("alreadyConverged() = true on an unreadable cursor, want false")
		}
	})

	t.Run("dolt_ignore read fails", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("create sql mock: %v", err)
		}
		defer db.Close()

		expectCurrentDatabase(mock, "testdb")
		expectNoMigrationWorkNeeded(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT pattern FROM dolt_ignore")).
			WillReturnError(sql.ErrConnDone)

		converged, err := alreadyConverged(context.Background(), db, "testdb")
		if err == nil {
			t.Fatal("alreadyConverged() error = nil, want the dolt_ignore read failure")
		}
		if converged {
			t.Fatal("alreadyConverged() = true on a failed dolt_ignore read, want false")
		}
	})
}
