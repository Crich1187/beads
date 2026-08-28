package schema

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
)

// The ignored-lane migration cursor is dolt_ignore'd on every database this
// binary opens, but dolt_ignore only exempts tables that have never been
// committed: like git's .gitignore, an ignore entry has no effect on a path
// that is already tracked. On lineages old enough to predate the pattern the
// cursor table WAS committed, and nothing in bd can ever clean it again —
// every staging path anti-joins dolt_ignore, so each migration pass re-dirties
// a tracked table that no commit will ever pick up. Dolt's merge preflight
// refuses to start while a tracked table is dirty, so those databases push
// fine and every single `bd dolt pull` dies with
//
//	Error 1105: cannot merge with uncommitted changes
//
// permanently (gastownhall/beads#4356; confirmed on two fleets, 24 and 17
// databases).
//
// The only way to make dolt_ignore effective again is to turn the table's
// presence back into an ADD delta: commit a deletion of the table into HEAD,
// then recreate it in the working set. That is exactly what migration
// 0062_events_dolt_ignore did for `events`, down to the transaction-pinning
// and force-staging quirks; this file is that same operation for the cursor
// table, in Go and re-entrant.
//
// It is deliberately NOT a numbered migration. A one-shot migration runs only
// while the main cursor is behind, and the affected databases are at-latest
// (their dirt is residue of a COMPLETED pass). Worse, a pull that merges a
// not-yet-healed peer's commits re-introduces the tracked table afterwards,
// and a spent migration never re-runs — the multi-machine fleets that are the
// entire affected population would re-wedge permanently. Re-probing at every
// writable open is what makes the heal converge instead of expire.
const (
	// ignoredCursorUntrackTempTable holds the cursor rows across the drop.
	// Dolt persists the working set to disk, so an uncommitted temp table
	// survives a crash: whatever phase is interrupted, the next open finds
	// the rows and finishes the job. The name is deliberately not matched by
	// any dolt_ignore pattern — a straggler must show up in dolt_status
	// rather than hide.
	ignoredCursorUntrackTempTable = "__temp__ignored_schema_migrations_untrack"

	ignoredCursorUntrackCommitMessage = "schema: untrack legacy ignored_schema_migrations so dolt_ignore can apply (gastownhall/beads#4356)"

	ignoredCursorTempSweepCommitMessage = "schema: drop stray ignored_schema_migrations untrack scratch table (gastownhall/beads#4356)"
)

// healTrackedIgnoredCursorTable untracks a legacy tracked-at-HEAD
// ignored_schema_migrations table and restores its rows, reporting whether it
// changed anything. It runs on every writable open and costs one round trip
// (the HEAD table listing) on a healthy database.
//
// Failure policy, straight from the #5816 lesson that a heal must never mint a
// new class of un-openable database: everything up to and including the row
// backup is advisory — a read that fails, a fenced client with no DDL grant, a
// database an operator deliberately opted out of — and degrades to "log it and
// carry on", leaving the caller exactly as well off as it was before this
// function existed (open works, reads/writes/push work, pull stays wedged
// until some privileged open heals it). From the DROP onward a failure is
// returned: the pass must not continue without a cursor table, and every
// intermediate state that can persist is one the next open resumes from.
func healTrackedIgnoredCursorTable(ctx context.Context, db DBConn) (bool, error) {
	needsUntrack, err := ignoredCursorNeedsUntrack(ctx, db, "")
	if err != nil {
		debug.Logf("schema: cannot tell whether %s is tracked at HEAD, leaving it alone: %v\n",
			ignoredSource.cursorTable, err)
		return false, nil
	}

	if !needsUntrack {
		// Either healthy (the normal case, and the end of the hot path) or
		// mid-heal: a previous open committed the drop and was interrupted
		// before it restored the rows. The temp table is the only evidence
		// that distinguishes the two.
		stray, err := tableExists(ctx, db, ignoredCursorUntrackTempTable)
		if err != nil {
			debug.Logf("schema: probing %s: %v\n", ignoredCursorUntrackTempTable, err)
			return false, nil
		}
		if !stray {
			return false, nil
		}
		log.Printf("schema: resuming interrupted %s untrack (gastownhall/beads#4356)", ignoredSource.cursorTable)
		if err := restoreIgnoredCursorRows(ctx, db); err != nil {
			return false, err
		}
		return true, nil
	}

	log.Printf("schema: %s is tracked at HEAD on this database, which permanently wedges `bd dolt pull`; untracking it and preserving its rows (gastownhall/beads#4356)",
		ignoredSource.cursorTable)

	if err := backupIgnoredCursorRows(ctx, db); err != nil {
		// Nothing destructive has happened yet. A partial temp table is
		// harmless: the next open either re-copies into it (INSERT IGNORE on
		// the version PK makes replays a union) or restores from it.
		debug.Logf("schema: cannot back up %s rows, leaving the tracked table in place: %v\n",
			ignoredSource.cursorTable, err)
		return false, nil
	}
	if err := commitIgnoredCursorUntrack(ctx, db); err != nil {
		return false, err
	}
	if err := restoreIgnoredCursorRows(ctx, db); err != nil {
		return false, err
	}
	return true, nil
}

