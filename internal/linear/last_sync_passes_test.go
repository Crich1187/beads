package linear

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// Acceptance run 2, finding N1: the relation pass wrote TEST-44/TEST-45 one
// second after the push recorded last_sync, so the next pull re-pulled them,
// the runner flagged them as edited locally and in Linear, and bd's own type
// label "task" was imported into the beads.
//
// A cycle here is what `bd linear sync --pull` then `bd linear sync --push`
// do: each Sync, then the post-sync passes, then last_sync. The pass is
// modeled as the relation pass's two writes: one to the Linear issue (its
// updatedAt moves) and one to the bead (the relation ledger in metadata).
func TestLastSyncRecordedAfterPostPushPasses(t *testing.T) {
	run := func(t *testing.T, stampBeforePasses bool) (res *tracker.SyncResult, local, remote, lastSync time.Time, labels []string, base string) {
		fake := newFakeLinear(t, "task", "gate:qa")
		fake.honorSince = true
		lt := fake.tracker()
		store := newMemTrackerStore()
		bead := &types.Issue{ID: "Test-Sandbox-a2c.2", Title: "Child B", Status: types.StatusInProgress,
			Priority: 2, IssueType: types.TypeTask, CreatedBy: "fixture", Labels: []string{"gate:qa"}}
		if err := store.CreateIssue(context.Background(), bead, "test"); err != nil {
			t.Fatal(err)
		}
		engine := tracker.NewEngine(lt, store, "linear-sync-runner")
		engine.ThreeWayLabelMerge = true
		engine.DeferLastSync = !stampBeforePasses
		cycleStep := func(opts tracker.SyncOptions, pass func()) {
			t.Helper()
			if _, err := engine.Sync(context.Background(), opts); err != nil {
				t.Fatalf("sync %+v: %v", opts, err)
			}
			if pass != nil {
				pass()
			}
			if !stampBeforePasses {
				if _, err := engine.RecordLastSync(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		}
		relationPass := func() {
			// Land the writes past the second a stamp taken before this
			// pass would carry, so ordering alone decides the outcome.
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(1050 * time.Millisecond)))
			ref := store.issue(bead.ID).ExternalRef
			if ref == nil {
				t.Fatal("bead was not created in Linear")
			}
			fake.touch(ExtractLinearIdentifier(*ref))
			store.localEdit(bead.ID, func(i *types.Issue) { i.UpdatedAt = time.Now().UTC() }, false)
		}

		cycleStep(tracker.SyncOptions{Pull: true}, nil)
		cycleStep(tracker.SyncOptions{Push: true}, relationPass)

		// The next cycle's pull.
		var err error
		if res, err = engine.Sync(context.Background(), tracker.SyncOptions{Pull: true}); err != nil {
			t.Fatalf("next pull: %v", err)
		}
		if stampBeforePasses {
			// Old order: this pull's own stamp is later; read the one it ran against.
			lastSync, _ = time.Parse(time.RFC3339Nano, res.PullStats.SyncedSince)
		} else {
			lastSync, _ = time.Parse(time.RFC3339Nano, tracker.ReadLastSync(context.Background(), store, "linear"))
		}
		stored := store.issue(bead.ID)
		return res, stored.UpdatedAt, fake.updatedAtOf(ExtractLinearIdentifier(*stored.ExternalRef)), lastSync,
			sortedLabels(stored.Labels), store.localMetadata["linear.labelbase."+bead.ID]
	}

	t.Run("recorded_after_passes", func(t *testing.T) {
		res, local, remote, lastSync, labels, base := run(t, false)
		if res.PullStats.Candidates != 0 || res.PullStats.Updated != 0 || res.PullStats.Created != 0 {
			t.Fatalf("next pull saw changes: %+v, want none (the pass's writes are bd's own)", res.PullStats)
		}
		// The runner's guard: edited locally and in Linear since last_sync.
		if local.After(lastSync) || remote.After(lastSync) {
			t.Fatalf("pass writes after last_sync %s (bead %s, Linear %s): the runner would report a conflict", lastSync, local, remote)
		}
		if !reflect.DeepEqual(labels, []string{"gate:qa"}) || strings.Contains(base, `"task"`) {
			t.Fatalf("bead labels %v, baseline %s: bd's type label was imported", labels, base)
		}
	})

	// Control: stamped inside Sync, before the passes (the run-2 order), the
	// same cycle leaves both writes after last_sync and the next pull
	// fetches the issue again.
	t.Run("stamped_before_passes_reproduces_N1", func(t *testing.T) {
		res, local, remote, lastSync, _, _ := run(t, true)
		if res.PullStats.Candidates != 1 {
			t.Fatalf("next pull candidates = %d, want 1 (the pass-touched issue re-fetched)", res.PullStats.Candidates)
		}
		if !local.After(lastSync) || !remote.After(lastSync) {
			t.Fatalf("expected both writes after last_sync %s (bead %s, Linear %s)", lastSync, local, remote)
		}
	})
}

