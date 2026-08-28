//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/depid"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// Real-Dolt coverage for the merge-aware dependency-id re-key
// (rekeyDependencyTable, internal/storage/schema/dep_id_backfill.go) and its
// completion marker, ignored migration 0026 (gastownhall/beads#5268).
//
// Why this cannot be a sqlmock test. The bug IS Dolt's primary-key enforcement:
// the old row-at-a-time rewrite issued statements a statement-echo mock happily
// accepts, and only a real engine answers the second one with "duplicate primary
// key given" and leaves the table half-re-keyed. The same reason
// migrate_wisp_dep_forward_repair_test.go exists. These tests also exercise the
// marker end to end -- that a pending 0026 forces a pass on a database whose
// main cursor is already at latest, and that an aborted re-key leaves 0026
// unrecorded so the next open retries -- which is a property of MigrateUp's step
// order, not of any single statement.

// depRekeyMarkerVersion is ignored migration 0026, the clone-local marker whose
// pending state forces the one repair pass.
const depRekeyMarkerVersion = 26

// TestEmbeddedDepRekeyRepairsHalfRekeyedDuplicateEdge is the victim repair: a
// database that a pre-1.3.0 binary left half-re-keyed and then declared
// migrated, because the per-step commits had already carried the main cursor to
// latest before the re-key aborted.
//
// The seeded shape is the reporter's, minus the 5,336-row haystack: one logical
// edge recorded twice, once as an issue ref and once as an external ref -- legal,
// since the unique keys are per typed column -- with the external twin already
// moved onto the deterministic id by the aborted pass.
func TestEmbeddedDepRekeyRepairsHalfRekeyedDuplicateEdge(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()

	dataDir := seedMainSchemaAt(t, ctx, schema.LatestVersion())
	conn, closeConn := openPinnedConn(t, ctx, dataDir)
	if _, err := schema.MigrateUp(ctx, conn); err != nil {
		t.Fatalf("MigrateUp on the seed store: %v", err)
	}

	const source, target = "hq-cv-vvnmq", "ops-w05"
	want := depid.New(source, target)
	seedIssue(t, ctx, conn, source)
	seedIssue(t, ctx, conn, target)
	// The surviving orientation, still carrying its 0043-minted random id.
	seedDependency(t, ctx, conn, "random-issue-row", source, strptr(target), nil, nil)
	// The twin the aborted pass already moved onto the deterministic id. It is
	// the loser anyway: the survivor rule is the typed column, not who got there
	// first, so the repair has to free this id before reusing it.
	seedDependency(t, ctx, conn, want, source, nil, nil, strptr(target))
	commitSeed(t, ctx, conn, "test: reproduce the #5268 half-re-keyed duplicate edge")

	// What makes this database a class-2b victim: the main cursor is at latest
	// and every numbered migration is recorded, so without a pending marker
	// migrationWorkNeeded short-circuits and the re-key never runs again.
	unrecordIgnoredVersion(t, ctx, conn, depRekeyMarkerVersion)
	closeConn()

	conn2, closeConn2 := openPinnedConn(t, ctx, dataDir)
	defer closeConn2()

	// Pre-fix this returns "rekey dependency ids: dependencies: re-key id
	// random-issue-row -> ...: duplicate primary key given".
	if _, err := schema.MigrateUp(ctx, conn2); err != nil {
		t.Fatalf("MigrateUp over a half-re-keyed duplicate edge: %v", err)
	}

	wantRows := []depRow{{id: want, issueID: source, dependsOnIssueID: target}}
	if got := readDeps(t, ctx, conn2); !sameDepRows(got, wantRows) {
		t.Fatalf("dependencies = %v, want %v", got, wantRows)
	}
	if !ignoredVersionRecorded(t, ctx, conn2, depRekeyMarkerVersion) {
		t.Fatalf("ignored migration %d not recorded after a successful pass", depRekeyMarkerVersion)
	}

	// With the marker recorded and both cursors at latest, migrationWorkNeeded
	// is false and MigrateUp short-circuits before the re-key. Prove that
	// directly rather than by "nothing changed": seed a row whose id IS
	// divergent, and assert the next open leaves it alone. If the pass still
	// ran, this row would be re-keyed.
	seedIssue(t, ctx, conn2, "steady-src")
	seedDependency(t, ctx, conn2, "steady-random-id", "steady-src", nil, nil, strptr("external:e1"))
	commitSeed(t, ctx, conn2, "test: a divergent row written after the repair pass")

	if _, err := schema.MigrateUp(ctx, conn2); err != nil {
		t.Fatalf("MigrateUp on the converged store: %v", err)
	}
	wantSteady := []depRow{
		{id: want, issueID: source, dependsOnIssueID: target},
		{id: "steady-random-id", issueID: "steady-src", dependsOnExternal: "external:e1"},
	}
	if got := readDeps(t, ctx, conn2); !sameDepRows(got, wantSteady) {
		t.Fatalf("after the steady-state open dependencies = %v, want %v", got, wantSteady)
	}
}