// ignoredCursorNeedsUntrack reports whether this database carries the legacy
// shape: the ignored-lane cursor table committed at HEAD *and* covered by an
// active dolt_ignore pattern. Both terms matter. The first is the wedge
// itself. The second is the operator's veto — dolt_ignore rows carry an
// `ignored` flag, and a row recorded with ignored=0 is an explicit choice to
// keep the table on the versioned plane (seedDoltIgnorePatterns respects the
// same override). Untracking it there would be fighting the operator, and
// leaving that database on the fast path costs nothing.
//
// qualifier is the identifier-quoted database name selectTargetDatabase
// returned, or empty when the session is already on the target database.
func ignoredCursorNeedsUntrack(ctx context.Context, db DBConn, qualifier string) (bool, error) {
	tracked, err := tableTrackedAtHead(ctx, db, qualifier, ignoredSource.cursorTable)
	if err != nil || !tracked {
		return false, err
	}
	return tableActivelyIgnored(ctx, db, qualifier, ignoredSource.cursorTable)
}

// tableTrackedAtHead reports whether table is committed at HEAD, which is the
// only question dolt_ignore's ADD-delta semantics actually turn on.
//
// It asks by LISTING the tables at HEAD rather than by selecting from the one
// table `AS OF 'HEAD'`. The direct spelling is the one the field diagnosed
// with, and it is unambiguous — it returns rows when the table is tracked and
// "table not found" when it is not — but as a probe it is exactly the mistake
// be-bv7x cost us: a Dolt session that issues a FAILING statement stays pinned
// to its pre-statement catalog snapshot for the rest of its pooled life, and
// on a healthy database this probe fails EVERY time, on EVERY open. SHOW
// TABLES AS OF always succeeds (verified against the embedded engine: 19 rows
// with the cursor table untracked, 20 with it tracked), so the hot path issues
// no failing statement at all.
func tableTrackedAtHead(ctx context.Context, db DBConn, qualifier, table string) (bool, error) {
	query := "SHOW TABLES AS OF 'HEAD'"
	if qualifier != "" {
		// The AS OF clause must trail the FROM; `SHOW TABLES AS OF ... FROM x`
		// is a syntax error.
		query = "SHOW TABLES FROM " + qualifier + " AS OF 'HEAD'"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return false, fmt.Errorf("listing tables at HEAD: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("listing tables at HEAD: %w", err)
		}
		if strings.EqualFold(name, table) {
			// Drain nothing further: the answer cannot change, and the
			// deferred Close discards the rest.
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("listing tables at HEAD: %w", err)
	}
	return false, nil
}

// tableActivelyIgnored reports whether table matches a dolt_ignore pattern
// that is switched on. The predicate is the database's own LIKE, matching
// existingIgnoredTables, so pattern semantics and collation are decided in one
// place rather than re-implemented in Go.
func tableActivelyIgnored(ctx context.Context, db DBConn, qualifier, table string) (bool, error) {
	doltIgnore := "dolt_ignore"
	if qualifier != "" {
		doltIgnore = qualifier + ".dolt_ignore"
	}
	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+doltIgnore+" WHERE ignored = 1 AND ? LIKE pattern", table,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("reading dolt_ignore: %w", err)
	}
	return count > 0, nil
}

