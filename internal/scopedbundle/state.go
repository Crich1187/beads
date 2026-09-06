package scopedbundle

import (
	"context"
	"fmt"
	"strings"
)

// IDSide selects which half of an exact mapping a state inspection reads.
type IDSide string

const (
	SourceSide IDSide = "source"
	TargetSide IDSide = "target"
)

// State is a deterministic snapshot of the five scoped tables.
type State struct {
	Schema Schema
	Tables []Table
	SHA256 string
}

// Inspect reads a scoped state without mutation. A namespace ID not named by
// the mapping is always an error. Missing IDs are allowed on the target side so
// a post-delete state can be fingerprinted for a guarded recovery.
func Inspect(ctx context.Context, db queryer, mapping Mapping, side IDSide) (State, error) {
	if err := mapping.Validate(); err != nil {
		return State{}, fmt.Errorf("mapping: %w", err)
	}
	schema, err := InspectSchema(ctx, db)
	if err != nil {
		return State{}, err
	}
	return inspectWithSchema(ctx, db, mapping, side, schema)
}

func inspectWithSchema(ctx context.Context, db queryer, mapping Mapping, side IDSide, schema Schema) (State, error) {
	var prefix string
	var ids []string
	var allowMissing bool
	var err error
	switch side {
	case SourceSide:
		prefix = mapping.SourcePrefix
		ids, err = mapping.SourceIDs()
	case TargetSide:
		prefix = mapping.TargetPrefix
		ids, err = mapping.TargetIDs()
		allowMissing = true
	default:
		return State{}, fmt.Errorf("unsupported ID side %q", side)
	}
	if err != nil {
		return State{}, err
	}
	if err := verifyNamespaceCensus(ctx, db, prefix, ids, allowMissing); err != nil {
		return State{}, err
	}

	tables := make([]Table, 0, len(transferredTables))
	for _, name := range transferredTables {
		table, err := exportTable(ctx, db, name, schema.Tables[name], ids)
		if err != nil {
			return State{}, err
		}
		tables = append(tables, table)
	}
	if side == TargetSide {
		if err := validateTargetClosedSet(tables, mapping); err != nil {
			return State{}, err
		}
	}
	digest, err := digestTables(tables)
	if err != nil {
		return State{}, err
	}
	return State{Schema: schema, Tables: tables, SHA256: digest}, nil
}

func verifyNamespaceCensus(ctx context.Context, db queryer, prefix string, expected []string, allowMissing bool) error {
	rows, err := db.QueryContext(ctx, sourceCensusSQL, prefix+"%")
	if err != nil {
		return fmt.Errorf("read namespace census: %w", err)
	}
	defer rows.Close()
	actual := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan namespace census: %w", err)
		}
		actual[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate namespace census: %w", err)
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		expectedSet[id] = struct{}{}
		if !allowMissing {
			if _, ok := actual[id]; !ok {
				return fmt.Errorf("missing source id %q; source changed, re-freeze required", id)
			}
		}
	}
	for id := range actual {
		if _, ok := expectedSet[id]; !ok {
			return fmt.Errorf("unexpected namespace id %q; source changed, re-freeze required", id)
		}
	}
	return nil
}

func validateTargetClosedSet(tables []Table, mapping Mapping) error {
	allowed := make(map[string]struct{}, mapping.ExpectedCount)
	ids, err := mapping.TargetIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	deps, ok := findTable(tables, "dependencies")
	if !ok {
		return fmt.Errorf("target dependencies table is missing")
	}
	for _, column := range []string{"issue_id", "depends_on_issue_id"} {
		index, ok := tableColumnIndex(deps, column)
		if !ok {
			return fmt.Errorf("target dependencies.%s is missing", column)
		}
		for _, row := range deps.Rows {
			cell := row.Cells[index]
			if cell.Null || cell.Text == "" {
				return fmt.Errorf("closed-set violation: target dependencies.%s is empty", column)
			}
			if _, ok := allowed[cell.Text]; !ok {
				return fmt.Errorf("closed-set violation: target dependency references %q", cell.Text)
			}
		}
	}
	for _, forbidden := range []string{"depends_on_wisp_id", "depends_on_external"} {
		index, ok := tableColumnIndex(deps, forbidden)
		if !ok {
			continue
		}
		for _, row := range deps.Rows {
			if cell := row.Cells[index]; !cell.Null && strings.TrimSpace(cell.Text) != "" {
				return fmt.Errorf("closed-set violation: target dependency has %s", forbidden)
			}
		}
	}
	return nil
}

func materializeDesired(bundle Bundle, target Schema) ([]Table, error) {
	if err := CheckCompatibility(bundle, target); err != nil {
		return nil, err
	}
	mapped, err := bundle.MappedTables()
	if err != nil {
		return nil, err
	}
	result := make([]Table, 0, len(transferredTables))
	for _, name := range transferredTables {
		source, ok := findTable(mapped, name)
		if !ok {
			return nil, fmt.Errorf("bundle table %s is missing", name)
		}
		sourceIndex := make(map[string]int, len(source.Columns))
		for i, column := range source.Columns {
			sourceIndex[column.Name] = i
		}
		columns := make([]Column, 0, len(target.Tables[name]))
		for _, column := range target.Tables[name] {
			if !column.Generated {
				columns = append(columns, column)
			}
		}
		table := Table{Name: name, Columns: columns, Rows: make([]Row, 0, len(source.Rows))}
		for _, sourceRow := range source.Rows {
			row := Row{Cells: make([]Cell, len(columns))}
			for i, targetColumn := range columns {
				if index, ok := sourceIndex[targetColumn.Name]; ok {
					row.Cells[i] = sourceRow.Cells[index]
					continue
				}
				cell, err := deterministicTargetDefault(targetColumn)
				if err != nil {
					return nil, fmt.Errorf("target-only column %s.%s: %w", name, targetColumn.Name, err)
				}
				row.Cells[i] = cell
			}
			table.Rows = append(table.Rows, row)
		}
		result = append(result, table)
	}
	return canonicalTables(result), nil
}

func deterministicTargetDefault(column Column) (Cell, error) {
	if column.Nullable {
		return Cell{Null: true}, nil
	}
	if column.Default == nil {
		return Cell{}, fmt.Errorf("required column has no default")
	}
	value := strings.TrimSpace(*column.Default)
	upper := strings.ToUpper(value)
	if value == "" {
		return Cell{Text: ""}, nil
	}
	if upper == "NULL" {
		return Cell{Null: true}, nil
	}
	if strings.ContainsAny(value, "()") || strings.Contains(upper, "CURRENT_") {
		return Cell{}, fmt.Errorf("dynamic default %q is not deterministically representable", value)
	}
	value = strings.Trim(value, "'")
	return Cell{Text: value}, nil
}
