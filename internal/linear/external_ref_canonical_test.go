package linear

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func TestCanonicalLinearIssueRefIsIdempotent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://linear.app/lymarinc/issue/TEST-17/acc22-20260924t162500z-t09-label-merge", "https://linear.app/lymarinc/issue/TEST-17"},
		{"https://linear.app/lymarinc/issue/TEST-17", "https://linear.app/lymarinc/issue/TEST-17"},
		{"linear:project-milestone:abc", "linear:project-milestone:abc"},
	} {
		got := canonicalLinearIssueRef(tc.in)
		if got != tc.want || canonicalLinearIssueRef(got) != got {
			t.Fatalf("canonicalLinearIssueRef(%q) = %q (twice: %q), want %q", tc.in, got, canonicalLinearIssueRef(got), tc.want)
		}
	}
}

// BatchPush reports the canonical (slug-less) ref, the same form pull writes,
// even though Linear's mutation responses carry the slug URL.
func TestBatchPush_ReportsCanonicalExternalRef(t *testing.T) {
	fake := newFakeLinear(t)
	fake.addIssue("TEST-17", "label merge", "old")
	ref := "https://linear.app/ws/issue/TEST-17"
	local := &types.Issue{ID: "Test-Sandbox-o8l", Title: "label merge", Description: "new", Status: types.StatusInProgress, Priority: 2, ExternalRef: &ref}
	result, err := fake.tracker().BatchPush(context.Background(), []*types.Issue{local}, nil)
	if err != nil {
		t.Fatalf("BatchPush: %v", err)
	}
	if len(result.Updated) != 1 || result.Updated[0].ExternalRef != ref {
		t.Fatalf("Updated = %+v, want ref %q", result.Updated, ref)
	}
}

// Finding F5: external_ref flipped between the slug and slug-less URL on every
// pull/push pair. Pull -> local edit -> push -> pull must leave the ref in one
// form and write it exactly once (at creation).
func TestEngineRoundTrip_ExternalRefStable(t *testing.T) {
	ctx := context.Background()
	fake := newFakeLinear(t)
	fake.addIssue("TEST-17", "label merge", "old")
	store := newMemTrackerStore()
	lt := fake.tracker()
	sync := func(opts tracker.SyncOptions) *tracker.SyncResult {
		t.Helper()
		result, err := tracker.NewEngine(lt, store, "sync").Sync(ctx, opts)
		if err != nil {
			t.Fatalf("Sync(%+v): %v", opts, err)
		}
		return result
	}

	sync(tracker.SyncOptions{Pull: true})
	bead := store.only(t)
	const canonical = "https://linear.app/ws/issue/TEST-17"
	if bead.ExternalRef == nil || *bead.ExternalRef != canonical {
		t.Fatalf("pulled external_ref = %v, want %q", bead.ExternalRef, canonical)
	}

	store.localEdit(bead.ID, func(i *types.Issue) { i.Description = "edited by the agent" }, true)
	pushed := sync(tracker.SyncOptions{Push: true})
	if pushed.PushStats.Updated != 1 || fake.updateCount() != 1 {
		t.Fatalf("push stats %+v, updates %d; want the edit pushed once", pushed.PushStats, fake.updateCount())
	}
	if got := store.issue(bead.ID).ExternalRef; got == nil || *got != canonical {
		t.Fatalf("external_ref after push = %v, want %q", got, canonical)
	}

	pulled := sync(tracker.SyncOptions{Pull: true})
	if pulled.PullStats.Updated != 0 {
		t.Fatalf("second pull updated %d bead(s); the ref must not flip", pulled.PullStats.Updated)
	}
	if len(store.refWrites) != 0 {
		t.Fatalf("external_ref rewritten after creation: %v", store.refWrites)
	}
}

// A bead still holding the legacy slug form converges to the canonical form
// in one write and then stays there.
func TestEngineRoundTrip_LegacySlugRefConvergesOnce(t *testing.T) {
	ctx := context.Background()
	fake := newFakeLinear(t)
	fake.addIssue("TEST-17", "label merge", "old")
	store := newMemTrackerStore()
	legacy := "https://linear.app/ws/issue/TEST-17/label-merge"
	if err := store.CreateIssue(ctx, &types.Issue{ID: "bd-legacy", Title: "label merge", Description: "new", Status: types.StatusInProgress, Priority: 2, ExternalRef: &legacy}, "test"); err != nil {
		t.Fatal(err)
	}
	lt := fake.tracker()
	for i := 0; i < 3; i++ {
		if _, err := tracker.NewEngine(lt, store, "sync").Sync(ctx, tracker.SyncOptions{Push: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := tracker.NewEngine(lt, store, "sync").Sync(ctx, tracker.SyncOptions{Pull: true}); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.refWrites) != 1 || store.refWrites[0] != "bd-legacy=https://linear.app/ws/issue/TEST-17" {
		t.Fatalf("external_ref writes = %v, want exactly one write of the canonical form", store.refWrites)
	}
}
