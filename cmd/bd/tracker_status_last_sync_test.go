package main

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// statusLastSyncStore answers the reads the status commands make; any other
// tracker.Store method panics through the nil embedded interface.
type statusLastSyncStore struct {
	tracker.Store
	config        map[string]string
	localMetadata map[string]string
}

func (s *statusLastSyncStore) GetConfig(_ context.Context, key string) (string, error) {
	return s.config[key], nil
}

func (s *statusLastSyncStore) GetLocalMetadata(_ context.Context, key string) (string, error) {
	return s.localMetadata[key], nil
}

func (s *statusLastSyncStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	return nil, nil
}

// The engine records <prefix>.last_sync in local_metadata. Acceptance run 1
// (finding F1) showed `bd linear status --json` reporting "" because status
// read only the config table.
func TestLinearStatusReportsEngineLastSync(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		local  string
		config string
		want   string
	}{
		{name: "engine-written local metadata", local: "2026-09-24T16:25:48Z", want: "2026-09-24T16:25:48Z"},
		{name: "local metadata beats stale config", local: "2026-09-24T16:25:48Z", config: "2026-01-01T00:00:00Z", want: "2026-09-24T16:25:48Z"},
		{name: "legacy config fallback", config: "2026-01-01T00:00:00Z", want: "2026-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &statusLastSyncStore{
				config:        map[string]string{"linear.team_id": "team-1"},
				localMetadata: map[string]string{},
			}
			if tc.local != "" {
				st.localMetadata["linear.last_sync"] = tc.local
			}
			if tc.config != "" {
				st.config["linear.last_sync"] = tc.config
			}
			info, err := collectLinearStatus(ctx, st)
			if err != nil {
				t.Fatalf("collectLinearStatus: %v", err)
			}
			if got := info.jsonMap()["last_sync"]; got != tc.want {
				t.Fatalf("status last_sync = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJiraStatusReportsEngineLastSync(t *testing.T) {
	ctx := context.Background()
	st := &statusLastSyncStore{
		config:        map[string]string{"jira.last_sync": "2026-01-01T00:00:00Z"},
		localMetadata: map[string]string{"jira.last_sync": "2026-09-24T16:25:48Z"},
	}
	if got := jiraStatusLastSync(ctx, st); got != "2026-09-24T16:25:48Z" {
		t.Fatalf("jira status last_sync = %q, want the local_metadata value", got)
	}
	delete(st.localMetadata, "jira.last_sync")
	if got := jiraStatusLastSync(ctx, st); got != "2026-01-01T00:00:00Z" {
		t.Fatalf("jira status last_sync = %q, want the legacy config value", got)
	}
}
