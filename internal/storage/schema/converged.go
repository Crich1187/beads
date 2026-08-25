package schema

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/dberrors"
)

// alreadyConverged reports whether databaseName is already at this binary's
// target schema in every respect MigrateUp checks before its no-work
// short-circuit: the session is on the right database, no migration work is
// pending, and the canonical dolt_ignore patterns are all present. When it
// says yes, a MigrateUp pass would acquire the migration lock, do nothing,
// and return (0, nil).
//
// Why it exists: the whole schema-init probe ran INSIDE the database-scoped
// GET_LOCK, so every bd invocation against a shared Dolt server serialized on
// it even though the probe is a no-op in the steady state. Measured on an
// 18-seat rig, the lock was held 96.7% of a 14s sampling window, held runs had
// a 673ms median and a 4.2s max, and free gaps had a 0ms median — handed
// straight from holder to holder. That queue was 0.4-2.4s of pure waiting on
// every claim, heartbeat, list and comment. The statements under the lock are
// individually cheap; the SERIALIZATION is the cost, so batching them does not
// help and answering the steady-state question WITHOUT the lock does.
//
// It is deliberately advisory and fails closed onto the locked path. Any
// error, any unreadable state, and anything short of provably converged
// returns false, and the caller then behaves exactly as it did before this
// check existed. A false negative costs one ordinary locked pass; false
// positives are avoided by evaluating the same predicates MigrateUp itself
// evaluates, on the same pinned session, with no writes of its own.
func alreadyConverged(ctx context.Context, db DBConn, databaseName string) (bool, error) {
	if databaseName == "" {
		return false, nil
	}
	// The session must already be on the target database. This is what makes
	// skipping a caller's locked bootstrap preparation safe: preparation
	// creates the database and USEs it, and a session that is already on
	// databaseName has provably had both done for it. A NULL result (no
	// database selected, or one dropped underneath us) is not converged.
	var current sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		return false, fmt.Errorf("reading current database: %w", err)
	}
	if !current.Valid || current.String != databaseName {
		return false, nil
	}

	// Exactly MigrateUp's own gate, and it runs first: on a fresh or
	// mid-upgrade database it reports work needed from the cursor probe alone,
	// before any statement that could fail against a missing table.
	needed, err := migrationWorkNeeded(ctx, db)
	if err != nil {
		return false, fmt.Errorf("checking schema migration work: %w", err)
	}
	if needed {
		return false, nil
	}

	// MigrateUp re-asserts the canonical dolt_ignore patterns ahead of that
	// gate precisely because an out-of-band-materialized database can arrive
	// with its cursors at-latest and the patterns missing. Read the same
	// question instead of writing it: an under-seeded database is not
	// converged and must take the locked path, which heals and commits it.
	return doltIgnoreSeeded(ctx, db)
}

// doltIgnoreSeeded reports whether every canonical dolt_ignore pattern
// seedDoltIgnorePatterns would assert is already present. It is the read-only
// counterpart of that seed and shares its version gate: a pattern whose flip
// migration has not been reached yet is not expected, exactly as the seed
// would not insert it.
//
// Presence is judged on the pattern alone, never on its ignored value, because
// INSERT IGNORE would leave an explicit operator override (a pattern recorded
// with ignored=false) untouched. Reporting such a row as missing would send
// every invocation down the locked path forever.
func doltIgnoreSeeded(ctx context.Context, db DBConn) (bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT pattern FROM dolt_ignore")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading dolt_ignore: %w", err)
	}
	defer rows.Close()

	present := make(map[string]struct{})
	for rows.Next() {
		var pattern string
		if err := rows.Scan(&pattern); err != nil {
			return false, fmt.Errorf("reading dolt_ignore: %w", err)
		}
		present[pattern] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading dolt_ignore: %w", err)
	}

	for _, pattern := range doltIgnorePatterns {
		if _, ok := present[pattern]; !ok {
			return false, nil
		}
	}
	mainVersion, err := mainSource.currentVersion(ctx, db)
	if err != nil {
		return false, fmt.Errorf("reading schema version for gated ignore patterns: %w", err)
	}
	for _, gated := range versionGatedDoltIgnorePatterns {
		if mainVersion < gated.minMainVersion {
			continue
		}
		if _, ok := present[gated.pattern]; !ok {
			return false, nil
		}
	}
	return true, nil
}
