package schema

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage/depid"
)

// rekeyDependencyIDs rewrites the surrogate primary key of every dependencies
// and wisp_dependencies row whose id does not already equal
// depid.New(issue_id, target). It is the data half of the #4259 fix.
//
// Migration 0043 minted dependencies.id from DEFAULT (UUID()), which is
// per-clone-random; migration 0050 + the deterministic insert paths fix new
// rows, but rows that already exist on an upgrading clone still carry the random
// id. Leaving them would keep two independently-migrated clones divergent (same
// edge, different primary key) and break `bd dolt pull`. This rewrites them to
// the deterministic value so two clones converge to byte-identical dependencies.
//
// It runs from MigrateUp right after the schema migrations (so 0050 has already
// asserted the canonical schema), and only on a pass where migration work was
// needed — it is not part of the steady-state open path. It is idempotent: a row
// already keyed deterministically is skipped, so re-running on a later migration
// pass is a cheap no-op. dependencies changes are staged and committed by
// MigrateUp; wisp_dependencies is dolt-ignored, so its re-key stays clone-local
// (it only escapes on promotion, which copies the id).
//
// gastownhall/beads#5268: depid keys on (issue_id, resolved target) and
// deliberately not on which typed column holds the target, but the unique keys
// are per-column (uk_dep_issue_target / uk_dep_wisp_target /
// uk_dep_external_target), so a legal table can hold the same logical edge
// twice — e.g. an April wisp→issue conversion that recorded the edge as
// external before the target existed as an issue, then again as an issue ref.
// Both rows derive the same id, so the old row-at-a-time rewrite aborted the
// second UPDATE with "duplicate primary key given" partway through the table.
// rekeyDependencyTable now merges those duplicates instead (see its doc), and
// ignored migration 0026 is the completion marker that makes the pass run once
// on every existing clone — including one already at the latest main version
// that a pre-1.3.0 binary left half-rekeyed — and re-run on the next open if it
// ever aborts again.

func rekeyDependencyIDs(ctx context.Context, db DBConn) (bool, error) {
	wroteDeps, err := rekeyDependencyTable(ctx, db, "dependencies")
	if err != nil {
		return wroteDeps, fmt.Errorf("dependencies: %w", err)
	}
	wroteWisp, err := rekeyDependencyTable(ctx, db, "wisp_dependencies")
	if err != nil {
		return wroteDeps || wroteWisp, fmt.Errorf("wisp_dependencies: %w", err)
	}
	return wroteDeps || wroteWisp, nil
}

// depRow is one scanned edge: its current primary key, the natural identity
// depid keys on, and which typed column carried the target.
type depRow struct {
	id        string
	issueID   string
	target    string
	hasTarget bool
	priority  int // index into depTargetColumns; lower wins the merge
}

// depTargetColumns is the COALESCE order of the three typed target columns,
// which is also the survivor priority for a duplicated edge: an issue ref beats
// a wisp ref beats an external ref. Changing this order changes both the
// derived target and which twin survives, so it is the same list for both.
var depTargetColumns = []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}

