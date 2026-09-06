package scopedbundle

import (
	"strings"
	"testing"
)

func sealedManifest(t *testing.T, mutate func(*ReconcileManifest)) ReconcileManifest {
	t.Helper()
	m := ReconcileManifest{
		Format:               ReconcileManifestFormat,
		Version:              ReconcileManifestVersion,
		ExpectedSourceSHA256: strings.Repeat("a", 64),
		ExpectedTargetSHA256: strings.Repeat("b", 64),
	}
	if mutate != nil {
		mutate(&m)
	}
	if err := m.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return m
}

func TestReconcileManifestSealIsDeterministicAndOrderIndependent(t *testing.T) {
	a := sealedManifest(t, func(m *ReconcileManifest) {
		m.RetainTargetEventIDs = []string{"e3", "e1", "e2"}
		m.CommentLinks = []CommentLink{{TargetID: "t2", SourceID: "s2"}, {TargetID: "t1", SourceID: "s1"}}
	})
	b := sealedManifest(t, func(m *ReconcileManifest) {
		m.RetainTargetEventIDs = []string{"e2", "e3", "e1"}
		m.CommentLinks = []CommentLink{{TargetID: "t1", SourceID: "s1"}, {TargetID: "t2", SourceID: "s2"}}
	})
	if a.SHA256 != b.SHA256 {
		t.Fatalf("input order changed the seal: %s vs %s", a.SHA256, b.SHA256)
	}
}

func TestReconcileManifestDetectsTampering(t *testing.T) {
	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.RetainTargetEventIDs = []string{"e1"}
	})
	if err := m.Verify(); err != nil {
		t.Fatalf("sealed manifest should verify: %v", err)
	}
	m.RetainTargetEventIDs = append(m.RetainTargetEventIDs, "e2-smuggled")
	if err := m.Verify(); err == nil {
		t.Fatal("tampered manifest verified; the seal is not load-bearing")
	}
}

