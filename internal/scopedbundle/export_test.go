package scopedbundle

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestExportRejectsStaleManifestWhenSourceCensusHasExtraID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mapping := syntheticResearchMapping()
	mapping.Pairs = append(mapping.Pairs, IDPair{Source: "source-Campaigns-046", Target: "target-Campaigns-046"})
	mapping.ExpectedCount = 18
	if err := mapping.Seal(); err != nil {
		t.Fatal(err)
	}
	expectV53Schema(t, mock)

	census := sqlmock.NewRows([]string{"id"})
	for _, pair := range mapping.Pairs {
		census.AddRow(pair.Source)
	}
	census.AddRow("source-Campaigns-047")
	mock.ExpectQuery(regexp.QuoteMeta(sourceCensusSQL)).
		WithArgs(mapping.SourcePrefix + "%").
		WillReturnRows(census)

	_, err = Export(context.Background(), db, mapping)
	if err == nil || !strings.Contains(err.Error(), "unexpected source id") {
		t.Fatalf("stale manifest error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExportPreservesExactRowsAcrossFiveTables(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mapping := syntheticResearchMapping()
	expectV53Schema(t, mock)
	expectExactCensus(mock, mapping)

	mock.ExpectQuery("SELECT .* FROM `issues` WHERE `id` IN").
		WillReturnRows(issueRows(mapping))
	mock.ExpectQuery("SELECT .* FROM `labels` WHERE `issue_id` IN").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "label"}).
			AddRow(mapping.Pairs[0].Source, "priority:p1"))
	mock.ExpectQuery("SELECT .* FROM `comments` WHERE `issue_id` IN").
		WillReturnRows(sqlmock.NewRows([]string{"id", "issue_id", "author", "text", "created_at"}).
			AddRow("comment-1", mapping.Pairs[0].Source, "original-author", "exact text", "2026-09-05 12:00:00"))
	mock.ExpectQuery("SELECT .* FROM `dependencies` WHERE `issue_id` IN .* OR `depends_on_issue_id` IN").
		WillReturnRows(sqlmock.NewRows([]string{"id", "issue_id", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"}).
			AddRow("dep-1", mapping.Pairs[0].Source, mapping.Pairs[1].Source, nil, nil))
	mock.ExpectQuery("SELECT .* FROM `events` WHERE `issue_id` IN").
		WillReturnRows(sqlmock.NewRows([]string{"id", "issue_id", "event_type", "actor", "created_at"}).
			AddRow("event-1", mapping.Pairs[0].Source, "updated", "original-actor", "2026-09-05 12:01:00"))

	bundle, err := Export(context.Background(), db, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Verify(); err != nil {
		t.Fatalf("bundle verification: %v", err)
	}
	for _, name := range transferredTables {
		_ = tableByName(t, bundle.Tables, name)
	}
	comments := tableByName(t, bundle.Tables, "comments")
	if got := rowValue(t, comments, 0, "author"); got != "original-author" {
		t.Fatalf("comment author = %q", got)
	}
	if got := rowValue(t, comments, 0, "id"); got != "comment-1" {
		t.Fatalf("comment id = %q", got)
	}
	if got := rowValue(t, comments, 0, "text"); got != "exact text" {
		t.Fatalf("comment text = %q", got)
	}
	if got := rowValue(t, comments, 0, "created_at"); got != "2026-09-05 12:00:00" {
		t.Fatalf("comment timestamp = %q", got)
	}
	events := tableByName(t, bundle.Tables, "events")
	if got := rowValue(t, events, 0, "actor"); got != "original-actor" {
		t.Fatalf("event actor = %q", got)
	}
	if got := rowValue(t, events, 0, "id"); got != "event-1" {
		t.Fatalf("event id = %q", got)
	}
	if got := rowValue(t, events, 0, "event_type"); got != "updated" {
		t.Fatalf("event type = %q", got)
	}
	if got := rowValue(t, events, 0, "created_at"); got != "2026-09-05 12:01:00" {
		t.Fatalf("event timestamp = %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectV53Schema(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta(schemaVersionSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(53))
	rows := sqlmock.NewRows([]string{
		"table_name", "column_name", "column_type", "is_nullable",
		"column_default", "extra", "generation_expression",
	})
	add := func(table, name, typ, nullable string) {
		rows.AddRow(table, name, typ, nullable, nil, "", "")
	}
	add("comments", "id", "varchar(64)", "NO")
	add("comments", "issue_id", "varchar(64)", "NO")
	add("comments", "author", "varchar(255)", "NO")
	add("comments", "text", "text", "NO")
	add("comments", "created_at", "datetime", "NO")
	add("dependencies", "id", "varchar(64)", "NO")
	add("dependencies", "issue_id", "varchar(64)", "NO")
	add("dependencies", "depends_on_issue_id", "varchar(64)", "YES")
	add("dependencies", "depends_on_wisp_id", "varchar(64)", "YES")
	add("dependencies", "depends_on_external", "varchar(255)", "YES")
	add("events", "id", "varchar(64)", "NO")
	add("events", "issue_id", "varchar(64)", "NO")
	add("events", "event_type", "varchar(64)", "NO")
	add("events", "actor", "varchar(255)", "NO")
	add("events", "created_at", "datetime", "NO")
	add("issues", "id", "varchar(64)", "NO")
	add("issues", "title", "varchar(500)", "NO")
	add("labels", "issue_id", "varchar(64)", "NO")
	add("labels", "label", "varchar(255)", "NO")
	mock.ExpectQuery(regexp.QuoteMeta(informationSchemaSQL)).WillReturnRows(rows)
}

func expectExactCensus(mock sqlmock.Sqlmock, mapping Mapping) {
	rows := sqlmock.NewRows([]string{"id"})
	for _, pair := range mapping.Pairs {
		rows.AddRow(pair.Source)
	}
	mock.ExpectQuery(regexp.QuoteMeta(sourceCensusSQL)).
		WithArgs(mapping.SourcePrefix + "%").
		WillReturnRows(rows)
}

func issueRows(mapping Mapping) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "title"})
	for _, pair := range mapping.Pairs {
		rows.AddRow(pair.Source, "synthetic "+pair.Source)
	}
	return rows
}
