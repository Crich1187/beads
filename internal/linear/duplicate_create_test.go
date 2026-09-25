package linear

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// Acceptance run 2, finding N2: one bead, two Linear issues (TEST-56 linked,
// TEST-57 an orphan carrying the bead's idempotency marker and the bead's
// labels from before its link was written).
//
// Reproduced conditions: `bd linear push <bead>` batch-creates the issue; the
// client times out (the real push took 33.2 s against a 30 s timeout) while
// Linear still completes that first request later; Execute used to resend the
// mutation blindly, so the resend created the issue bd linked and the timed
// out request landed afterwards as the orphan.
func TestPushTimedOutCreateNeverDuplicatesLinearIssue(t *testing.T) {
	fake := newFakeLinear(t, "task", "gate:qa", "owner:herdr-implementer", "runtime:claude")
	fake.firstCreateDelay = 1500 * time.Millisecond
	lt := fake.tracker()
	lt.clients["team-1"] = lt.clients["team-1"].WithHTTPClient(&http.Client{Timeout: 300 * time.Millisecond})

	store := newMemTrackerStore()
	bead := &types.Issue{
		ID: "Test-Sandbox-bgy", Title: "[acc22 T09] label merge", Status: types.StatusInProgress,
		Priority: 2, IssueType: types.TypeTask, CreatedBy: "herdr-implementer/claude:acc22",
		Labels: []string{"gate:qa", "owner:herdr-implementer", "runtime:claude"},
	}
	if err := store.CreateIssue(context.Background(), bead, "test"); err != nil {
		t.Fatal(err)
	}
	stored := store.issue(bead.ID)
	marker := GenerateIdempotencyMarker(stored.ID, stored.CreatedBy, stored.CreatedAt.UnixNano())

	// Cycle 1, the runner's create step: `bd linear push Test-Sandbox-bgy`.
	res, err := tracker.NewEngine(lt, store, "linear-sync-runner").Sync(context.Background(),
		tracker.SyncOptions{Push: true, IssueIDs: []string{bead.ID}})
	if err != nil {
		t.Fatalf("create push: %v", err)
	}
	if got := fake.createRequests.Load(); got != 1 {
		t.Fatalf("create mutations sent = %d, want 1: a timed-out create must never be resent", got)
	}
	if res.PushStats.Created != 0 || res.PushStats.Errors != 1 {
		t.Fatalf("create push stats = %+v, want created 0 and 1 error (outcome unknown)", res.PushStats)
	}
	if !anyContains(res.Warnings, "create outcome unknown") {
		t.Fatalf("warnings %q do not say the create outcome is unknown", res.Warnings)
	}
	if ref := store.issue(bead.ID).ExternalRef; ref != nil {
		t.Fatalf("bead linked to %q after an unconfirmed create", *ref)
	}

	// Linear finishes the timed-out request after bd gave up on it.
	waitFor(t, 5*time.Second, func() bool { return len(fake.issuesWithDescription(marker)) == 1 })

	// Cycle 2, the next cycle's `bd linear sync --push`.
	engine := tracker.NewEngine(lt, store, "linear-sync-runner")
	engine.ThreeWayLabelMerge = true
	res, err = engine.Sync(context.Background(), tracker.SyncOptions{Push: true})
	if err != nil {
		t.Fatalf("sync push: %v", err)
	}
	if got := fake.createRequests.Load(); got != 1 {
		t.Fatalf("create mutations sent = %d after the next push, want still 1", got)
	}
	if got := fake.issueIdentifiers(); !reflect.DeepEqual(got, []string{"TEST-1"}) {
		t.Fatalf("Linear issues = %v, want exactly [TEST-1], the one the timed-out create made", got)
	}
	ref := store.issue(bead.ID).ExternalRef
	if ref == nil || *ref != "https://linear.app/ws/issue/TEST-1" {
		t.Fatalf("bead external_ref = %v, want it linked to the issue that landed (TEST-1)", ref)
	}
	if res.PushStats.Created != 0 || res.PushStats.Updated != 1 {
		t.Fatalf("sync push stats = %+v, want created 0 (nothing was created) and updated 1", res.PushStats)
	}

	// Settled: a third push creates nothing either.
	if _, err := engine.Sync(context.Background(), tracker.SyncOptions{Push: true}); err != nil {
		t.Fatalf("settled push: %v", err)
	}
	if got := fake.createRequests.Load(); got != 1 {
		t.Fatalf("create mutations sent = %d after a settled push, want 1", got)
	}
	if got := fake.issueIdentifiers(); !reflect.DeepEqual(got, []string{"TEST-1"}) {
		t.Fatalf("Linear issues after a settled push = %v, want [TEST-1]", got)
	}
}

