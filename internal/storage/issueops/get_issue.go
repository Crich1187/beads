package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

const maxRenameHops = 8

// GetIssueInTx retrieves a single issue by ID within an existing transaction,
// including its labels. Automatically routes to the wisps/wisp_labels tables
// if the ID is an active wisp. After bd rename, the old ID is gone from the
// issues table but events.renamed (old_value → new_value) remains; follow
// that chain so bd show <old-id> resolves instead of 404ing. Returns
// storage.ErrNotFound (wrapped) if the issue does not exist in either table
// and no rename event points at a live row.
func GetIssueInTx(ctx context.Context, tx DBTX, id string) (*types.Issue, error) {
	seen := make(map[string]struct{}, maxRenameHops)
	cur := id
	for hop := 0; hop < maxRenameHops; hop++ {
		if _, ok := seen[cur]; ok {
			break
		}
		seen[cur] = struct{}{}

		issue, err := getIssueFromTableInTx(ctx, tx, "issues", "labels", cur)
		if err == nil {
			return issue, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}

		issue, err = getIssueFromTableInTx(ctx, tx, "wisps", "wisp_labels", cur)
		if err == nil {
			return issue, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}

		next, err := lookupRenamedIDInTx(ctx, tx, cur)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				break
			}
			return nil, err
		}
		cur = next
	}
	return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
}

// lookupRenamedIDInTx returns the latest new_value for a renamed old ID.
func lookupRenamedIDInTx(ctx context.Context, tx DBTX, oldID string) (string, error) {
	for _, table := range []string{"events", "wisp_events"} {
		//nolint:gosec // G201: table is a hardcoded literal
		row := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT new_value FROM %s WHERE event_type = 'renamed' AND old_value = ? ORDER BY created_at DESC LIMIT 1`,
			table,
		), oldID)
		var newID sql.NullString
		err := row.Scan(&newID)
		if err == sql.ErrNoRows || isTableNotExistError(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("lookup renamed id: %w", err)
		}
		if newID.Valid && newID.String != "" && newID.String != oldID {
			return newID.String, nil
		}
	}
	return "", storage.ErrNotFound
}

func getIssueFromTableInTx(ctx context.Context, tx DBTX, issueTable, labelTable, id string) (*types.Issue, error) {
	//nolint:gosec // G201: issueTable is a hardcoded literal supplied by GetIssueInTx ("issues" or "wisps")
	row := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = ?`, IssueSelectColumns, issueTable), id)
	issue, err := ScanIssueFrom(row)
	if err == sql.ErrNoRows || isTableNotExistError(err) {
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
