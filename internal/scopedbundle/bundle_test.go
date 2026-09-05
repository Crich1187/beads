package scopedbundle

import (
	"strings"
	"testing"
)

func textCell(value string) Cell { return Cell{Text: value} }
func nullCell() Cell             { return Cell{Null: true} }

func minimalBundle(t *testing.T) Bundle {
	t.Helper()
	m := syntheticResearchMapping()
	issueRows := make([]Row, 0, len(m.Pairs))
	for _, pair := range m.Pairs {
		title := strings.TrimPrefix(pair.Source, "source-")
		if pair.Source == "source-001" {
			title = "root"
		}
		if pair.Source == "source-Campaigns-045" {
			title = "campaign"
		}
		issueRows = append(issueRows, Row{Cells: []Cell{textCell(pair.Source), textCell(title)}})
	}
	return Bundle{
		Format:       BundleFormat,
		Version:      BundleVersion,
		SourceSchema: 53,
		Mapping:      m,
		Tables: []Table{
			{
				Name: "issues",
				Columns: []Column{
					{Name: "id", SQLType: "varchar", Nullable: false},
					{Name: "title", SQLType: "varchar", Nullable: false},
				},
				Rows: issueRows,
			},
			{
				Name: "dependencies",
				Columns: []Column{
					{Name: "id", SQLType: "char", Nullable: false},
					{Name: "issue_id", SQLType: "varchar", Nullable: false},
					{Name: "depends_on_issue_id", SQLType: "varchar", Nullable: true},
					{Name: "depends_on_wisp_id", SQLType: "varchar", Nullable: true},
					{Name: "depends_on_external", SQLType: "varchar", Nullable: true},
				},
				Rows: []Row{{Cells: []Cell{
					textCell("dep-1"), textCell("source-Campaigns-045"), textCell("source-001"), nullCell(), nullCell(),
				}}},
			},
			{
				Name: "labels",
				Columns: []Column{
					{Name: "issue_id", SQLType: "varchar", Nullable: false},
					{Name: "label", SQLType: "varchar", Nullable: false},
				},
			},
			{
				Name: "comments",
				Columns: []Column{
					{Name: "id", SQLType: "varchar", Nullable: false},
					{Name: "issue_id", SQLType: "varchar", Nullable: false},
				},
			},
			{
				Name: "events",
				Columns: []Column{
					{Name: "id", SQLType: "varchar", Nullable: false},
					{Name: "issue_id", SQLType: "varchar", Nullable: false},
				},
			},
		},
	}
}

func TestBundleSealIsDeterministicAndDetectsTampering(t *testing.T) {
	t.Parallel()

	a := minimalBundle(t)
	b := minimalBundle(t)
	b.Tables[0].Rows[0], b.Tables[0].Rows[len(b.Tables[0].Rows)-1] = b.Tables[0].Rows[len(b.Tables[0].Rows)-1], b.Tables[0].Rows[0]
	b.Tables[0], b.Tables[1] = b.Tables[1], b.Tables[0]

	if err := a.Seal(); err != nil {
		t.Fatalf("Seal(a): %v", err)
	}
	if err := b.Seal(); err != nil {
		t.Fatalf("Seal(b): %v", err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatalf("bundle digest depends on table/row order: %s != %s", a.SHA256, b.SHA256)
	}
	if a.SourceStateSHA256 == "" || a.DesiredStateSHA256 == "" {
		t.Fatal("Seal() did not populate state digests")
	}
	if err := a.Verify(); err != nil {
		t.Fatalf("Verify(sealed): %v", err)
	}

	a.Tables[0].Rows[0].Cells[1].Text = "tampered"
	if err := a.Verify(); err == nil || !strings.Contains(err.Error(), "bundle digest mismatch") {
		t.Fatalf("Verify(tampered) error = %v", err)
	}
}

func TestBundleRejectsTablesOutsideTheFiveTableContract(t *testing.T) {
	bundle := minimalBundle(t)
	bundle.Tables = append(bundle.Tables, Table{Name: "config"})
	if err := bundle.Seal(); err == nil || !strings.Contains(err.Error(), "unexpected table") {
		t.Fatalf("unexpected table error = %v", err)
	}
}

func TestBundleRejectsEveryMissingRequiredTable(t *testing.T) {
	for _, missing := range transferredTables {
		t.Run(missing, func(t *testing.T) {
			bundle := minimalBundle(t)
			kept := make([]Table, 0, len(bundle.Tables)-1)
			for _, table := range bundle.Tables {
				if table.Name != missing {
					kept = append(kept, table)
				}
			}
			bundle.Tables = kept

			err := bundle.Seal()
			if err == nil || !strings.Contains(err.Error(), "bundle has no "+missing+" table") {
				t.Fatalf("missing %s error = %v", missing, err)
			}
		})
	}
}

func TestBundleMappedTablesRewriteOnlyReferenceColumns(t *testing.T) {
	t.Parallel()

	b := minimalBundle(t)
	mapped, err := b.MappedTables()
	if err != nil {
		t.Fatalf("MappedTables: %v", err)
	}

	issues := tableByName(t, mapped, "issues")
	if got := rowValue(t, issues, 0, "id"); got != "target-001" {
		t.Fatalf("first mapped issue ID = %q", got)
	}
	if got := rowValue(t, issues, 0, "title"); got != "root" {
		t.Fatalf("ordinary content was rewritten: %q", got)
	}

	deps := tableByName(t, mapped, "dependencies")
	if got := rowValue(t, deps, 0, "issue_id"); got != "target-Campaigns-045" {
		t.Fatalf("mapped dependency source = %q", got)
	}
	if got := rowValue(t, deps, 0, "depends_on_issue_id"); got != "target-001" {
		t.Fatalf("mapped dependency target = %q", got)
	}
}

func TestBundleMappedTablesRejectUnrepresentableDependencyTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		column string
		value  string
	}{
		{name: "unmapped issue", column: "depends_on_issue_id", value: "source-outside"},
		{name: "wisp target", column: "depends_on_wisp_id", value: "wisp-1"},
		{name: "external target", column: "depends_on_external", value: "external-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := minimalBundle(t)
			deps := &b.Tables[1]
			setRowValue(t, deps, 0, tc.column, Cell{Text: tc.value})
			if tc.column != "depends_on_issue_id" {
				setRowValue(t, deps, 0, "depends_on_issue_id", nullCell())
			}
			_, err := b.MappedTables()
			if err == nil || !strings.Contains(err.Error(), "closed-set") {
				t.Fatalf("MappedTables() error = %v", err)
			}
		})
	}
}

func tableByName(t *testing.T, tables []Table, name string) Table {
	t.Helper()
	for _, table := range tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("table %q not found", name)
	return Table{}
}

func columnIndex(t *testing.T, table Table, name string) int {
	t.Helper()
	for i, column := range table.Columns {
		if column.Name == name {
			return i
		}
	}
	t.Fatalf("column %s.%s not found", table.Name, name)
	return -1
}

func rowValue(t *testing.T, table Table, row int, column string) string {
	t.Helper()
	return table.Rows[row].Cells[columnIndex(t, table, column)].Text
}

func setRowValue(t *testing.T, table *Table, row int, column string, value Cell) {
	t.Helper()
	table.Rows[row].Cells[columnIndex(t, *table, column)] = value
}
