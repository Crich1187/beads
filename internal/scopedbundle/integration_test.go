//go:build scopedbundle_integration

package scopedbundle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/beads/internal/storage/schema"
)

func TestPrivateDoltV53ExactRecoveryAndAtomicity(t *testing.T) {
	ctx := context.Background()
	source := newPrivateDatabase(t, "scoped_source_v53", 53)
	target := newPrivateDatabase(t, "scoped_target_v53", 53)
	mapping := syntheticResearchMapping()
	seedSyntheticSource(t, source, mapping)
	seedUnrelatedControl(t, target, "unrelated-before")

	bundle, err := Export(ctx, source, mapping)
	if err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.ExecContext(ctx,
		"INSERT INTO comments (id,issue_id,author,text,created_at) VALUES (?,?,?,?,?)",
		"00000000-0000-0000-0000-000000000001", "unrelated-control", "other-author", "collision", "2026-09-05 12:00:00"); err != nil {
		t.Fatal(err)
	}
	_, err = Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: before.SHA256})
	if err == nil || !strings.Contains(err.Error(), "ID collision") {
		t.Fatalf("global comment collision error = %v", err)
	}
	if got := scalarCount(t, target, "SELECT COUNT(*) FROM issues WHERE id LIKE 'target-%'"); got != 0 {
		t.Fatalf("collision preflight allowed %d scoped issue writes", got)
	}
	if _, err := target.ExecContext(ctx, "DELETE FROM comments WHERE id = ?", "00000000-0000-0000-0000-000000000001"); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: before.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("initial apply reported no change")
	}
	assertMappedEquality(t, target, *bundle)

	deleteScopedFixture(t, target, mapping)
	if _, err := target.ExecContext(ctx, "UPDATE issues SET title = ? WHERE id = ?", "unrelated-newer", "unrelated-control"); err != nil {
		t.Fatal(err)
	}
	postDelete, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: postDelete.SHA256}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	assertMappedEquality(t, target, *bundle)
	assertUnrelatedControl(t, target, "unrelated-newer")

	rerun, err := Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: postDelete.SHA256})
	if err != nil {
		t.Fatalf("exact rerun: %v", err)
	}
	if rerun.Changed {
		t.Fatal("exact rerun was not a no-op")
	}

	deleteScopedFixture(t, target, mapping)
	preFault, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	_, err = apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: preFault.SHA256}, func(*sql.Tx) error {
		return errors.New("synthetic mid-transaction failure")
	})
	if err == nil || !strings.Contains(err.Error(), "injected apply failure") {
		t.Fatalf("induced failure error = %v", err)
	}
	postFault, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if postFault.SHA256 != preFault.SHA256 {
		t.Fatalf("partial failure escaped transaction: %s != %s", postFault.SHA256, preFault.SHA256)
	}
	assertUnrelatedControl(t, target, "unrelated-newer")
}

