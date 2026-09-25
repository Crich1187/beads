package tracker

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// lastSyncTestStore is a pureTestStore whose config table is a plain map, so
// tests can model a legacy store that kept last_sync in config.
type lastSyncTestStore struct {
	*pureTestStore
	config map[string]string
}

func newLastSyncTestStore(issues ...*types.Issue) *lastSyncTestStore {
	return &lastSyncTestStore{pureTestStore: newPureTestStore(issues...), config: map[string]string{}}
}

func (s *lastSyncTestStore) GetConfig(_ context.Context, key string) (string, error) {
	return s.config[key], nil
}

func (s *lastSyncTestStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	for _, issue := range s.issues {
		if issue.ExternalRef != nil && *issue.ExternalRef == ref {
			return issue, nil
		}
	}
	return nil, nil
}

func TestReadLastSync(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		local  string
		config string
		want   string
	}{
		{name: "local metadata only (what the engine writes)", local: "2026-09-24T16:25:48Z", want: "2026-09-24T16:25:48Z"},
		{name: "legacy config only", config: "2026-01-01T00:00:00Z", want: "2026-01-01T00:00:00Z"},
		{name: "local metadata wins over stale config", local: "2026-09-24T16:25:48Z", config: "2026-01-01T00:00:00Z", want: "2026-09-24T16:25:48Z"},
		{name: "neither set", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newLastSyncTestStore()
			if tc.local != "" {
				st.localMetadata["linear.last_sync"] = tc.local
			}
			if tc.config != "" {
				st.config["linear.last_sync"] = tc.config
			}
			if got := ReadLastSync(ctx, st, "linear"); got != tc.want {
				t.Fatalf("ReadLastSync = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEngineLastSyncReadableByReadLastSync pins the write/read contract: the
// value Sync records is exactly what ReadLastSync (and therefore every status
// command) reports, and the config table is never written.
func TestEngineLastSyncReadableByReadLastSync(t *testing.T) {
	ctx := context.Background()
	st := newLastSyncTestStore()
	result, err := NewEngine(newMockTracker("linear"), st, "sync").Sync(ctx, SyncOptions{Pull: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.LastSync == "" {
		t.Fatal("Sync did not record last_sync")
	}
	if got := ReadLastSync(ctx, st, "linear"); got != result.LastSync {
		t.Fatalf("ReadLastSync = %q, want the engine's %q", got, result.LastSync)
	}
	if v, ok := st.config["linear.last_sync"]; ok {
		t.Fatalf("engine wrote last_sync to config (%q); it belongs in local_metadata", v)
	}
}

// TestEnginePullUsesLegacyConfigLastSync: a store whose last_sync still lives
// in config pulls incrementally from it instead of treating the store as
// never synced.
func TestEnginePullUsesLegacyConfigLastSync(t *testing.T) {
	ctx := context.Background()
	st := newLastSyncTestStore()
	st.config["test.last_sync"] = "2026-01-01T00:00:00Z"
	var since string
	mock := newMockTracker("test")
	mock.fetchIssues = func(_ context.Context, opts FetchOptions) ([]TrackerIssue, error) {
		if opts.Since != nil {
			since = opts.Since.UTC().Format("2006-01-02T15:04:05Z")
		}
		return nil, nil
	}
	result, err := NewEngine(mock, st, "sync").Sync(ctx, SyncOptions{Pull: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !result.PullStats.Incremental || since != "2026-01-01T00:00:00Z" {
		t.Fatalf("pull incremental=%v since=%q, want incremental from the legacy config value", result.PullStats.Incremental, since)
	}
}

// With DeferLastSync, Sync leaves last_sync to the caller, which records it
// with RecordLastSync after its own post-sync passes (acceptance run 2,
// finding N1). An early stamp would stand if the passes never finished.
func TestEngineDeferLastSyncLeavesItToRecordLastSync(t *testing.T) {
	ctx := context.Background()
	st := newLastSyncTestStore()
	st.localMetadata["linear.last_sync"] = "2026-09-24T16:25:48Z"
	engine := NewEngine(newMockTracker("linear"), st, "sync")
	engine.DeferLastSync = true
	result, err := engine.Sync(ctx, SyncOptions{Pull: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.LastSync != "" || ReadLastSync(ctx, st, "linear") != "2026-09-24T16:25:48Z" {
		t.Fatalf("deferred Sync recorded last_sync (result %q, stored %q)", result.LastSync, ReadLastSync(ctx, st, "linear"))
	}
	recorded, err := engine.RecordLastSync(ctx)
	if err != nil {
		t.Fatalf("RecordLastSync: %v", err)
	}
	if got := ReadLastSync(ctx, st, "linear"); got == "" || got != recorded {
		t.Fatalf("ReadLastSync = %q, want the recorded %q", got, recorded)
	}
}