// rekeyDependencyTable re-derives ids for one edge table. table must be a
// hardcoded constant ("dependencies" or "wisp_dependencies").
//
// Plan-then-execute, deliberately (#5268): the whole table is read and the full
// statement plan validated before a single write, so the pass either applies a
// coherent plan or writes nothing at all. Rows are grouped by their derived id;
// a group with more than one member is the same logical edge recorded in two or
// three typed columns, which is invalid state that the per-column unique keys
// cannot catch. The merge keeps the highest-priority typed row (issue > wisp >
// external, mirroring the COALESCE order) and deletes the rest — the loser's
// type/audit columns go with it, since edge identity deliberately excludes them
// and the resolution has to be a pure function of table content so two clones
// converge byte-identically (#4259). Nothing references dependencies.id, so
// dropping the row is referentially safe.
//
// DELETEs run before UPDATEs because a loser may currently hold exactly the id
// the survivor is being moved to (that is what a half-rekeyed database looks
// like), and updating first would manufacture the very duplicate-primary-key
// abort this replaces.
//
// Every statement is a final-state mutation, so a partially applied pass is
// benign: the rows it touched are already correct, the rest are untouched, and
// a re-run finishes the job. A converged table plans nothing.
func rekeyDependencyTable(ctx context.Context, db DBConn, table string) (bool, error) {
	// Skip cleanly if the table or its id column isn't present (e.g. an older or
	// partial schema where the surrogate key was never added): nothing to re-key.
	hasID, err := columnExists(ctx, db, table, "id")
	if err != nil {
		return false, err
	}
	if !hasID {
		return false, nil
	}

	rows, err := scanDependencyRows(ctx, db, table)
	if err != nil {
		return false, err
	}
	deletes, updates, err := planDependencyRekey(table, rows)
	if err != nil {
		return false, err
	}

	for _, id := range deletes {
		//nolint:gosec // G201: table is a hardcoded constant, never user input.
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, table), id); err != nil {
			return true, fmt.Errorf("drop duplicate edge row %s: %w", id, err)
		}
	}
	for _, u := range updates {
		//nolint:gosec // G201: table is a hardcoded constant, never user input.
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET id = ? WHERE id = ?`, table),
			u.newID, u.oldID); err != nil {
			return true, fmt.Errorf("re-key id %s -> %s: %w", u.oldID, u.newID, err)
		}
	}
	return len(deletes)+len(updates) > 0, nil
}

// scanDependencyRows reads every edge row with its typed target columns exposed
// rather than COALESCEd away: the merge needs to know which column held the
// target, not just the resolved value. Resolution order is unchanged — the first
// non-null in depTargetColumns is exactly what the old COALESCE returned. Rows
// with no target at all — ck_dep_one_target should prevent them — come back with
// hasTarget false and are left alone by the planner, exactly as before, so
// `bd doctor` surfaces them rather than this guessing.
func scanDependencyRows(ctx context.Context, db DBConn, table string) ([]depRow, error) {
	//nolint:gosec // G201: table and the column list are hardcoded constants, never user input.
	rows, err := db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, issue_id, %s FROM %s`, strings.Join(depTargetColumns, ", "), table))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []depRow
	for rows.Next() {
		var id, issueID string
		targets := make([]sql.NullString, len(depTargetColumns))
		dest := []any{&id, &issueID}
		for i := range targets {
			dest = append(dest, &targets[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := depRow{id: id, issueID: issueID, priority: len(depTargetColumns)}
		for i, t := range targets {
			if t.Valid {
				row.target, row.hasTarget, row.priority = t.String, true, i
				break
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type depRekey struct{ oldID, newID string }

// planDependencyRekey turns the scanned table into the ordered DELETE and
// UPDATE plans, or an error if the table cannot be converged without guessing.
// It is a pure function of the scanned rows: group keys and statements are
// emitted in sorted order so two clones with the same data produce the same
// plan (map iteration order must never leak into the result).
func planDependencyRekey(table string, rows []depRow) (deletes []string, updates []depRekey, err error) {
	groups := make(map[string][]depRow)
	byCurrentID := make(map[string]depRow, len(rows))
	for _, r := range rows {
		byCurrentID[r.id] = r
		if !r.hasTarget {
			// Malformed row with no target: not part of any edge group, but it
			// still occupies a primary key, so it stays in byCurrentID for the
			// collision check below.
			continue
		}
		want := depid.New(r.issueID, r.target)
		groups[want] = append(groups[want], r)
	}

	wantIDs := make([]string, 0, len(groups))
	for want := range groups {
		wantIDs = append(wantIDs, want)
	}
	sort.Strings(wantIDs)

	doomed := make(map[string]bool)
	for _, want := range wantIDs {
		group := groups[want]
		// Highest-priority typed column survives. The id tiebreak is
		// unreachable while the uk_dep_* unique keys hold (they forbid two rows
		// with the same issue_id and the same typed target) but keeps the plan
		// deterministic on a database that lost one.
		sort.Slice(group, func(i, j int) bool {
			if group[i].priority != group[j].priority {
				return group[i].priority < group[j].priority
			}
			return group[i].id < group[j].id
		})
		for _, loser := range group[1:] {
			deletes = append(deletes, loser.id)
			doomed[loser.id] = true
		}
		if survivor := group[0]; survivor.id != want {
			updates = append(updates, depRekey{oldID: survivor.id, newID: want})
		}
	}

	// Validate the whole plan before returning it: an UPDATE whose target id is
	// already held by a row that is not being deleted would abort mid-pass on
	// the primary key. That is genuine corruption (a row keyed to some other
	// edge's deterministic id), not a duplicate this can merge, so name both
	// rows and refuse rather than guess.
	for _, u := range updates {
		occupant, held := byCurrentID[u.newID]
		if !held || doomed[occupant.id] {
			continue
		}
		return nil, nil, fmt.Errorf(
			"%s: cannot re-key row %s (issue %s -> %s) to %s: that id is already held by row (issue %s -> %s), which is not a duplicate of it; run `bd doctor` to inspect the table",
			table, u.oldID, byCurrentID[u.oldID].issueID, byCurrentID[u.oldID].target,
			u.newID, occupant.issueID, occupant.target)
	}

	sort.Strings(deletes)
	sort.Slice(updates, func(i, j int) bool { return updates[i].newID < updates[j].newID })
	return deletes, updates, nil
}

// columnExists reports whether table.column is present in the current schema.
func columnExists(ctx context.Context, db DBConn, table, column string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
