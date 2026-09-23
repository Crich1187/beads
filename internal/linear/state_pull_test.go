package linear

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// tenStateConfig mirrors the ten-state contract (topology doc section 5.4):
// the original type-key map plus explicit per-name entries, loaded through
// LoadMappingConfig so key lowercasing and status.custom parsing are exercised.
func tenStateConfig() *MappingConfig {
	return LoadMappingConfig(&mockConfigLoader{config: map[string]string{
		"status.custom":                        "waiting_external:wip",
		"linear.state_map.backlog":             "open",
		"linear.state_map.unstarted":           "open",
		"linear.state_map.started":             "in_progress",
		"linear.state_map.completed":           "closed",
		"linear.state_map.canceled":            "closed",
		"linear.state_map.Blocked":             "blocked",
		"linear.state_map.Needs Carl":          "deferred",
		"linear.state_map.In Review":           "in_review",
		"linear.state_map.Waiting on External": "waiting_external",
		// Duplicate has its own state type, which no type default covers, so
		// it always resolved by name; this entry pulled as closed before the
		// patch too.
		"linear.state_map.duplicate": "closed",
	}})
}

// pullStatus runs a Linear issue in the given state through the field mapper's
// IssueToBeads, the conversion the tracker engine uses on pull.
func pullStatus(t *testing.T, config *MappingConfig, state *State) types.Status {
	t.Helper()
	li := &Issue{
		ID:         "uuid-" + state.Name,
		Identifier: "LYM-1",
		Title:      "state " + state.Name,
		URL:        "https://linear.app/lym/issue/LYM-1/state",
		State:      state,
		CreatedAt:  "2026-09-23T10:00:00Z",
		UpdatedAt:  "2026-09-23T10:00:00Z",
	}
	m := &linearFieldMapper{config: config}
	ti := linearToTrackerIssue(li)
	conv := m.IssueToBeads(&ti)
	if conv == nil || conv.Issue == nil {
		t.Fatalf("IssueToBeads(%q) returned nil conversion", state.Name)
	}
	return conv.Issue.Status
}

func TestPullExplicitStateNameBeatsTypeDefault(t *testing.T) {
	config := tenStateConfig()

	tests := []struct {
		state State
		want  types.Status
	}{
		// Explicit name entries win over the type default.
		{State{Name: "Blocked", Type: "started"}, types.StatusBlocked},
		{State{Name: "Needs Carl", Type: "unstarted"}, types.StatusDeferred},
		{State{Name: "In Review", Type: "started"}, types.StatusInReview},
		{State{Name: "Waiting on External", Type: "started"}, types.Status("waiting_external")},
		// States without a name entry keep their defaults.
		{State{Name: "Backlog", Type: "backlog"}, types.StatusOpen},
		{State{Name: "Todo", Type: "unstarted"}, types.StatusOpen},
		{State{Name: "In Progress", Type: "started"}, types.StatusInProgress},
		{State{Name: "Done", Type: "completed"}, types.StatusClosed},
		{State{Name: "Canceled", Type: "canceled"}, types.StatusClosed},
		{State{Name: "Duplicate", Type: "duplicate"}, types.StatusClosed},
	}

	for _, tt := range tests {
		t.Run(tt.state.Name, func(t *testing.T) {
			state := tt.state
			if got := pullStatus(t, config, &state); got != tt.want {
				t.Errorf("pull status for %q (%s) = %q, want %q", tt.state.Name, tt.state.Type, got, tt.want)
			}
			if got := StateToBeadsStatus(&state, config); got != tt.want {
				t.Errorf("StateToBeadsStatus(%q) = %q, want %q", tt.state.Name, got, tt.want)
			}
		})
	}
}

// Without the explicit name entries, the same states keep their type defaults.
func TestPullWithoutNameEntriesKeepsTypeDefaults(t *testing.T) {
	config := LoadMappingConfig(&mockConfigLoader{config: map[string]string{
		"status.custom":            "waiting_external:wip",
		"linear.state_map.started": "in_progress",
	}})
	for _, name := range []string{"Blocked", "In Review", "Waiting on External"} {
		if got := pullStatus(t, config, &State{Name: name, Type: "started"}); got != types.StatusInProgress {
			t.Errorf("pull status for %q without name entry = %q, want in_progress", name, got)
		}
	}
	if got := pullStatus(t, config, &State{Name: "Needs Carl", Type: "unstarted"}); got != types.StatusOpen {
		t.Errorf("pull status for Needs Carl without name entry = %q, want open", got)
	}
}

// An explicit type-key override still applies when no name entry matches.
func TestPullExplicitTypeOverrideStillApplies(t *testing.T) {
	config := LoadMappingConfig(&mockConfigLoader{config: map[string]string{
		"linear.state_map.started": "blocked",
	}})
	if got := pullStatus(t, config, &State{Name: "In Progress", Type: "started"}); got != types.StatusBlocked {
		t.Errorf("pull status with started=blocked = %q, want blocked", got)
	}
}

// A name entry naming a custom status that is not configured in status.custom
// is ignored, so the state falls back to its type default instead of becoming
// open (which would surface it in bd ready).
func TestPullUnknownExplicitNameValueFallsBackToType(t *testing.T) {
	config := LoadMappingConfig(&mockConfigLoader{config: map[string]string{
		"linear.state_map.started":             "in_progress",
		"linear.state_map.Waiting on External": "waiting_external",
	}})
	if got := pullStatus(t, config, &State{Name: "Waiting on External", Type: "started"}); got != types.StatusInProgress {
		t.Errorf("pull status with unconfigured custom status = %q, want in_progress", got)
	}
}

func TestParseBeadsStatusInReviewAndCustom(t *testing.T) {
	custom := []types.CustomStatus{{Name: "waiting_external", Category: types.CategoryWIP}}

	tests := []struct {
		input  string
		custom []types.CustomStatus
		want   types.Status
	}{
		{"in_review", nil, types.StatusInReview},
		{"In_Review", nil, types.StatusInReview},
		{"in-review", nil, types.StatusInReview},
		{"inreview", nil, types.StatusInReview},
		{"waiting_external", custom, types.Status("waiting_external")},
		{" Waiting_External ", custom, types.Status("waiting_external")},
		// Custom statuses are only accepted when configured.
		{"waiting_external", nil, types.StatusOpen},
		{"not_configured", custom, types.StatusOpen},
		{"", custom, types.StatusOpen},
	}

	for _, tt := range tests {
		if got := ParseBeadsStatus(tt.input, tt.custom...); got != tt.want {
			t.Errorf("ParseBeadsStatus(%q, custom=%v) = %q, want %q", tt.input, tt.custom, got, tt.want)
		}
	}
}