// TestEmbeddedDepRekeyMergesFreshDuplicateEdges is the class-1 victim: a
// database with duplicate typed edges and pending migrations, i.e. any rig that
// upgrades across the #4259 fix carrying an old wisp-to-issue conversion. The
// re-key runs because migrations are pending, and must merge rather than abort.
func TestEmbeddedDepRekeyMergesFreshDuplicateEdges(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()

	t.Run("dependencies, main cursor behind", func(t *testing.T) {
		dataDir := seedMainSchemaAt(t, ctx, 53)
		conn, closeConn := openPinnedConn(t, ctx, dataDir)

		seedIssue(t, ctx, conn, "src")
		seedIssue(t, ctx, conn, "tgt")
		seedIssue(t, ctx, conn, "other")
		// The duplicate pair, both still random, plus an unrelated edge that
		// must survive the pass re-keyed and intact.
		seedDependency(t, ctx, conn, "rand-a", "src", strptr("tgt"), nil, nil)
		seedDependency(t, ctx, conn, "rand-b", "src", nil, nil, strptr("tgt"))
		seedDependency(t, ctx, conn, "rand-c", "other", strptr("tgt"), nil, nil)
		commitSeed(t, ctx, conn, "test: duplicate typed edges before the 54..latest jump")
		closeConn()

		conn2, closeConn2 := openPinnedConn(t, ctx, dataDir)
		defer closeConn2()

		if _, err := schema.MigrateUp(ctx, conn2); err != nil {
			t.Fatalf("MigrateUp over duplicate typed edges: %v", err)
		}
		if v := currentMainVersion(t, ctx, conn2); v != schema.LatestVersion() {
			t.Fatalf("main schema version = %d, want latest %d", v, schema.LatestVersion())
		}

		want := []depRow{
			{id: depid.New("other", "tgt"), issueID: "other", dependsOnIssueID: "tgt"},
			{id: depid.New("src", "tgt"), issueID: "src", dependsOnIssueID: "tgt"},
		}
		if got := readDeps(t, ctx, conn2); !sameDepRows(got, want) {
			t.Fatalf("dependencies = %v, want %v", got, want)
		}
	})

	t.Run("wisp_dependencies", func(t *testing.T) {
		// The clone-local twin of the same table. Its re-key never syncs, but it
		// aborts the whole pass just as loudly, so it needs the same merge.
		dataDir := seedMainSchemaAt(t, ctx, schema.LatestVersion())
		conn, closeConn := openPinnedConn(t, ctx, dataDir)
		if _, err := schema.MigrateUp(ctx, conn); err != nil {
			t.Fatalf("MigrateUp on the seed store: %v", err)
		}

		seedWisp(t, ctx, conn, "w1")
		seedIssue(t, ctx, conn, "wt")
		seedWispDependency(t, ctx, conn, "rand-w-a", "w1", strptr("wt"), nil, nil)
		seedWispDependency(t, ctx, conn, "rand-w-b", "w1", nil, nil, strptr("wt"))
		commitSeed(t, ctx, conn, "test: duplicate typed wisp edges")

		unrecordIgnoredVersion(t, ctx, conn, depRekeyMarkerVersion)
		closeConn()

		conn2, closeConn2 := openPinnedConn(t, ctx, dataDir)
		defer closeConn2()

		if _, err := schema.MigrateUp(ctx, conn2); err != nil {
			t.Fatalf("MigrateUp over duplicate typed wisp edges: %v", err)
		}
		want := []depRow{{id: depid.New("w1", "wt"), issueID: "w1", dependsOnIssueID: "wt"}}
		if got := readDepTable(t, ctx, conn2, "wisp_dependencies"); !sameDepRows(got, want) {
			t.Fatalf("wisp_dependencies = %v, want %v", got, want)
		}
	})
}

