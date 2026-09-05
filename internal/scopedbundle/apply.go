package scopedbundle

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// ApplyOptions bind one apply to an exact pre-mutation state and actor.
type ApplyOptions struct {
	ExpectedCurrentSHA256 string
	Actor                 string
	JournalEnabled        bool
}

type ApplyResult struct {
	BeforeSHA256 string
	AfterSHA256  string
	Changed      bool
}

// Apply atomically reconciles only the reviewed target IDs. It does not create
// a database, alter schema, commit Dolt history, or perform remote operations.
func Apply(ctx context.Context, db *sql.DB, bundle Bundle, options ApplyOptions) (ApplyResult, error) {
	return apply(ctx, db, bundle, options, nil)
}

func apply(ctx context.Context, db *sql.DB, bundle Bundle, options ApplyOptions, afterMutation func(*sql.Tx) error) (result ApplyResult, err error) {
	if strings.TrimSpace(options.ExpectedCurrentSHA256) == "" {
		return ApplyResult{}, fmt.Errorf("expected current SHA-256 is required")
	}
	if options.JournalEnabled && strings.TrimSpace(options.Actor) == "" {
		return ApplyResult{}, fmt.Errorf("actor is required when the events journal is enabled")
	}
	if err := bundle.Verify(); err != nil {
		return ApplyResult{}, fmt.Errorf("verify bundle: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin scoped apply: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	targetSchema, err := InspectSchema(ctx, tx)
	if err != nil {
		return ApplyResult{}, err
	}
	desired, err := materializeDesired(bundle, targetSchema)
	if err != nil {
		return ApplyResult{}, err
	}
	desiredSHA, err := digestTables(desired)
	if err != nil {
		return ApplyResult{}, err
	}
	current, err := inspectWithSchema(ctx, tx, bundle.Mapping, TargetSide, targetSchema)
	if err != nil {
		return ApplyResult{}, err
	}
	result.BeforeSHA256 = current.SHA256
	if current.SHA256 == desiredSHA {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			return ApplyResult{}, fmt.Errorf("finish no-op apply: %w", err)
		}
		return ApplyResult{BeforeSHA256: current.SHA256, AfterSHA256: current.SHA256, Changed: false}, nil
	}
	if current.SHA256 != options.ExpectedCurrentSHA256 {
		return ApplyResult{}, fmt.Errorf("expected current SHA-256 %s but found %s", options.ExpectedCurrentSHA256, current.SHA256)
	}
	if err := validateImmutableHistory(current.Tables, desired); err != nil {
		return ApplyResult{}, err
	}
	if err := validateGlobalIDCollisions(ctx, tx, desired); err != nil {
		return ApplyResult{}, err
	}

	delta := computeDelta(current.Tables, desired)
	cleanupJournal := issueops.ScopeEventsJournalTransaction(tx, options.JournalEnabled)
	defer cleanupJournal()
	if err := reconcileTables(ctx, tx, bundle.Mapping, current.Tables, desired); err != nil {
		return ApplyResult{}, err
	}
	if afterMutation != nil {
		if err := afterMutation(tx); err != nil {
			return ApplyResult{}, fmt.Errorf("injected apply failure: %w", err)
		}
	}
	if err := recordJournalDelta(ctx, tx, delta, desired, options); err != nil {
		return ApplyResult{}, err
	}

	post, err := inspectWithSchema(ctx, tx, bundle.Mapping, TargetSide, targetSchema)
	if err != nil {
		return ApplyResult{}, err
	}
	if post.SHA256 != desiredSHA {
		return ApplyResult{}, fmt.Errorf("postcondition digest mismatch: got %s want %s", post.SHA256, desiredSHA)
	}
	if err := tx.Commit(); err != nil {
		return ApplyResult{}, fmt.Errorf("commit scoped apply: %w", err)
	}
	return ApplyResult{BeforeSHA256: current.SHA256, AfterSHA256: post.SHA256, Changed: true}, nil
}

func validateImmutableHistory(current, desired []Table) error {
	for _, name := range []string{"comments", "events"} {
		currentTable, _ := findTable(current, name)
		desiredTable, _ := findTable(desired, name)
		desiredRows := rowsByPrimaryID(desiredTable)
		for _, row := range currentTable.Rows {
			id, err := rowID(currentTable, row)
			if err != nil {
				return err
			}
			wanted, exists := desiredRows[id]
			if !exists {
				return fmt.Errorf("destination-only %s row %q is unrepresentable", name, id)
			}
			if rowSortKey(row) != rowSortKey(wanted) {
				return fmt.Errorf("destination %s row %q collides with different content", name, id)
			}
		}
	}
	return nil
}

func validateGlobalIDCollisions(ctx context.Context, tx *sql.Tx, desired []Table) error {
	for _, name := range []string{"comments", "dependencies", "events"} {
		table, _ := findTable(desired, name)
		ids := primaryIDs(table)
		if len(ids) == 0 {
			continue
		}
		existing, err := exportTableByIDs(ctx, tx, table, ids)
		if err != nil {
			return err
		}
		wanted := rowsByPrimaryID(table)
		for _, row := range existing.Rows {
			id, err := rowID(existing, row)
			if err != nil {
				return err
			}
			if rowSortKey(row) != rowSortKey(wanted[id]) {
				return fmt.Errorf("global %s ID collision for %q", name, id)
			}
		}
	}
	return nil
}

func exportTableByIDs(ctx context.Context, db queryer, shape Table, ids []string) (Table, error) {
	columns := make([]string, len(shape.Columns))
	for i, column := range shape.Columns {
		columns[i] = quoteIdentifier(column.Name)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := "SELECT " + strings.Join(columns, ",") + " FROM " + quoteIdentifier(shape.Name) + " WHERE `id` IN (" + placeholders + ")"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return Table{}, fmt.Errorf("check %s ID collisions: %w", shape.Name, err)
	}
	defer rows.Close()
	result := Table{Name: shape.Name, Columns: shape.Columns}
	for rows.Next() {
		values := make([]any, len(shape.Columns))
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return Table{}, fmt.Errorf("scan %s collision row: %w", shape.Name, err)
		}
		row := Row{Cells: make([]Cell, len(values))}
		for i, value := range values {
			row.Cells[i] = normalizeCell(value)
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Table{}, err
	}
	return result, nil
}

func reconcileTables(ctx context.Context, tx *sql.Tx, mapping Mapping, current, desired []Table) error {
	issues, _ := findTable(desired, "issues")
	if err := upsertRows(ctx, tx, issues); err != nil {
		return err
	}
	ids, err := mapping.TargetIDs()
	if err != nil {
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := stringsToAny(ids)

	if _, err := tx.ExecContext(ctx, "DELETE FROM `labels` WHERE `issue_id` IN ("+placeholders+")", args...); err != nil {
		return fmt.Errorf("replace labels: %w", err)
	}
	labels, _ := findTable(desired, "labels")
	if err := insertRows(ctx, tx, labels, nil); err != nil {
		return err
	}

	depArgs := append(append([]any{}, args...), args...)
	if _, err := tx.ExecContext(ctx, "DELETE FROM `dependencies` WHERE `issue_id` IN ("+placeholders+") OR `depends_on_issue_id` IN ("+placeholders+")", depArgs...); err != nil {
		return fmt.Errorf("replace dependencies: %w", err)
	}
	dependencies, _ := findTable(desired, "dependencies")
	if err := insertRows(ctx, tx, dependencies, nil); err != nil {
		return err
	}

	for _, name := range []string{"comments", "events"} {
		table, _ := findTable(desired, name)
		currentTable, _ := findTable(current, name)
		if err := insertRows(ctx, tx, table, rowsByPrimaryID(currentTable)); err != nil {
			return err
		}
	}
	return nil
}

func upsertRows(ctx context.Context, tx *sql.Tx, table Table) error {
	if len(table.Rows) == 0 {
		return nil
	}
	columns := quotedColumnList(table.Columns)
	updates := make([]string, 0, len(table.Columns)-1)
	for _, column := range table.Columns {
		if column.Name != "id" {
			quoted := quoteIdentifier(column.Name)
			updates = append(updates, quoted+"=VALUES("+quoted+")")
		}
	}
	query := "INSERT INTO " + quoteIdentifier(table.Name) + " (" + strings.Join(columns, ",") + ") VALUES (" + placeholders(len(table.Columns)) + ") ON DUPLICATE KEY UPDATE " + strings.Join(updates, ",")
	for _, row := range table.Rows {
		if _, err := tx.ExecContext(ctx, query, cellsToAny(row.Cells)...); err != nil {
			return fmt.Errorf("upsert %s: %w", table.Name, err)
		}
	}
	return nil
}

func insertRows(ctx context.Context, tx *sql.Tx, table Table, existing map[string]Row) error {
	if len(table.Rows) == 0 {
		return nil
	}
	query := "INSERT INTO " + quoteIdentifier(table.Name) + " (" + strings.Join(quotedColumnList(table.Columns), ",") + ") VALUES (" + placeholders(len(table.Columns)) + ")"
	for _, row := range table.Rows {
		if existing != nil {
			id, err := rowID(table, row)
			if err != nil {
				return err
			}
			if _, ok := existing[id]; ok {
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, query, cellsToAny(row.Cells)...); err != nil {
			return fmt.Errorf("insert %s: %w", table.Name, err)
		}
	}
	return nil
}

type applyDelta struct {
	issueCreates []string
	issueUpdates []string
	depRemoves   []Row
	depAdds      []Row
	comments     []Row
}

func computeDelta(current, desired []Table) applyDelta {
	var delta applyDelta
	currentIssues, _ := findTable(current, "issues")
	desiredIssues, _ := findTable(desired, "issues")
	currentByID := rowsByPrimaryID(currentIssues)
	for _, row := range desiredIssues.Rows {
		id, _ := rowID(desiredIssues, row)
		old, exists := currentByID[id]
		if !exists {
			delta.issueCreates = append(delta.issueCreates, id)
		} else if rowSortKey(old) != rowSortKey(row) {
			delta.issueUpdates = append(delta.issueUpdates, id)
		}
	}
	currentDeps, _ := findTable(current, "dependencies")
	desiredDeps, _ := findTable(desired, "dependencies")
	delta.depRemoves, delta.depAdds = rowSetDelta(currentDeps.Rows, desiredDeps.Rows)
	currentComments, _ := findTable(current, "comments")
	desiredComments, _ := findTable(desired, "comments")
	currentCommentsByID := rowsByPrimaryID(currentComments)
	for _, row := range desiredComments.Rows {
		id, _ := rowID(desiredComments, row)
		if _, exists := currentCommentsByID[id]; !exists {
			delta.comments = append(delta.comments, row)
		}
	}
	return delta
}

func recordJournalDelta(ctx context.Context, tx *sql.Tx, delta applyDelta, desired []Table, options ApplyOptions) error {
	if !options.JournalEnabled {
		return nil
	}
	for _, id := range delta.issueCreates {
		if err := issueops.RecordEventInTx(ctx, tx, issueops.EventCreate, id, options.Actor); err != nil {
			return err
		}
	}
	for _, id := range delta.issueUpdates {
		if err := issueops.RecordEventInTx(ctx, tx, issueops.EventUpdate, id, options.Actor); err != nil {
			return err
		}
	}
	deps, _ := findTable(desired, "dependencies")
	for _, item := range []struct {
		op   issueops.EventOp
		rows []Row
	}{
		{issueops.EventDepRemove, delta.depRemoves},
		{issueops.EventDepAdd, delta.depAdds},
	} {
		for _, row := range item.rows {
			issueID := cellText(deps, row, "issue_id")
			target := cellText(deps, row, "depends_on_issue_id")
			kind := cellText(deps, row, "type")
			metadata := cellText(deps, row, "metadata")
			if err := issueops.RecordDepEventInTx(ctx, tx, item.op, issueID, kind, target, metadata, options.Actor); err != nil {
				return err
			}
		}
	}
	comments, _ := findTable(desired, "comments")
	for _, row := range delta.comments {
		createdAt, err := parseSQLTime(cellText(comments, row, "created_at"))
		if err != nil {
			return err
		}
		comment := &issueops.EventComment{
			ID:        cellText(comments, row, "id"),
			Author:    cellText(comments, row, "author"),
			Text:      cellText(comments, row, "text"),
			CreatedAt: createdAt,
			Source:    issueops.CommentSourceStructured,
		}
		if err := issueops.RecordCommentEventInTx(ctx, tx, cellText(comments, row, "issue_id"), comment); err != nil {
			return err
		}
	}
	return nil
}

func parseSQLTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrepresentable comment timestamp %q", value)
}

func rowSetDelta(oldRows, newRows []Row) (removed, added []Row) {
	oldSet := make(map[string]Row, len(oldRows))
	newSet := make(map[string]Row, len(newRows))
	for _, row := range oldRows {
		oldSet[rowSortKey(row)] = row
	}
	for _, row := range newRows {
		newSet[rowSortKey(row)] = row
	}
	for key, row := range oldSet {
		if _, ok := newSet[key]; !ok {
			removed = append(removed, row)
		}
	}
	for key, row := range newSet {
		if _, ok := oldSet[key]; !ok {
			added = append(added, row)
		}
	}
	sort.Slice(removed, func(i, j int) bool { return rowSortKey(removed[i]) < rowSortKey(removed[j]) })
	sort.Slice(added, func(i, j int) bool { return rowSortKey(added[i]) < rowSortKey(added[j]) })
	return removed, added
}

func rowsByPrimaryID(table Table) map[string]Row {
	result := make(map[string]Row, len(table.Rows))
	for _, row := range table.Rows {
		id, err := rowID(table, row)
		if err == nil {
			result[id] = row
		}
	}
	return result
}

func rowID(table Table, row Row) (string, error) {
	index, ok := tableColumnIndex(table, "id")
	if !ok {
		return "", fmt.Errorf("table %s has no id column", table.Name)
	}
	if row.Cells[index].Null || row.Cells[index].Text == "" {
		return "", fmt.Errorf("table %s has an empty row id", table.Name)
	}
	return row.Cells[index].Text, nil
}

func primaryIDs(table Table) []string {
	ids := make([]string, 0, len(table.Rows))
	for _, row := range table.Rows {
		if id, err := rowID(table, row); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func quotedColumnList(columns []Column) []string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdentifier(column.Name)
	}
	return quoted
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func cellsToAny(cells []Cell) []any {
	values := make([]any, len(cells))
	for i, cell := range cells {
		if cell.Null {
			values[i] = nil
		} else {
			values[i] = cell.Text
		}
	}
	return values
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func cellText(table Table, row Row, column string) string {
	index, ok := tableColumnIndex(table, column)
	if !ok || row.Cells[index].Null {
		return ""
	}
	return row.Cells[index].Text
}