// backupIgnoredCursorRows copies the cursor rows into the scratch table. The
// copy is a union, not a replacement: CREATE IF NOT EXISTS keeps an earlier
// interrupted run's copy, and INSERT IGNORE on the version primary key merges
// the two, so no recorded migration version can be lost by replaying this from
// any state.
//
// A source table that is missing is not an error. It is the crash window
// between the drop and its commit: HEAD still carries the table (which is why
// Gate 0 sent us here), the working set no longer does, and the rows are
// already safe in the scratch table from the interrupted run.
func backupIgnoredCursorRows(ctx context.Context, db DBConn) error {
	if _, err := db.ExecContext(ctx, ignoredCursorTempBootstrapSQL()); err != nil {
		return fmt.Errorf("creating %s: %w", ignoredCursorUntrackTempTable, err)
	}
	present, err := tableExists(ctx, db, ignoredSource.cursorTable)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	columns, err := ignoredCursorCopyColumns(ctx, db)
	if err != nil {
		return err
	}
	//nolint:gosec // G201: both table names are constants and the column list is a fixed allowlist.
	copySQL := "INSERT IGNORE INTO " + ignoredCursorUntrackTempTable + " (" + columns + ") SELECT " + columns +
		" FROM " + ignoredSource.cursorTable
	if _, err := db.ExecContext(ctx, copySQL); err != nil {
		return fmt.Errorf("copying %s rows to %s: %w", ignoredSource.cursorTable, ignoredCursorUntrackTempTable, err)
	}
	return nil
}

// commitIgnoredCursorUntrack drops the tracked cursor table and commits the
// deletion into HEAD, which is the whole point: after this commit the table's
// presence is an ADD delta again and dolt_ignore genuinely exempts it.
//
// Every statement here is load-bearing, and each one is a lesson 0062 already
// paid for:
//
//   - the unstage sweep, because DOLT_COMMIT commits the STAGED set and this
//     commit must carry nothing but the deletion (the hazard 0040/0041's
//     commitNonlocalRepair documents);
//   - the bare COMMIT, because on the sql-server path migrations run on a
//     transaction-pinned connection and Dolt's staging procedures read the
//     working set, which an open transaction has not published yet. It is a
//     no-op on the autocommit paths (embedded engine, dolt CLI);
//   - the '-f' on DOLT_ADD, because a plain add of a dolt_ignore'd table is a
//     silent no-op, and the table is ignored by construction here — without
//     the force the drop never stages, the recreate below nets it back out,
//     and the database stays exactly as wedged as it started;
//   - '--skip-empty', because a replay from an intermediate state would
//     otherwise die on "nothing to commit" (the 0040/0041 lesson).
func commitIgnoredCursorUntrack(ctx context.Context, db DBConn) error {
	dirty, err := dirtyTables(ctx, db, false)
	if err != nil {
		return fmt.Errorf("reading working set before untracking %s: %w", ignoredSource.cursorTable, err)
	}
	if err := unstagePreExistingTables(ctx, db, dirty); err != nil {
		return fmt.Errorf("unstaging before untracking %s: %w", ignoredSource.cursorTable, err)
	}
	//nolint:gosec // G201: the table name is a constant.
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+ignoredSource.cursorTable); err != nil {
		return fmt.Errorf("dropping tracked %s: %w", ignoredSource.cursorTable, err)
	}
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("committing the session transaction before staging the %s drop: %w",
			ignoredSource.cursorTable, err)
	}
	if err := DrainCall(ctx, db, "CALL DOLT_ADD('-f', ?)", ignoredSource.cursorTable); err != nil {
		return fmt.Errorf("staging the %s drop: %w", ignoredSource.cursorTable, err)
	}
	if err := DrainCall(ctx, db, "CALL DOLT_COMMIT('--skip-empty', '-m', ?)", ignoredCursorUntrackCommitMessage); err != nil {
		return fmt.Errorf("committing the %s drop: %w", ignoredSource.cursorTable, err)
	}
	return nil
}

