package scopedbundle

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	schemaVersionSQL     = "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"
	informationSchemaSQL = "SELECT table_name, column_name, column_type, is_nullable, column_default, extra, generation_expression FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name IN ('comments','dependencies','events','issues','labels') ORDER BY table_name, ordinal_position"
	sourceCensusSQL      = "SELECT id FROM issues WHERE id LIKE ? ORDER BY id"
)

var transferredTables = []string{"issues", "labels", "comments", "dependencies", "events"}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Schema is the exact non-mutating view of the transferable table shapes.
type Schema struct {
	Version int
	Tables  map[string][]Column
}

// InspectSchema reads schema metadata without executing DDL or invoking the
// Beads migration machinery.
func InspectSchema(ctx context.Context, db queryer) (Schema, error) {
	var version int
	if err := db.QueryRowContext(ctx, schemaVersionSQL).Scan(&version); err != nil {
		return Schema{}, fmt.Errorf("read schema version: %w", err)
	}
	if version < 53 {
		return Schema{}, fmt.Errorf("schema v%d is below minimum v53", version)
	}

	rows, err := db.QueryContext(ctx, informationSchemaSQL)
	if err != nil {
		return Schema{}, fmt.Errorf("inspect transferable schema: %w", err)
	}
	defer rows.Close()

	tables := make(map[string][]Column, len(transferredTables))
	for rows.Next() {
		var tableName, columnName, columnType, nullable, extra string
		var defaultValue, generation sql.NullString
		if err := rows.Scan(&tableName, &columnName, &columnType, &nullable, &defaultValue, &extra, &generation); err != nil {
			return Schema{}, fmt.Errorf("scan schema metadata: %w", err)
		}
		extraUpper := strings.ToUpper(extra)
		column := Column{
			Name:      columnName,
			SQLType:   columnType,
			Nullable:  nullable == "YES",
			Generated: (generation.Valid && generation.String != "") || strings.Contains(extraUpper, "VIRTUAL GENERATED") || strings.Contains(extraUpper, "STORED GENERATED"),
		}
		if defaultValue.Valid {
			value := defaultValue.String
			column.Default = &value
		}
		tables[tableName] = append(tables[tableName], column)
	}
	if err := rows.Err(); err != nil {
		return Schema{}, fmt.Errorf("iterate schema metadata: %w", err)
	}

	required := map[string][]string{
		"issues":       {"id"},
		"labels":       {"issue_id"},
		"comments":     {"id", "issue_id"},
		"dependencies": {"id", "issue_id", "depends_on_issue_id"},
		"events":       {"id", "issue_id"},
	}
	for _, tableName := range transferredTables {
		columns, ok := tables[tableName]
		if !ok {
			return Schema{}, fmt.Errorf("required table %s is missing", tableName)
		}
		for _, name := range required[tableName] {
			if !hasColumn(columns, name) {
				return Schema{}, fmt.Errorf("required column %s.%s is missing", tableName, name)
			}
		}
	}
	return Schema{Version: version, Tables: tables}, nil
}

func hasColumn(columns []Column, name string) bool {
	for _, column := range columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

// Export reads exactly the reviewed source namespace and returns a sealed,
// deterministic bundle. A new or missing issue in that namespace aborts before
// any table rows are exported.
func Export(ctx context.Context, db queryer, mapping Mapping) (*Bundle, error) {
	if err := mapping.Validate(); err != nil {
		return nil, fmt.Errorf("mapping: %w", err)
	}
	schema, err := InspectSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := verifySourceCensus(ctx, db, mapping); err != nil {
		return nil, err
	}

	ids, err := mapping.SourceIDs()
	if err != nil {
		return nil, err
	}
	tables := make([]Table, 0, len(transferredTables))
	for _, name := range transferredTables {
		table, err := exportTable(ctx, db, name, schema.Tables[name], ids)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	bundle := &Bundle{
		Format:       BundleFormat,
		Version:      BundleVersion,
		SourceSchema: schema.Version,
		Mapping:      mapping,
		Tables:       tables,
	}
	if err := bundle.Seal(); err != nil {
		return nil, fmt.Errorf("seal bundle: %w", err)
	}
	return bundle, nil
}

func verifySourceCensus(ctx context.Context, db queryer, mapping Mapping) error {
	rows, err := db.QueryContext(ctx, sourceCensusSQL, mapping.SourcePrefix+"%")
	if err != nil {
		return fmt.Errorf("read source census: %w", err)
	}
	defer rows.Close()
	actual := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan source census: %w", err)
		}
		actual[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate source census: %w", err)
	}
	expected, err := mapping.SourceIDs()
	if err != nil {
		return err
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		expectedSet[id] = struct{}{}
		if _, ok := actual[id]; !ok {
			return fmt.Errorf("missing source id %q; source changed, re-freeze required", id)
		}
	}
	for id := range actual {
		if _, ok := expectedSet[id]; !ok {
			return fmt.Errorf("unexpected source id %q; source changed, re-freeze required", id)
		}
	}
	if len(actual) != mapping.ExpectedCount {
		return fmt.Errorf("source census count %d differs from reviewed count %d", len(actual), mapping.ExpectedCount)
	}
	return nil
}

func exportTable(ctx context.Context, db queryer, name string, columns []Column, ids []string) (Table, error) {
	writable := make([]Column, 0, len(columns))
	for _, column := range columns {
		if !column.Generated {
			writable = append(writable, column)
		}
	}
	if len(writable) == 0 {
		return Table{}, fmt.Errorf("table %s has no writable columns", name)
	}

	quotedColumns := make([]string, 0, len(writable))
	for _, column := range writable {
		quotedColumns = append(quotedColumns, quoteIdentifier(column.Name))
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id)
	}

	var predicate string
	switch name {
	case "issues":
		predicate = "`id` IN (" + placeholders + ")"
	case "dependencies":
		predicate = "`issue_id` IN (" + placeholders + ") OR `depends_on_issue_id` IN (" + placeholders + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	default:
		predicate = "`issue_id` IN (" + placeholders + ")"
	}
	query := "SELECT " + strings.Join(quotedColumns, ",") + " FROM " + quoteIdentifier(name) + " WHERE " + predicate
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return Table{}, fmt.Errorf("export table %s: %w", name, err)
	}
	defer rows.Close()

	table := Table{Name: name, Columns: writable}
	for rows.Next() {
		values := make([]any, len(writable))
		dest := make([]any, len(writable))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return Table{}, fmt.Errorf("scan table %s: %w", name, err)
		}
		row := Row{Cells: make([]Cell, len(values))}
		for i, value := range values {
			row.Cells[i] = normalizeCell(value)
		}
		table.Rows = append(table.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Table{}, fmt.Errorf("iterate table %s: %w", name, err)
	}
	sort.Slice(table.Rows, func(i, j int) bool {
		return rowSortKey(table.Rows[i]) < rowSortKey(table.Rows[j])
	})
	return table, nil
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func normalizeCell(value any) Cell {
	switch typed := value.(type) {
	case nil:
		return Cell{Null: true}
	case []byte:
		return Cell{Text: string(typed)}
	case time.Time:
		return Cell{Text: typed.UTC().Format(time.RFC3339Nano)}
	default:
		return Cell{Text: fmt.Sprint(typed)}
	}
}

func rowSortKey(row Row) string {
	parts := make([]string, len(row.Cells))
	for i, cell := range row.Cells {
		if cell.Null {
			parts[i] = "0:"
		} else {
			parts[i] = "1:" + cell.Text
		}
	}
	return strings.Join(parts, "\x00")
}