// No create mutation is ever sent for a bead whose external_ref points at
// Linear, whichever push path it takes. Before this fix the batch path created
// issues for Linear refs that are not issue URLs (a project URL, "linear:")
// and Tracker.CreateIssue had no guard at all; a bead linked by an issue URL
// already went to the update path, and this pins that too.
func TestPushNeverCreatesForLinkedBead(t *testing.T) {
	newBead := func(id, ref string) *types.Issue {
		issue := &types.Issue{
			ID: id, Title: "bead " + id, Description: "local edit", Status: types.StatusInProgress,
			Priority: 2, IssueType: types.TypeTask, CreatedBy: "agent", CreatedAt: time.Unix(1790000000, 0).UTC(),
		}
		if ref != "" {
			issue.ExternalRef = &ref
		}
		return issue
	}
	linkedBeads := func() []*types.Issue {
		return []*types.Issue{
			newBead("bd-normal", "https://linear.app/ws/issue/TEST-1"),
			newBead("bd-slug", "https://linear.app/ws/issue/TEST-1/bead-bd-slug"),
			newBead("bd-forced", "https://linear.app/ws/issue/TEST-2"),
			newBead("bd-missing", "https://linear.app/ws/issue/TEST-99"),
			newBead("bd-archived", "https://linear.app/ws/issue/TEST-3"),
			newBead("bd-project", "https://linear.app/ws/project/sandbox-work-51d739d6"),
			newBead("bd-linear-scheme", "linear:TEST-4"),
		}
	}
	setup := func(t *testing.T) *fakeLinear {
		fake := newFakeLinear(t, "task")
		fake.addIssue("TEST-1", "remote one", "")
		fake.addIssue("TEST-2", "remote two", "")
		fake.addIssue("TEST-3", "remote three", "")
		fake.archive("TEST-3")
		return fake
	}
	onlyCreated := func(t *testing.T, fake *fakeLinear, want ...string) {
		t.Helper()
		fake.mu.Lock()
		got := append([]string(nil), fake.createdTitles...)
		fake.mu.Unlock()
		if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("created Linear issues for %v, want only %v", got, want)
		}
	}

	t.Run("batch_forced_and_mixed", func(t *testing.T) {
		fake := setup(t)
		beads := append(linkedBeads(), newBead("bd-new", ""))
		result, err := fake.tracker().BatchPush(context.Background(), beads, map[string]bool{"bd-forced": true, "bd-missing": true})
		if err != nil {
			t.Fatalf("BatchPush: %v", err)
		}
		onlyCreated(t, fake, "bead bd-new")
		if len(result.Created) != 1 || result.Created[0].LocalID != "bd-new" {
			t.Fatalf("Created = %+v, want only bd-new", result.Created)
		}
		for _, id := range []string{"bd-project", "bd-linear-scheme", "bd-archived"} {
			if !containsString(result.Refused, id) {
				t.Errorf("%s not refused (Refused = %v)", id, result.Refused)
			}
		}
		if !hasBatchError(result, "bd-missing") {
			t.Errorf("linked-but-missing bead has no per-issue error: %+v", result.Errors)
		}
	})

	t.Run("engine_sync_push", func(t *testing.T) {
		fake := setup(t)
		store := newMemTrackerStore()
		for _, bead := range linkedBeads() {
			if err := store.CreateIssue(context.Background(), bead, "test"); err != nil {
				t.Fatal(err)
			}
		}
		engine := tracker.NewEngine(fake.tracker(), store, "linear-sync-runner")
		engine.ThreeWayLabelMerge = true
		res, err := engine.Sync(context.Background(), tracker.SyncOptions{Push: true})
		if err != nil {
			t.Fatalf("sync push: %v", err)
		}
		onlyCreated(t, fake)
		if res.PushStats.Created != 0 {
			t.Fatalf("created = %d, want 0", res.PushStats.Created)
		}
	})

	t.Run("multi_team", func(t *testing.T) {
		fake := setup(t)
		lt := fake.tracker()
		lt.teamIDs = []string{"team-1", "team-2"}
		lt.clients["team-2"] = NewClient("key", "team-2").WithEndpoint(fake.server.URL)
		if _, err := lt.BatchPush(context.Background(), linkedBeads(), nil); err != nil {
			t.Fatalf("BatchPush: %v", err)
		}
		onlyCreated(t, fake)
	})

	t.Run("single_create", func(t *testing.T) {
		fake := setup(t)
		lt := fake.tracker()
		for _, bead := range linkedBeads() {
			if _, err := lt.CreateIssue(context.Background(), bead); err == nil {
				t.Errorf("CreateIssue(%s) succeeded for a linked bead", bead.ID)
			}
		}
		onlyCreated(t, fake)
		if got := fake.createRequests.Load(); got != 0 {
			t.Fatalf("create mutations sent = %d, want 0", got)
		}
	})
}

func anyContains(values []string, sub string) bool {
	for _, v := range values {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func hasBatchError(result *tracker.BatchPushResult, localID string) bool {
	for _, e := range result.Errors {
		if e.LocalID == localID {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