func TestPrivateDoltCurrentJournalIsAtomicAndRerunSafe(t *testing.T) {
	ctx := context.Background()
	source := newPrivateDatabase(t, "scoped_source_current", 0)
	target := newPrivateDatabase(t, "scoped_target_current", 0)
	mapping := syntheticResearchMapping()
	seedSyntheticSource(t, source, mapping)

	bundle, err := Export(ctx, source, mapping)
	if err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := materializeDesired(*bundle, before.Schema)
	if err != nil {
		t.Fatal(err)
	}
	comments, _ := findTable(desired, "comments")
	if len(comments.Rows) != 1 || cellText(comments, comments.Rows[0], "created_at") == "" {
		t.Fatalf("materialized current comment timestamp missing: rows=%d has_column=%t", len(comments.Rows), hasColumn(comments.Columns, "created_at"))
	}
	if _, err := Apply(ctx, target, *bundle, ApplyOptions{
		ExpectedCurrentSHA256: before.SHA256,
		Actor:                 "scoped-bundle-test",
		JournalEnabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	journalCount := scalarCount(t, target, "SELECT COUNT(*) FROM bd_events_journal")
	if journalCount == 0 {
		t.Fatal("journal-enabled apply created no durable journal rows")
	}
	if _, err := Apply(ctx, target, *bundle, ApplyOptions{
		ExpectedCurrentSHA256: before.SHA256,
		Actor:                 "scoped-bundle-test",
		JournalEnabled:        true,
	}); err != nil {
		t.Fatalf("journal rerun: %v", err)
	}
	if got := scalarCount(t, target, "SELECT COUNT(*) FROM bd_events_journal"); got != journalCount {
		t.Fatalf("rerun journal count = %d want %d", got, journalCount)
	}
	assertMappedEquality(t, target, *bundle)
}

func TestPrivateDoltRejectsStaleExpectedDigestWithoutWrites(t *testing.T) {
	ctx := context.Background()
	source := newPrivateDatabase(t, "scoped_source_stale_digest", 53)
	target := newPrivateDatabase(t, "scoped_target_stale_digest", 53)
	mapping := syntheticResearchMapping()
	seedSyntheticSource(t, source, mapping)
	seedUnrelatedControl(t, target, "unrelated-before-stale-check")

	bundle, err := Export(ctx, source, mapping)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: empty.SHA256}); err != nil {
		t.Fatal(err)
	}
	approved, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.ExecContext(ctx, "UPDATE issues SET title = ? WHERE id = ?", "unreviewed target drift", mapping.Pairs[0].Target); err != nil {
		t.Fatal(err)
	}
	drifted, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if drifted.SHA256 == approved.SHA256 {
		t.Fatal("synthetic target drift did not change the scoped state digest")
	}
	fullBefore := fullFiveTableDigest(t, target)

	_, err = Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: approved.SHA256})
	if err == nil || !strings.Contains(err.Error(), "expected current SHA-256") {
		t.Fatalf("stale expected-current error = %v", err)
	}
	after, inspectErr := Inspect(ctx, target, mapping, TargetSide)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.SHA256 != drifted.SHA256 {
		t.Fatalf("stale digest refusal changed scoped rows: got %s want %s", after.SHA256, drifted.SHA256)
	}
	if fullAfter := fullFiveTableDigest(t, target); fullAfter != fullBefore {
		t.Fatalf("stale digest refusal changed database rows: got %s want %s", fullAfter, fullBefore)
	}
	assertUnrelatedControl(t, target, "unrelated-before-stale-check")
}

