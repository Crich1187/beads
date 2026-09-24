package tracker

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// refWriteCountingStore counts the external_ref writes the engine makes.
type refWriteCountingStore struct {
	*pureTestStore
	refWrites []string
}

func (s *refWriteCountingStore) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if ref, ok := updates["external_ref"].(string); ok {
		s.refWrites = append(s.refWrites, id+"="+ref)
	}
	return s.pureTestStore.UpdateIssue(ctx, id, updates, actor)
}

// A batch push that reports the ref the issue already has must not rewrite
// it: the rewrite is a real issue update, and before finding F5 it ran for
// every pushed issue on every cycle.
func TestApplyBatchPushResultSkipsUnchangedExternalRef(t *testing.T) {
	ctx := context.Background()
	const canonical = "https://linear.app/ws/issue/TEST-17"
	for _, tc := range []struct {
		name       string
		reported   string
		wantWrites int
	}{
		{name: "unchanged ref is not rewritten", reported: canonical, wantWrites: 0},
		{name: "unchanged ref with surrounding space is not rewritten", reported: " " + canonical + " ", wantWrites: 0},
		{name: "changed ref is written once", reported: "https://linear.app/ws/issue/TEST-18", wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := canonical
			issue := &types.Issue{ID: "bd-1", Title: "t", Status: types.StatusOpen, IssueType: types.TypeTask, ExternalRef: &ref}
			store := &refWriteCountingStore{pureTestStore: newPureTestStore(issue)}
			mock := &mockBatchTracker{
				mockTracker: newMockTracker("linear"),
				batchResult: &BatchPushResult{Updated: []BatchPushItem{{LocalID: "bd-1", ExternalRef: tc.reported}}},
			}
			if _, err := NewEngine(mock, store, "sync").Sync(ctx, SyncOptions{Push: true}); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if len(store.refWrites) != tc.wantWrites {
				t.Fatalf("external_ref writes = %v, want %d", store.refWrites, tc.wantWrites)
			}
		})
	}
}

// A created issue has no ref yet, so its reported ref is always written.
func TestApplyBatchPushResultWritesCreatedExternalRef(t *testing.T) {
	ctx := context.Background()
	issue := &types.Issue{ID: "bd-new", Title: "t", Status: types.StatusOpen, IssueType: types.TypeTask}
	store := &refWriteCountingStore{pureTestStore: newPureTestStore(issue)}
	mock := &mockBatchTracker{
		mockTracker: newMockTracker("linear"),
		batchResult: &BatchPushResult{Created: []BatchPushItem{{LocalID: "bd-new", ExternalRef: "https://linear.app/ws/issue/TEST-40"}}},
	}
	if _, err := NewEngine(mock, store, "sync").Sync(ctx, SyncOptions{Push: true}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(store.refWrites) != 1 || store.refWrites[0] != "bd-new=https://linear.app/ws/issue/TEST-40" {
		t.Fatalf("external_ref writes = %v", store.refWrites)
	}
}