func TestReconcileManifestRejectsMalformedShapes(t *testing.T) {
	cases := []struct {
		name string
		want string
		m    ReconcileManifest
	}{
		{
			name: "wrong format",
			want: "unsupported reconcile manifest",
			m:    ReconcileManifest{Format: "nope", Version: 1, ExpectedSourceSHA256: strings.Repeat("a", 64), ExpectedTargetSHA256: strings.Repeat("b", 64)},
		},
		{
			name: "short source digest",
			want: "expected_source_sha256",
			m:    ReconcileManifest{Format: ReconcileManifestFormat, Version: 1, ExpectedSourceSHA256: "abc", ExpectedTargetSHA256: strings.Repeat("b", 64)},
		},
		{
			name: "table outside contract",
			want: "outside the five-table contract",
			m: ReconcileManifest{Format: ReconcileManifestFormat, Version: 1,
				ExpectedSourceSHA256: strings.Repeat("a", 64), ExpectedTargetSHA256: strings.Repeat("b", 64),
				SchemaAdditions: []SchemaAddition{{Table: "wisps", Column: "x", SQLType: "bigint", Nullable: true}}},
		},
		{
			name: "not-null addition without default",
			want: "needs an explicit default",
			m: ReconcileManifest{Format: ReconcileManifestFormat, Version: 1,
				ExpectedSourceSHA256: strings.Repeat("a", 64), ExpectedTargetSHA256: strings.Repeat("b", 64),
				SchemaAdditions: []SchemaAddition{{Table: "issues", Column: "row_lock", SQLType: "bigint", Nullable: false}}},
		},
		{
			name: "comment linked twice",
			want: "linked more than once",
			m: ReconcileManifest{Format: ReconcileManifestFormat, Version: 1,
				ExpectedSourceSHA256: strings.Repeat("a", 64), ExpectedTargetSHA256: strings.Repeat("b", 64),
				CommentLinks: []CommentLink{{TargetID: "t1", SourceID: "s1"}, {TargetID: "t1", SourceID: "s2"}}},
		},
		{
			name: "comment both linked and destination-only",
			want: "both linked and retained",
			m: ReconcileManifest{Format: ReconcileManifestFormat, Version: 1,
				ExpectedSourceSHA256: strings.Repeat("a", 64), ExpectedTargetSHA256: strings.Repeat("b", 64),
				CommentLinks:           []CommentLink{{TargetID: "t1", SourceID: "s1"}},
				RetainTargetCommentIDs: []string{"t1"}},
		},
		{
			name: "duplicate retained event",
			want: "duplicate",
			m: ReconcileManifest{Format: ReconcileManifestFormat, Version: 1,
				ExpectedSourceSHA256: strings.Repeat("a", 64), ExpectedTargetSHA256: strings.Repeat("b", 64),
				RetainTargetEventIDs: []string{"e1", "e1"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.m.Digest(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// --- schema addition planning -------------------------------------------------

func rowLockBundle(t *testing.T, values []Cell) Bundle {
	t.Helper()
	b := minimalBundle(t)
	issues, ok := findTable(b.Tables, "issues")
	if !ok {
		t.Fatal("fixture has no issues table")
	}
	issues.Columns = append(issues.Columns, Column{Name: "row_lock", SQLType: "bigint"})
	for i := range issues.Rows {
		cell := Cell{Text: "0"}
		if i < len(values) {
			cell = values[i]
		}
		issues.Rows[i].Cells = append(issues.Rows[i].Cells, cell)
	}
	for i := range b.Tables {
		if b.Tables[i].Name == "issues" {
			b.Tables[i] = issues
		}
	}
	if err := b.Seal(); err != nil {
		t.Fatalf("seal bundle: %v", err)
	}
	return b
}

func targetSchemaWithoutRowLock(b Bundle) Schema {
	s := Schema{Version: 66, Tables: map[string][]Column{}}
	for _, table := range b.Tables {
		cols := make([]Column, 0, len(table.Columns))
		for _, c := range table.Columns {
			if table.Name == "issues" && c.Name == "row_lock" {
				continue
			}
			cols = append(cols, c)
		}
		s.Tables[table.Name] = cols
	}
	return s
}

func TestPlanSchemaAdditionsEmitsOnlyReviewedAddColumn(t *testing.T) {
	b := rowLockBundle(t, []Cell{{Text: "7"}})
	target := targetSchemaWithoutRowLock(b)
	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.SchemaAdditions = []SchemaAddition{{Table: "issues", Column: "row_lock", SQLType: "bigint", Nullable: false, Default: "0"}}
	})

	stmts, err := PlanSchemaAdditions(b, target, m)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(stmts) != 1 {
		t.Fatalf("statements = %v, want exactly one", stmts)
	}
	got := stmts[0]
	for _, want := range []string{"ALTER TABLE `issues`", "ADD COLUMN `row_lock`", "bigint", "NOT NULL DEFAULT 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("statement %q missing %q", got, want)
		}
	}
	for _, forbidden := range []string{"DROP", "RENAME", "MODIFY", "TRUNCATE", "DELETE"} {
		if strings.Contains(strings.ToUpper(got), forbidden) {
			t.Errorf("statement %q contains forbidden verb %s", got, forbidden)
		}
	}
}

// This is the control that must NOT be weakened: a source-only column carrying
// real values, with no reviewed addition covering it, is still refused.
func TestPlanSchemaAdditionsStillRejectsUncoveredNonDefaultColumn(t *testing.T) {
	b := rowLockBundle(t, []Cell{{Text: "7"}})
	target := targetSchemaWithoutRowLock(b)
	m := sealedManifest(t, nil) // no schema additions at all

	_, err := PlanSchemaAdditions(b, target, m)
	if err == nil || !strings.Contains(err.Error(), "not covered by a reviewed schema addition") {
		t.Fatalf("error = %v, want refusal of the uncovered non-default column", err)
	}
}

func TestPlanSchemaAdditionsRejectsInventedColumn(t *testing.T) {
	b := rowLockBundle(t, []Cell{{Text: "0"}})
	target := targetSchemaWithoutRowLock(b)
	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.SchemaAdditions = []SchemaAddition{{Table: "issues", Column: "not_a_source_column", SQLType: "bigint", Nullable: true}}
	})
	_, err := PlanSchemaAdditions(b, target, m)
	if err == nil || !strings.Contains(err.Error(), "does not exist in the source") {
		t.Fatalf("error = %v, want refusal of an invented column", err)
	}
}

func TestPlanSchemaAdditionsRejectsColumnAlreadyPresent(t *testing.T) {
	b := rowLockBundle(t, []Cell{{Text: "0"}})
	target := Schema{Version: 66, Tables: map[string][]Column{}}
	for _, table := range b.Tables {
		target.Tables[table.Name] = table.Columns // row_lock already present
	}
	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.SchemaAdditions = []SchemaAddition{{Table: "issues", Column: "row_lock", SQLType: "bigint", Nullable: false, Default: "0"}}
	})
	_, err := PlanSchemaAdditions(b, target, m)
	if err == nil || !strings.Contains(err.Error(), "already exists in the target") {
		t.Fatalf("error = %v, want refusal when the column is already present", err)
	}
}

func TestPlanSchemaAdditionsRejectsUnsealedManifest(t *testing.T) {
	b := rowLockBundle(t, []Cell{{Text: "0"}})
	target := targetSchemaWithoutRowLock(b)
	m := sealedManifest(t, nil)
	m.SHA256 = "" // strip the seal
	if _, err := PlanSchemaAdditions(b, target, m); err == nil {
		t.Fatal("unsealed manifest was accepted")
	}
}

func TestApplySchemaAdditionsRefusesNonAdditiveStatement(t *testing.T) {
	err := ApplySchemaAdditions(t.Context(), nil, []string{"DROP TABLE `issues`"})
	if err == nil || !strings.Contains(err.Error(), "refusing non-additive schema statement") {
		t.Fatalf("error = %v, want refusal of a non-additive statement", err)
	}
}
