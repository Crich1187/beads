package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// maxRenameHops limits how many rename events GetIssueInTx will follow in a
// single chain. Chains longer than this are treated as not found to avoid
// unbounded work and accidental cycles.
const maxRenameHops = 8

// GetIssueInTx retrieves a single issue by ID within an existing transaction,
// including its labels. Automatically routes to the wisps/wisp_labels tables
// if the ID is an active wisp. If the ID is not present, it follows
// event_type=renamed rows in events and wisp_events (old_value -> new_value)
// with a hop limit of maxRenameHops and cycle detection. Returns
// storage.ErrNotFound (wrapped) if the issue does not exist and no rename
// chain resolves it.
func GetIssueInTx(ctx context.Context, tx DBTX, id string) (*types.Issue, error) {
	return getIssueInTxRecursive(ctx, tx, id, 0, nil)
}

func getIssueInTxRecursive(ctx context.Context, tx DBTX, id string, hops int, visited map[string]bool) (*types.Issue, error) {
	if hops > maxRenameHops {
		return nil, fmt.Errorf("%w: rename chain exceeded %d hops for issue %s", storage.ErrNotFound, maxRenameHops, id)
	}
	if visited == nil {
		visited = make(map[string]bool)
	}
	if visited[id] {
		return nil, fmt.Errorf("%w: rename cycle detected at issue %s", storage.ErrNotFound, id)
	}
	visited[id] = true

	issue, err := getIssueFromTableInTx(ctx, tx, "issues", "labels", id)
	if err == nil {
		return issue, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	issue, err = getIssueFromTableInTx(ctx, tx, "wisps", "wisp_labels", id)
	if err == nil {
		return issue, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	nextID, found, err := resolveRenameInTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
	}

	return getIssueInTxRecursive(ctx, tx, nextID, hops+1, visited)
}

// resolveRenameInTx looks for the most recent event_type=renamed row where
// old_value matches id, searching both events and wisp_events. It returns the
// corresponding new_value. Renames are ordered by created_at DESC, then id
// DESC, so a tie is broken deterministically.
func resolveRenameInTx(ctx context.Context, tx DBTX, id string) (string, bool, error) {
	tables := []string{"events", "wisp_events"}
	var nextID string
	var newest time.Time
	found := false

	for _, table := range tables {
		var candidate string
		var at time.Time
		//nolint:gosec // G201: table names are hardcoded literals.
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT new_value, created_at FROM %s
			WHERE old_value = ? AND event_type = ?
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		`, table), id, string(types.EventRenamed)).Scan(&candidate, &at)
		if err == sql.ErrNoRows {
			continue
		}
		// Pre-migration databases may not have the events or wisp_events
		// tables; treat their absence the same as "no rename event".
		if dberrors.IsMissingTable(err, table) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("resolve rename from %s: %w", table, err)
		}
		if !found || at.After(newest) || (at.Equal(newest) && candidate > nextID) {
			nextID = candidate
			newest = at
			found = true
		}
	}

	return nextID, found, nil
}

// missingOptionalIssueTable reports whether err is the absence of the optional
// issue plane the hydration query just read. The hydration FROM clause also
// carries sqlbuild.LeaseJoin, so a blanket table-not-exist check here folds a
// missing leases table into "row absent" — a wrong answer, not an empty one.
func missingOptionalIssueTable(err error, issueTable string) bool {
	return optionalBlockedTable(issueTable) && dberrors.IsMissingTable(err, issueTable)
}

func getIssueFromTableInTx(ctx context.Context, tx DBTX, issueTable, labelTable, id string) (*types.Issue, error) {
	//nolint:gosec // G201: issueTable is a hardcoded literal supplied by GetIssueInTx ("issues" or "wisps")
	row := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s %s WHERE id = ?`,
		IssueSelectColumns, issueTable, sqlbuild.LeaseJoin(issueTable)), id)
	issue, err := ScanIssueFrom(row)
	if err == sql.ErrNoRows || missingOptionalIssueTable(err, issueTable) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get issue: %w", err)
	}

	// Fetch labels in the same transaction to avoid MaxOpenConns=1 deadlock.
	labels, err := GetLabelsInTx(ctx, tx, labelTable, id)
	if err != nil {
		return nil, fmt.Errorf("get issue labels: %w", err)
	}
	issue.Labels = labels

	return issue, nil
}