// bd pushes the type label itself (the label_type_map key for issue_type) and
// derives issue_type from it on pull, so the pull never makes it a bead label
// or puts it in the label baseline; a Linear-side label edit on the same issue
// still merges.
func TestPullNeverImportsBdTypeLabel(t *testing.T) {
	fake := newFakeLinear(t, "task", "gate:qa", "Governor", "Bug", "defect")
	fake.addIssue("TEST-56", "label merge", "", "task", "gate:qa")
	fake.addIssue("TEST-60", "a bug", "", "Bug", "defect")
	fake.addIssue("TEST-61", "type label only", "", "task")
	store := newMemTrackerStore()
	engine := tracker.NewEngine(fake.tracker(), store, "linear-sync-runner")
	engine.ThreeWayLabelMerge = true

	if _, err := engine.Sync(context.Background(), tracker.SyncOptions{Pull: true}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	fake.setLabels("TEST-56", "task", "gate:qa", "Governor") // Carl adds Governor
	if _, err := engine.Sync(context.Background(), tracker.SyncOptions{Pull: true}); err != nil {
		t.Fatalf("pull: %v", err)
	}

	for _, id := range store.order {
		issue := store.issue(id)
		base := store.localMetadata["linear.labelbase."+id]
		switch ExtractLinearIdentifier(*issue.ExternalRef) {
		case "TEST-56":
			if got := sortedLabels(issue.Labels); !reflect.DeepEqual(got, []string{"Governor", "gate:qa"}) || issue.IssueType != types.TypeTask {
				t.Errorf("TEST-56 bead labels %v type %s, want [Governor gate:qa] and type task", got, issue.IssueType)
			}
			if strings.Contains(base, `"task"`) {
				t.Errorf("TEST-56 label baseline %s holds the type label", base)
			}
		case "TEST-60":
			// "Bug" is the type label (any case); "defect" is not the
			// label bd pushes for type bug, so it stays a bead label.
			if got := sortedLabels(issue.Labels); !reflect.DeepEqual(got, []string{"defect"}) || issue.IssueType != types.TypeBug {
				t.Errorf("TEST-60 bead labels %v type %s, want [defect] and type bug", got, issue.IssueType)
			}
		case "TEST-61":
			// Only the type label: a known, empty label set, not "unknown".
			if len(issue.Labels) != 0 || base != `{"v":1,"target":"TEST-61","labels":[]}` {
				t.Errorf("TEST-61 bead labels %v, baseline %s, want none and an empty baseline", issue.Labels, base)
			}
		}
	}
	// And the push puts the type label back: Linear keeps "task".
	push := tracker.NewEngine(fake.tracker(), store, "linear-sync-runner")
	push.ThreeWayLabelMerge = true
	if _, err := push.Sync(context.Background(), tracker.SyncOptions{Push: true}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := fake.labelNames("TEST-56"); !reflect.DeepEqual(got, []string{"Governor", "gate:qa", "task"}) {
		t.Fatalf("Linear TEST-56 labels after push = %v, want [Governor gate:qa task]", got)
	}
}
