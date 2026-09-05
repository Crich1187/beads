package scopedbundle

import (
	"strings"
	"testing"
)

func TestCheckCompatibilityAllowsOnlyRepresentableSchemaDifferences(t *testing.T) {
	bundle := minimalBundle(t)
	legacyDefault := ""
	issues := tablePointer(t, &bundle, "issues")
	issues.Columns = append(issues.Columns, Column{
		Name: "legacy_note", SQLType: "text", Nullable: true, Default: &legacyDefault,
	})
	for i := range issues.Rows {
		issues.Rows[i].Cells = append(issues.Rows[i].Cells, nullCell())
	}
	target := schemaFromBundle(bundle)
	target.Tables["issues"] = target.Tables["issues"][:2]

	if err := CheckCompatibility(bundle, target); err != nil {
		t.Fatalf("default/null source-only column should be representable: %v", err)
	}

	issues.Rows[0].Cells[2] = textCell("non-default history")
	if err := CheckCompatibility(bundle, target); err == nil || !strings.Contains(err.Error(), "non-default") {
		t.Fatalf("source-only non-default error = %v", err)
	}
}

func TestCheckCompatibilityRejectsRequiredTargetOnlyAndTypeMismatch(t *testing.T) {
	bundle := minimalBundle(t)
	target := schemaFromBundle(bundle)
	target.Tables["issues"] = append(target.Tables["issues"], Column{
		Name: "required_new", SQLType: "varchar(64)", Nullable: false,
	})
	if err := CheckCompatibility(bundle, target); err == nil || !strings.Contains(err.Error(), "target-only") {
		t.Fatalf("required target-only error = %v", err)
	}

	target = schemaFromBundle(bundle)
	target.Tables["issues"][1].SQLType = "bigint"
	if err := CheckCompatibility(bundle, target); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("type mismatch error = %v", err)
	}
}

func schemaFromBundle(bundle Bundle) Schema {
	tables := make(map[string][]Column, len(bundle.Tables))
	for _, table := range bundle.Tables {
		tables[table.Name] = append([]Column(nil), table.Columns...)
	}
	return Schema{Version: bundle.SourceSchema, Tables: tables}
}

func tablePointer(t *testing.T, bundle *Bundle, name string) *Table {
	t.Helper()
	for i := range bundle.Tables {
		if bundle.Tables[i].Name == name {
			return &bundle.Tables[i]
		}
	}
	t.Fatalf("table %q not found", name)
	return nil
}
