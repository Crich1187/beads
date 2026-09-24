package linear

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// Finding F6 (T06c): a bead linked to an archived Linear issue was pushed
// anyway. The default issues query hides archived issues, so the skip-check
// fetch came back empty and BatchPush sent issueUpdate to the archived issue.
func TestBatchPush_RefusesArchivedIssue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
	}{
		{name: "normal push"},
		{name: "forced push (conflict resolution)", force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeLinear(t)
			fake.addIssue("TEST-15", "pollution probe", "old text")
			fake.archive("TEST-15")
			ref := "https://linear.app/ws/issue/TEST-15"
			local := &types.Issue{ID: "Test-Sandbox-xwmnlz", Title: "pollution probe", Description: "edited locally", Status: types.StatusInProgress, Priority: 2, ExternalRef: &ref}
			var force map[string]bool
			if tc.force {
				force = map[string]bool{local.ID: true}
			}

			result, err := fake.tracker().BatchPush(context.Background(), []*types.Issue{local}, force)
			if err != nil {
				t.Fatalf("BatchPush: %v", err)
			}
			if n := fake.updateCount(); n != 0 {
				t.Fatalf("issueUpdate sent %d time(s) to an archived issue", n)
			}
			if fake.includeArchivedSeen == 0 || fake.archivedFieldSeen == 0 {
				t.Fatalf("push fetch did not ask for archived issues (includeArchived=%d, archivedAt=%d)", fake.includeArchivedSeen, fake.archivedFieldSeen)
			}
			if len(result.Updated) != 0 || len(result.Errors) != 0 || len(result.Skipped) != 0 || len(result.Refused) != 1 || result.Refused[0] != local.ID {
				t.Fatalf("result updated=%v errors=%v skipped=%v refused=%v, want the bead refused", result.Updated, result.Errors, result.Skipped, result.Refused)
			}
			if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], local.ID) || !strings.Contains(result.Warnings[0], "TEST-15 is archived") {
				t.Fatalf("warnings = %q, want one per-issue archived warning", result.Warnings)
			}
		})
	}
}

// An unarchived issue in the same push is still updated: the refusal is per
// issue, not per batch.
func TestBatchPush_ArchivedRefusalIsPerIssue(t *testing.T) {
	fake := newFakeLinear(t)
	fake.addIssue("TEST-15", "archived one", "old")
	fake.archive("TEST-15")
	fake.addIssue("TEST-16", "live one", "old")
	archivedRef, liveRef := "https://linear.app/ws/issue/TEST-15", "https://linear.app/ws/issue/TEST-16"
	issues := []*types.Issue{
		{ID: "bd-archived", Title: "archived one", Description: "new", Status: types.StatusInProgress, Priority: 2, ExternalRef: &archivedRef},
		{ID: "bd-live", Title: "live one", Description: "new", Status: types.StatusInProgress, Priority: 2, ExternalRef: &liveRef},
	}
	result, err := fake.tracker().BatchPush(context.Background(), issues, nil)
	if err != nil {
		t.Fatalf("BatchPush: %v", err)
	}
	if len(fake.updatedIdentifiers) != 1 || fake.updatedIdentifiers[0] != "TEST-16" {
		t.Fatalf("updated %v, want only TEST-16", fake.updatedIdentifiers)
	}
	if len(result.Updated) != 1 || result.Updated[0].LocalID != "bd-live" {
		t.Fatalf("Updated = %v", result.Updated)
	}
}

func TestTrackerUpdateIssue_RefusesArchivedIssue(t *testing.T) {
	fake := newFakeLinear(t)
	fake.addIssue("TEST-15", "pollution probe", "old text")
	fake.archive("TEST-15")
	_, err := fake.tracker().UpdateIssue(context.Background(), "TEST-15", &types.Issue{ID: "bd-1", Title: "x", Description: "y", Status: types.StatusInProgress, Priority: 2})
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("UpdateIssue error = %v, want archived refusal", err)
	}
	if n := fake.updateCount(); n != 0 {
		t.Fatalf("issueUpdate sent %d time(s) to an archived issue", n)
	}
}

// Through the engine, as the runner's unscoped `bd linear sync --push` runs
// it: the archived link is reported in the sync result and nothing is sent.
func TestEnginePush_ArchivedLinearIssueReportedNotWritten(t *testing.T) {
	fake := newFakeLinear(t)
	fake.addIssue("TEST-15", "pollution probe", "old text")
	fake.archive("TEST-15")
	store := newMemTrackerStore()
	ref := "https://linear.app/ws/issue/TEST-15"
	if err := store.CreateIssue(context.Background(), &types.Issue{ID: "bd-x", Title: "pollution probe", Description: "edited", Status: types.StatusInProgress, Priority: 2, ExternalRef: &ref}, "test"); err != nil {
		t.Fatal(err)
	}
	result, err := tracker.NewEngine(fake.tracker(), store, "sync").Sync(context.Background(), tracker.SyncOptions{Push: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n := fake.updateCount(); n != 0 {
		t.Fatalf("issueUpdate sent %d time(s) to an archived issue", n)
	}
	if result.PushStats.Updated != 0 || result.PushStats.Skipped != 1 {
		t.Fatalf("PushStats = %+v, want 1 skipped", result.PushStats)
	}
	found := false
	for _, w := range result.Warnings {
		found = found || strings.Contains(w, "bd-x") && strings.Contains(w, "archived")
	}
	if !found {
		t.Fatalf("sync warnings %q do not report the archived link", result.Warnings)
	}
}

// Pull keeps treating archived issues as absent: the default fetch is
// unchanged.
func TestFetchIssueByIdentifier_StillHidesArchived(t *testing.T) {
	fake := newFakeLinear(t)
	fake.addIssue("TEST-15", "pollution probe", "old text")
	fake.archive("TEST-15")
	client := fake.tracker().clients["team-1"]
	got, err := client.FetchIssueByIdentifier(context.Background(), "TEST-15")
	if err != nil || got != nil {
		t.Fatalf("FetchIssueByIdentifier = %+v, %v; want nil for an archived issue", got, err)
	}
	got, err = client.FetchIssueByIdentifierIncludingArchived(context.Background(), "TEST-15")
	if err != nil || got == nil || !got.IsArchived() {
		t.Fatalf("FetchIssueByIdentifierIncludingArchived = %+v, %v; want the archived issue", got, err)
	}
}