// restoreIgnoredCursorRows recreates the cursor table in the working set and
// puts the rows back. The recreated table has never been committed on this
// lineage, so it is an ADD delta — the same shape a fresh clone has, on which
// this wedge is impossible by construction.
func restoreIgnoredCursorRows(ctx context.Context, db DBConn) error {
	if _, err := db.ExecContext(ctx, ignoredSource.bootstrapSQL()); err != nil {
		return fmt.Errorf("recreating %s: %w", ignoredSource.cursorTable, err)
	}
	columns, err := ignoredCursorCopyColumns(ctx, db)
	if err != nil {
		return err
	}
	//nolint:gosec // G201: both table names are constants and the column list is a fixed allowlist.
	restoreSQL := "INSERT IGNORE INTO " + ignoredSource.cursorTable + " (" + columns + ") SELECT " + columns +
		" FROM " + ignoredCursorUntrackTempTable
	if _, err := db.ExecContext(ctx, restoreSQL); err != nil {
		return fmt.Errorf("restoring %s rows: %w", ignoredSource.cursorTable, err)
	}
	//nolint:gosec // G201: the table name is a constant.
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+ignoredCursorUntrackTempTable); err != nil {
		return fmt.Errorf("dropping %s: %w", ignoredCursorUntrackTempTable, err)
	}
	return sweepCommittedIgnoredCursorTemp(ctx, db)
}

// sweepCommittedIgnoredCursorTemp cleans up after the one window in which the
// scratch table can end up tracked: it carries a perfectly committable name,
// so a concurrent writer's `-A`-style commit landing between the phases can
// sweep it into HEAD. Dropping it locally would then leave a permanent delete
// delta. Commit the deletion instead, scoped to that table.
//
// Cold path only — reached exactly once per healed database, right after the
// untrack.
func sweepCommittedIgnoredCursorTemp(ctx context.Context, db DBConn) error {
	tracked, err := tableTrackedAtHead(ctx, db, "", ignoredCursorUntrackTempTable)
	if err != nil || !tracked {
		return err
	}
	if err := DrainCall(ctx, db, "CALL DOLT_ADD(?)", ignoredCursorUntrackTempTable); err != nil {
		return fmt.Errorf("staging the %s drop: %w", ignoredCursorUntrackTempTable, err)
	}
	if err := DrainCall(ctx, db, "CALL DOLT_COMMIT('--skip-empty', '-m', ?)", ignoredCursorTempSweepCommitMessage); err != nil {
		return fmt.Errorf("committing the %s drop: %w", ignoredCursorUntrackTempTable, err)
	}
	return nil
}

// ignoredCursorCopyColumns is the column list both the backup and the restore
// move. content_hash was added to the cursor tables out of band (#4259), and
// the databases this heal exists for are old enough to predate it, so the list
// is read from the schema rather than assumed. The scratch table always has
// the column; the check is against whichever side is the older shape, and
// hasContentHashColumn reports false for a table that does not exist at all.
func ignoredCursorCopyColumns(ctx context.Context, db DBConn) (string, error) {
	hasContentHash, err := ignoredSource.hasContentHashColumn(ctx, db)
	if err != nil {
		return "", err
	}
	if hasContentHash {
		return "version, applied_at, content_hash", nil
	}
	return "version, applied_at", nil
}

func ignoredCursorTempBootstrapSQL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version INT PRIMARY KEY,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	content_hash CHAR(64)
)`, ignoredCursorUntrackTempTable)
}

// tableExists reports whether a table is present in the working set of the
// session's current database. It is the be-bv7x probe-before-act shape: a
// query that always succeeds, so a caller can ask about a table that may not
// be there without pinning the pooled session to a stale catalog snapshot.
func tableExists(ctx context.Context, db DBConn, table string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		table,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("probing %s existence: %w", table, err)
	}
	return count > 0, nil
}