func TestPrivateDoltRejectsUnreviewedCommentAndEventHistoryWithoutWrites(t *testing.T) {
	tests := []struct {
		name       string
		table      string
		mutate     func(*testing.T, *sql.DB, Mapping)
		wantReason string
	}{
		{
			name:  "destination-only comment",
			table: "comments",
			mutate: func(t *testing.T, db *sql.DB, mapping Mapping) {
				_, err := db.Exec(`INSERT INTO comments (id,issue_id,author,text,created_at) VALUES (?,?,?,?,?)`,
					"10000000-0000-0000-0000-000000000001", mapping.Pairs[0].Target,
					"destination-author", "destination-only comment", "2026-09-05 12:01:00")
				if err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "destination-only comments row",
		},
		{
			name:  "content-colliding comment",
			table: "comments",
			mutate: func(t *testing.T, db *sql.DB, _ Mapping) {
				if _, err := db.Exec("UPDATE comments SET text = ? WHERE id = ?", "colliding comment", "00000000-0000-0000-0000-000000000001"); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "destination comments row",
		},
		{
			name:  "destination-only event",
			table: "events",
			mutate: func(t *testing.T, db *sql.DB, mapping Mapping) {
				_, err := db.Exec(`INSERT INTO events (id,issue_id,event_type,actor,old_value,new_value,comment,created_at) VALUES (?,?,?,?,?,?,?,?)`,
					"10000000-0000-0000-0000-000000000003", mapping.Pairs[0].Target,
					"updated", "destination-actor", "before", "after", "destination-only event", "2026-09-05 12:01:00")
				if err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "destination-only events row",
		},
		{
			name:  "content-colliding event",
			table: "events",
			mutate: func(t *testing.T, db *sql.DB, _ Mapping) {
				if _, err := db.Exec("UPDATE events SET comment = ? WHERE id = ?", "colliding event", "00000000-0000-0000-0000-000000000003"); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "destination events row",
		},
	}

	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			source := newPrivateDatabase(t, fmt.Sprintf("scoped_source_history_%d", index), 53)
			target := newPrivateDatabase(t, fmt.Sprintf("scoped_target_history_%d", index), 53)
			mapping := syntheticResearchMapping()
			seedSyntheticSource(t, source, mapping)
			seedUnrelatedControl(t, target, "unrelated-before-history-check")

			bundle, err := Export(ctx, source, mapping)
			if err != nil {
				t.Fatal(err)
			}
			empty, err := Inspect(ctx, target, mapping, TargetSide)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: empty.SHA256}); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, target, mapping)
			before, err := Inspect(ctx, target, mapping, TargetSide)
			if err != nil {
				t.Fatal(err)
			}
			rowCountBefore := scalarCount(t, target, "SELECT COUNT(*) FROM "+quoteIdentifier(tc.table))
			fullBefore := fullFiveTableDigest(t, target)

			_, err = Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: before.SHA256})
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("immutable history error = %v", err)
			}
			after, inspectErr := Inspect(ctx, target, mapping, TargetSide)
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			if after.SHA256 != before.SHA256 {
				t.Fatalf("history refusal changed scoped rows: got %s want %s", after.SHA256, before.SHA256)
			}
			if got := scalarCount(t, target, "SELECT COUNT(*) FROM "+quoteIdentifier(tc.table)); got != rowCountBefore {
				t.Fatalf("history refusal changed %s row count: got %d want %d", tc.table, got, rowCountBefore)
			}
			if fullAfter := fullFiveTableDigest(t, target); fullAfter != fullBefore {
				t.Fatalf("history refusal changed database rows: got %s want %s", fullAfter, fullBefore)
			}
			assertUnrelatedControl(t, target, "unrelated-before-history-check")
		})
	}
}

func TestPrivateDoltPostconditionMismatchRollsBackEveryWrite(t *testing.T) {
	ctx := context.Background()
	source := newPrivateDatabase(t, "scoped_source_postcondition", 53)
	target := newPrivateDatabase(t, "scoped_target_postcondition", 53)
	mapping := syntheticResearchMapping()
	seedSyntheticSource(t, source, mapping)
	seedUnrelatedControl(t, target, "unrelated-before-postcondition")

	bundle, err := Export(ctx, source, mapping)
	if err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	fullBefore := fullFiveTableDigest(t, target)
	_, err = apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: before.SHA256}, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE issues SET title = ? WHERE id = ?", "postcondition sabotage", mapping.Pairs[0].Target)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "postcondition digest mismatch") {
		t.Fatalf("postcondition mismatch error = %v", err)
	}
	after, inspectErr := Inspect(ctx, target, mapping, TargetSide)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.SHA256 != before.SHA256 {
		t.Fatalf("postcondition refusal changed scoped rows: got %s want %s", after.SHA256, before.SHA256)
	}
	if fullAfter := fullFiveTableDigest(t, target); fullAfter != fullBefore {
		t.Fatalf("postcondition refusal changed database rows: got %s want %s", fullAfter, fullBefore)
	}
	assertUnrelatedControl(t, target, "unrelated-before-postcondition")
}

func newPrivateDatabase(t *testing.T, name string, version int) *sql.DB {
	t.Helper()
	address := os.Getenv("SCOPED_BUNDLE_TEST_ADDR")
	if address == "" {
		t.Fatal("SCOPED_BUNDLE_TEST_ADDR is required by the isolated integration harness")
	}
	root, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/scoped_host?parseTime=true&multiStatements=true", address))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	if _, err := root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s?parseTime=true&multiStatements=true", address, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if version == 0 {
		if _, err := schema.MigrateUp(context.Background(), db); err != nil {
			t.Fatalf("migrate current scratch schema: %v", err)
		}
	} else {
		if _, err := schema.MigrateUpTo(context.Background(), db, version); err != nil {
			t.Fatalf("migrate scratch schema to v%d: %v", version, err)
		}
	}
	return db
}

