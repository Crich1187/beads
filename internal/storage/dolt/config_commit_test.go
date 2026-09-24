package dolt

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/memoryops"
)

// TestRootC1Q3P_ConfigSetSurvivesCommitWithConfig pins the explicit-commit
// half of root-c1q3p: a config write left in the working set (here under a
// deferred version-commit context, i.e. --dolt-auto-commit batch/off) is
// readable but missing from HEAD, plain Commit must not sweep it (GH#2455),
// and CommitWithConfig must publish it.
//
// Since config-LinearSync-001.30 a NON-deferred SetConfig publishes itself;
// that contract is pinned by TestConfigLinearSync30_SetConfigPublishesToHead.
func TestRootC1Q3P_ConfigSetSurvivesCommitWithConfig(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	const key = "custom.root_c1q3p.probe"
	const val = "durable-probe-value"

	if err := store.SetConfig(issueops.WithDeferredVersionCommit(ctx), key, val); err != nil {
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

	// CommitWithConfig (and CommitAll used by bd dolt/vc commit) must make the
	// value survive AS OF HASHOF('HEAD').
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

// TestRootC1Q3P_CommitAllSurvivesConfigSet is the CLI contract for
// `bd dolt commit` / `bd vc commit`: they route through CommitAll, which must
// include config (not DOLT_COMMIT -Am alone).
func TestRootC1Q3P_CommitAllSurvivesConfigSet(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	const key = "custom.root_c1q3p.commit_all"
	const val = "commit-all-durable"

	// Deferred so the config row is still dirty when CommitAll runs.
	if err := store.SetConfig(issueops.WithDeferredVersionCommit(ctx), key, val); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	committed, err := store.CommitAll(ctx, "bd: dolt commit include config")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if !committed {
		t.Fatal("CommitAll reported nothing to commit while config was dirty")
	}

	var headCnt int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM config AS OF HASHOF('HEAD') WHERE `key` = ?", key).Scan(&headCnt); err != nil {
		t.Fatalf("head count: %v", err)
	}
	if headCnt != 1 {
		t.Fatalf("CommitAll must include config; HEAD count=%d want 1", headCnt)
	}
}

// configTableState reports, for one table, whether dolt_status lists it and
// the row counts in the working set and AS OF HEAD.
func configTableState(t *testing.T, ctx context.Context, store *DoltStore, table string) (dirty bool, working, head int) {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_status WHERE table_name = ?", table).Scan(&n); err != nil {
		t.Fatalf("dolt_status %s: %v", table, err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&working); err != nil {
		t.Fatalf("working count %s: %v", table, err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" AS OF HASHOF('HEAD')").Scan(&head); err != nil {
		t.Fatalf("head count %s: %v", table, err)
	}
	return n > 0, working, head
}

func assertTablePublished(t *testing.T, ctx context.Context, store *DoltStore, step string, tables ...string) {
	t.Helper()
	for _, table := range tables {
		dirty, working, head := configTableState(t, ctx, store, table)
		if dirty || working != head {
			t.Fatalf("%s: %s not published: dirty=%v working=%d head=%d (config-LinearSync-001.30 stranding shape)",
				step, table, dirty, working, head)
		}
	}
}

// TestConfigLinearSync30_SetConfigPublishesToHead is the regression test for
// config-LinearSync-001.30 (recurrence of root-c1q3p): on the direct
// SQL-server route the CLI auto-commit epilogue does not run, so SetConfig /
// DeleteConfig themselves must leave dolt_status clean for config and its
// projection table, with AS OF HEAD equal to the working set — without any
// later sweep (hourly hub backup, bd dolt commit). It also pins the two
// boundaries of the fix: a deferred context still leaves the write in the
// working set (batch/off), and the commit never sweeps a concurrent writer's
// unrelated dirty table (GH#2455).
func TestConfigLinearSync30_SetConfigPublishesToHead(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// 1. Plain key: config row lands in HEAD immediately.
	if err := store.SetConfig(ctx, "custom.ls30.probe", "v1"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	assertTablePublished(t, ctx, store, "SetConfig custom key", "config")

	// 2. Projected key: config AND custom_statuses land in the same commit.
	if err := store.SetConfig(ctx, "status.custom", "ls30_waiting:active"); err != nil {
		t.Fatalf("SetConfig status.custom: %v", err)
	}
	assertTablePublished(t, ctx, store, "SetConfig status.custom", "config", "custom_statuses")
	var msg string
	if err := store.db.QueryRowContext(ctx,
		"SELECT message FROM dolt_log LIMIT 1").Scan(&msg); err != nil {
		t.Fatalf("dolt_log: %v", err)
	}
	if msg != "bd: config set status.custom" {
		t.Fatalf("HEAD message = %q, want %q", msg, "bd: config set status.custom")
	}

	// 3. DeleteConfig publishes the deletion.
	if err := store.DeleteConfig(ctx, "custom.ls30.probe"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	assertTablePublished(t, ctx, store, "DeleteConfig", "config")

	// 4. Deferred context (batch/off): stays in the working set, then one
	// CommitConfigWrites publishes the batch including the projection.
	deferred := issueops.WithDeferredVersionCommit(ctx)
	if err := store.SetConfig(deferred, "custom.ls30.batch", "b"); err != nil {
		t.Fatalf("deferred SetConfig: %v", err)
	}
	if err := store.SetConfig(deferred, "types.custom", "ls30type"); err != nil {
		t.Fatalf("deferred SetConfig types.custom: %v", err)
	}
	if dirty, _, _ := configTableState(t, ctx, store, "config"); !dirty {
		t.Fatal("deferred SetConfig must leave config in the working set")
	}
	if dirty, _, _ := configTableState(t, ctx, store, "custom_types"); !dirty {
		t.Fatal("deferred SetConfig types.custom must leave custom_types in the working set")
	}
	if err := store.CommitConfigWrites(ctx, []string{"custom.ls30.batch", "types.custom"}, "bd: config set-many"); err != nil {
		t.Fatalf("CommitConfigWrites: %v", err)
	}
	assertTablePublished(t, ctx, store, "CommitConfigWrites", "config", "custom_types")

	// 5. GH#2455: a concurrent writer's unrelated dirty table is not swept.
	if _, err := store.db.ExecContext(ctx,
		"INSERT INTO metadata (`key`, value) VALUES ('ls30_foreign', 'x')"); err != nil {
		t.Fatalf("dirty metadata: %v", err)
	}
	if err := store.SetConfig(ctx, "custom.ls30.after_foreign", "y"); err != nil {
		t.Fatalf("SetConfig with foreign dirt: %v", err)
	}
	assertTablePublished(t, ctx, store, "SetConfig with foreign dirt", "config")
	if dirty, _, _ := configTableState(t, ctx, store, "metadata"); !dirty {
		t.Fatal("SetConfig swept an unrelated dirty table (metadata) into its commit (GH#2455)")
	}

	// 6. Memories are kv.memory.* config rows: bd remember / bd forget share
	// the contract.
	mem, err := store.Memories()
	if err != nil {
		t.Fatalf("Memories: %v", err)
	}
	if _, err := mem.Remember(ctx, memoryops.RememberRequest{Key: "ls30-mem", Content: "durable memory"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	assertTablePublished(t, ctx, store, "Remember", "config")
	if _, err := mem.Forget(ctx, memoryops.ForgetRequest{Key: "ls30-mem"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	assertTablePublished(t, ctx, store, "Forget", "config")
}
