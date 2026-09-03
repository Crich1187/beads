package dolt

import (
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// TestRootC1Q3P_ConfigSetSurvivesCommitWithConfig pins the failure shape that
// stranded `bd config set` on every server-mode store: working-set readable,
// HEAD missing, until an explicit config-inclusive commit.
func TestRootC1Q3P_ConfigSetSurvivesCommitWithConfig(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	const key = "custom.root_c1q3p.probe"
	const val = "durable-probe-value"

	if err := store.SetConfig(ctx, key, val); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	var workingCnt, headCnt int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM config WHERE `key` = ?", key).Scan(&workingCnt); err != nil {
		t.Fatalf("working count: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM config AS OF HASHOF('HEAD') WHERE `key` = ?", key).Scan(&headCnt); err != nil {
		t.Fatalf("head count before commit: %v", err)
	}
	if workingCnt != 1 {
		t.Fatalf("expected working count 1, got %d", workingCnt)
	}
	if headCnt != 0 {
		t.Fatalf("expected HEAD count 0 before commit, got %d", headCnt)
	}

	// Plain Commit must leave config stranded (GH#2455 exclude path).
	if err := store.Commit(ctx, "should skip config"); err != nil && !issueops.IsNothingToCommitError(err) {
		t.Fatalf("Commit: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM config AS OF HASHOF('HEAD') WHERE `key` = ?", key).Scan(&headCnt); err != nil {
		t.Fatalf("head count after Commit: %v", err)
	}
	if headCnt != 0 {
		t.Fatalf("Commit must omit config; HEAD count=%d want 0", headCnt)
	}

	// CommitWithConfig (used by bd dolt commit / bd vc commit after root-c1q3p)
	// must make the value survive AS OF HASHOF('HEAD').
	if err := store.CommitWithConfig(ctx, "include config"); err != nil {
		t.Fatalf("CommitWithConfig: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM config AS OF HASHOF('HEAD') WHERE `key` = ?", key).Scan(&headCnt); err != nil {
		t.Fatalf("head count after CommitWithConfig: %v", err)
	}
	if headCnt != 1 {
		t.Fatalf("CommitWithConfig must include config; HEAD count=%d want 1", headCnt)
	}

	var got string
	if err := store.db.QueryRowContext(ctx,
		"SELECT value FROM config AS OF HASHOF('HEAD') WHERE `key` = ?", key).Scan(&got); err != nil {
		t.Fatalf("read HEAD value: %v", err)
	}
	if got != val {
		t.Fatalf("HEAD value=%q want %q", got, val)
	}
}