func seedSyntheticSource(t *testing.T, db *sql.DB, mapping Mapping) {
	t.Helper()
	ctx := context.Background()
	when := "2026-09-05 12:00:00"
	for _, pair := range mapping.Pairs {
		_, err := db.ExecContext(ctx, `INSERT INTO issues
			(id,title,description,design,acceptance_criteria,notes,status,priority,issue_type,created_at,created_by,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			pair.Source, "title "+pair.Source, "description", "design", "acceptance", "notes",
			"open", 2, "task", when, "original-creator", when)
		if err != nil {
			t.Fatalf("seed issue %s: %v", pair.Source, err)
		}
	}
	first, second := mapping.Pairs[0].Source, mapping.Pairs[1].Source
	statements := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO labels (issue_id,label) VALUES (?,?)", []any{first, "priority:p1"}},
		{"INSERT INTO comments (id,issue_id,author,text,created_at) VALUES (?,?,?,?,?)", []any{"00000000-0000-0000-0000-000000000001", first, "original-author", "exact comment", when}},
		{"INSERT INTO dependencies (id,issue_id,depends_on_issue_id,type,created_at,created_by,metadata,thread_id) VALUES (?,?,?,?,?,?,?,?)", []any{"00000000-0000-0000-0000-000000000002", first, second, "blocks", when, "original-creator", `{}`, "thread-1"}},
		{"INSERT INTO events (id,issue_id,event_type,actor,old_value,new_value,comment,created_at) VALUES (?,?,?,?,?,?,?,?)", []any{"00000000-0000-0000-0000-000000000003", first, "updated", "original-actor", "old", "new", "exact event", when}},
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed child row: %v", err)
		}
	}
}

func seedUnrelatedControl(t *testing.T, db *sql.DB, title string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO issues
		(id,title,description,design,acceptance_criteria,notes,status,priority,issue_type,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"unrelated-control", title, "", "", "", "", "open", 2, "task", "2026-09-05 12:00:00", "2026-09-05 12:00:00")
	if err != nil {
		t.Fatal(err)
	}
}

func deleteScopedFixture(t *testing.T, db *sql.DB, mapping Mapping) {
	t.Helper()
	ids, err := mapping.TargetIDs()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	query := "DELETE FROM issues WHERE id IN (" + placeholders(len(ids)) + ")"
	if _, err := tx.Exec(query, stringsToAny(ids)...); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func assertMappedEquality(t *testing.T, db *sql.DB, bundle Bundle) {
	t.Helper()
	state, err := Inspect(context.Background(), db, bundle.Mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := materializeDesired(bundle, state.Schema)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := digestTables(desired)
	if err != nil {
		t.Fatal(err)
	}
	if state.SHA256 != digest {
		t.Fatalf("restored state digest = %s want %s", state.SHA256, digest)
	}
}

func assertUnrelatedControl(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var title string
	if err := db.QueryRow("SELECT title FROM issues WHERE id = ?", "unrelated-control").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != want {
		t.Fatalf("unrelated control title = %q want %q", title, want)
	}
}

func scalarCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func fullFiveTableDigest(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	schemaState, err := InspectSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	tables := make([]Table, 0, len(transferredTables))
	for _, name := range transferredTables {
		columns := make([]Column, 0, len(schemaState.Tables[name]))
		quoted := make([]string, 0, len(schemaState.Tables[name]))
		for _, column := range schemaState.Tables[name] {
			if column.Generated {
				continue
			}
			columns = append(columns, column)
			quoted = append(quoted, quoteIdentifier(column.Name))
		}
		rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(quoted, ",")+" FROM "+quoteIdentifier(name))
		if err != nil {
			t.Fatal(err)
		}
		table := Table{Name: name, Columns: columns}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			row := Row{Cells: make([]Cell, len(values))}
			for i, value := range values {
				row.Cells[i] = normalizeCell(value)
			}
			table.Rows = append(table.Rows, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	digest, err := digestTables(tables)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