// TestEmbeddedDepRekeyAbortLeavesMarkerUnrecorded pins the retry guarantee that
// the marker's position in MigrateUp buys: a re-key that cannot converge fails
// the pass BEFORE ignoredSource.migrate records 0026, so the database does not
// claim it migrated and the next open tries again. This is the property whose
// absence turned #5268 from a loud error into silent half-applied state.
func TestEmbeddedDepRekeyAbortLeavesMarkerUnrecorded(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()

	dataDir := seedMainSchemaAt(t, ctx, schema.LatestVersion())
	conn, closeConn := openPinnedConn(t, ctx, dataDir)
	if _, err := schema.MigrateUp(ctx, conn); err != nil {
		t.Fatalf("MigrateUp on the seed store: %v", err)
	}

	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		seedIssue(t, ctx, conn, id)
	}
	// c3 -> c4 is squatting on the id that c1 -> c2 must move to: a distinct
	// edge, so the pass cannot merge them and must refuse instead of guessing.
	squatted := depid.New("c1", "c2")
	seedDependency(t, ctx, conn, "rand-c1", "c1", strptr("c2"), nil, nil)
	seedDependency(t, ctx, conn, squatted, "c3", strptr("c4"), nil, nil)
	commitSeed(t, ctx, conn, "test: a foreign row squatting a deterministic dependency id")

	unrecordIgnoredVersion(t, ctx, conn, depRekeyMarkerVersion)
	closeConn()

	conn2, closeConn2 := openPinnedConn(t, ctx, dataDir)
	_, err := schema.MigrateUp(ctx, conn2)
	if err == nil {
		t.Fatal("MigrateUp succeeded over a squatted deterministic id")
	}
	if !strings.Contains(err.Error(), "bd doctor") {
		t.Errorf("error %q does not point at bd doctor", err)
	}
	if ignoredVersionRecorded(t, ctx, conn2, depRekeyMarkerVersion) {
		t.Fatalf("ignored migration %d recorded despite the failed re-key", depRekeyMarkerVersion)
	}
	// Nothing was written: the plan is validated before the first statement.
	before := readDeps(t, ctx, conn2)
	if len(before) != 2 {
		t.Fatalf("dependencies = %v, want the two seeded rows untouched", before)
	}

	// Repair the way `bd doctor` would -- give the squatter its own
	// deterministic id -- and prove the retried pass converges.
	mustExecConn(t, ctx, conn2, "UPDATE dependencies SET id = ? WHERE id = ?", depid.New("c3", "c4"), squatted)
	commitSeed(t, ctx, conn2, "test: repair the squatted dependency id")
	closeConn2()

	conn3, closeConn3 := openPinnedConn(t, ctx, dataDir)
	defer closeConn3()
	if _, err := schema.MigrateUp(ctx, conn3); err != nil {
		t.Fatalf("MigrateUp after the repair: %v", err)
	}
	want := []depRow{
		{id: depid.New("c1", "c2"), issueID: "c1", dependsOnIssueID: "c2"},
		{id: depid.New("c3", "c4"), issueID: "c3", dependsOnIssueID: "c4"},
	}
	if got := readDeps(t, ctx, conn3); !sameDepRows(got, want) {
		t.Fatalf("dependencies = %v, want %v", got, want)
	}
	if !ignoredVersionRecorded(t, ctx, conn3, depRekeyMarkerVersion) {
		t.Fatalf("ignored migration %d not recorded after the retried pass succeeded", depRekeyMarkerVersion)
	}
}

// --- helpers ---

type depRow struct {
	id                string
	issueID           string
	dependsOnIssueID  string
	dependsOnWispID   string
	dependsOnExternal string
}

func seedDependency(t *testing.T, ctx context.Context, conn *sql.Conn, id, issueID string, issueTarget, wispTarget, externalTarget *string) {
	t.Helper()
	seedDepTable(t, ctx, conn, "dependencies", id, issueID, issueTarget, wispTarget, externalTarget)
}

func seedWispDependency(t *testing.T, ctx context.Context, conn *sql.Conn, id, issueID string, issueTarget, wispTarget, externalTarget *string) {
	t.Helper()
	seedDepTable(t, ctx, conn, "wisp_dependencies", id, issueID, issueTarget, wispTarget, externalTarget)
}

// seedDepTable inserts an edge with an explicit id, which is the whole point:
// the affected rows predate the deterministic derivation, so they cannot be
// created through AddDependency.
func seedDepTable(t *testing.T, ctx context.Context, conn *sql.Conn, table, id, issueID string, issueTarget, wispTarget, externalTarget *string) {
	t.Helper()
	mustExecConn(t, ctx, conn, `
INSERT INTO `+table+` (id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata)
VALUES (?, ?, ?, ?, ?, 'blocks', NOW(), 'tester', JSON_OBJECT())`,
		id, issueID, nullable(issueTarget), nullable(wispTarget), nullable(externalTarget))
}

func readDeps(t *testing.T, ctx context.Context, conn *sql.Conn) []depRow {
	t.Helper()
	return readDepTable(t, ctx, conn, "dependencies")
}

func readDepTable(t *testing.T, ctx context.Context, conn *sql.Conn, table string) []depRow {
	t.Helper()
	rows, err := conn.QueryContext(ctx, `
SELECT id, issue_id,
       COALESCE(depends_on_issue_id, ''),
       COALESCE(depends_on_wisp_id, ''),
       COALESCE(depends_on_external, '')
FROM `+table+`
ORDER BY issue_id, id`)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []depRow
	for rows.Next() {
		var r depRow
		if err := rows.Scan(&r.id, &r.issueID, &r.dependsOnIssueID, &r.dependsOnWispID, &r.dependsOnExternal); err != nil {
			t.Fatalf("scan %s row: %v", table, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	return out
}

func sameDepRows(got, want []depRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// unrecordIgnoredVersion removes one ignored-series cursor row, reproducing the
// state a clone is in before it has run the marker's pass. The cursor table is
// dolt-ignored, so this needs no commit.
func unrecordIgnoredVersion(t *testing.T, ctx context.Context, conn *sql.Conn, version int) {
	t.Helper()
	mustExecConn(t, ctx, conn, "DELETE FROM ignored_schema_migrations WHERE version = ?", version)
}

func ignoredVersionRecorded(t *testing.T, ctx context.Context, conn *sql.Conn, version int) bool {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ignored_schema_migrations WHERE version = ?", version).Scan(&n); err != nil {
		t.Fatalf("read ignored_schema_migrations: %v", err)
	}
	return n > 0
}
