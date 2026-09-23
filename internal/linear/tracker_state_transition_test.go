package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// transitionTestStates is a team workflow that includes the Linear-only
// distinctions patch 3 must preserve (Backlog vs Todo, Canceled/Duplicate vs
// Done).
var transitionTestStates = []State{
	{ID: "st-backlog", Name: "Backlog", Type: "backlog"},
	{ID: "st-todo", Name: "Todo", Type: "unstarted"},
	{ID: "st-inprogress", Name: "In Progress", Type: "started"},
	{ID: "st-done", Name: "Done", Type: "completed"},
	{ID: "st-canceled", Name: "Canceled", Type: "canceled"},
	{ID: "st-duplicate", Name: "Duplicate", Type: "duplicate"},
}

// transitionTestConfig loads a mapping config the same way the tracker does
// at Init. Without outbound_state_map, "open" would resolve to Backlog (name
// match on the backlog entry) and "closed" would be ambiguous (Canceled and
// Duplicate), so a stateId of Todo or Done proves outbound_state_map was used.
func transitionTestConfig() *MappingConfig {
	return LoadMappingConfig(&mockConfigLoader{config: map[string]string{
		"linear.state_map.backlog":              "open",
		"linear.state_map.unstarted":            "open",
		"linear.state_map.started":              "in_progress",
		"linear.state_map.completed":            "closed",
		"linear.state_map.canceled":             "closed",
		"linear.state_map.duplicate":            "closed",
		"linear.outbound_state_map.open":        "Todo",
		"linear.outbound_state_map.in_progress": "In Progress",
		"linear.outbound_state_map.closed":      "Done",
	}})
}

// fakeLinearForTransitions is a fake Linear GraphQL endpoint holding one issue
// (TEAM-1) in a configurable current state. A nil remote state makes the
// issue lookup return no nodes, i.e. the remote state is unknown.
type fakeLinearForTransitions struct {
	mu          sync.Mutex
	remote      *State
	updateCalls int
	lastInput   map[string]interface{}
}

func (f *fakeLinearForTransitions) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GraphQLRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")

		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.Contains(req.Query, "TeamStates"):
			nodes := make([]interface{}, 0, len(transitionTestStates))
			for _, s := range transitionTestStates {
				nodes = append(nodes, map[string]interface{}{"id": s.ID, "name": s.Name, "type": s.Type})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"team": map[string]interface{}{
						"id":     "team-1",
						"states": map[string]interface{}{"nodes": nodes},
					},
				},
			})
		case strings.Contains(req.Query, "TeamLabels"):
			_ = json.NewEncoder(w).Encode(teamLabelsEmptyResp("team-1"))
		case strings.Contains(req.Query, "IssueByIdentifier"):
			if f.remote == nil {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"issues": map[string]interface{}{"nodes": []interface{}{}},
					},
				})
				return
			}
			// Remote description differs from the local one so the batch
			// path's PushFieldsEqual skip check does not short-circuit.
			_ = json.NewEncoder(w).Encode(issueByIdentifierResp(
				"uuid-1", "TEAM-1", "Same Title", "old description", 3,
				f.remote.ID, f.remote.Name, f.remote.Type,
			))
		case strings.Contains(req.Query, "issueUpdate"):
			f.updateCalls++
			f.lastInput, _ = req.Variables["input"].(map[string]interface{})
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"issueUpdate": map[string]interface{}{
						"success": true,
						"issue": map[string]interface{}{
							"id":         "uuid-1",
							"identifier": "TEAM-1",
							"url":        "https://linear.app/team/issue/TEAM-1",
							"updatedAt":  "2026-01-02T00:00:00Z",
						},
					},
				},
			})
		default:
			t.Errorf("unexpected query: %s", req.Query)
			http.Error(w, "unexpected query", http.StatusInternalServerError)
		}
	}
}

func stateByName(t *testing.T, name string) *State {
	t.Helper()
	for i := range transitionTestStates {
		if transitionTestStates[i].Name == name {
			s := transitionTestStates[i]
			return &s
		}
	}
	t.Fatalf("no test state named %q", name)
	return nil
}

type stateTransitionCase struct {
	name        string
	remoteState string // "" = remote issue not found (state unknown)
	beadStatus  types.Status
	wantStateID string // "" = stateId must be absent from the update
}

var stateTransitionCases = []stateTransitionCase{
	// Description-only edits: bead status already equals the pull mapping of
	// the current remote state, so the Linear-only state must be preserved.
	{name: "canceled description-only omits stateId", remoteState: "Canceled", beadStatus: types.StatusClosed},
	{name: "backlog description-only omits stateId", remoteState: "Backlog", beadStatus: types.StatusOpen},
	{name: "duplicate description-only omits stateId", remoteState: "Duplicate", beadStatus: types.StatusClosed},
	// Real transitions: stateId comes from outbound_state_map.
	{name: "reopen from canceled sends outbound open", remoteState: "Canceled", beadStatus: types.StatusOpen, wantStateID: "st-todo"},
	{name: "close from todo sends outbound closed", remoteState: "Todo", beadStatus: types.StatusClosed, wantStateID: "st-done"},
	{name: "start from backlog sends outbound in_progress", remoteState: "Backlog", beadStatus: types.StatusInProgress, wantStateID: "st-inprogress"},
	// Unknown remote state: conservative, stateId is sent.
	{name: "unknown remote state sends stateId", remoteState: "", beadStatus: types.StatusOpen, wantStateID: "st-todo"},
}

func assertStateIDDecision(t *testing.T, fake *fakeLinearForTransitions, tc stateTransitionCase) {
	t.Helper()
	if fake.updateCalls != 1 {
		t.Fatalf("issueUpdate calls = %d, want 1", fake.updateCalls)
	}
	if got, _ := fake.lastInput["description"].(string); got != "new description" {
		t.Errorf("description in update = %q, want %q", got, "new description")
	}
	got, present := fake.lastInput["stateId"]
	if tc.wantStateID == "" {
		if present {
			t.Errorf("stateId = %v present in update, want absent (remote %q, bead %q)", got, tc.remoteState, tc.beadStatus)
		}
		return
	}
	if !present {
		t.Fatalf("stateId absent from update, want %q (remote %q, bead %q)", tc.wantStateID, tc.remoteState, tc.beadStatus)
	}
	if got != tc.wantStateID {
		t.Errorf("stateId = %v, want %q", got, tc.wantStateID)
	}
}

func newTransitionFixture(t *testing.T, tc stateTransitionCase) (*fakeLinearForTransitions, *Tracker) {
	t.Helper()
	fake := &fakeLinearForTransitions{}
	if tc.remoteState != "" {
		fake.remote = stateByName(t, tc.remoteState)
	}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	tr := &Tracker{
		teamIDs: []string{"team-1"},
		clients: map[string]*Client{
			"team-1": NewClient("key", "team-1").WithEndpoint(server.URL),
		},
		config: transitionTestConfig(),
	}
	return fake, tr
}

func transitionLocalIssue(status types.Status) *types.Issue {
	extRef := "https://linear.app/team/issue/TEAM-1"
	return &types.Issue{
		ID:          "bd-1",
		Title:       "Same Title",
		Description: "new description",
		Status:      status,
		Priority:    2,
		ExternalRef: &extRef,
	}
}

// TestUpdateIssue_StateIDOnlyOnTransition covers the single-issue update path
// (Tracker.UpdateIssue, used by the sync engine's doPush).
func TestUpdateIssue_StateIDOnlyOnTransition(t *testing.T) {
	for _, tc := range stateTransitionCases {
		t.Run(tc.name, func(t *testing.T) {
			fake, tr := newTransitionFixture(t, tc)
			if _, err := tr.UpdateIssue(context.Background(), "TEAM-1", transitionLocalIssue(tc.beadStatus)); err != nil {
				t.Fatalf("UpdateIssue: %v", err)
			}
			assertStateIDDecision(t, fake, tc)
		})
	}
}

// TestBatchPush_StateIDOnlyOnTransition covers the batch update path, both
// with the content skip check active and with it bypassed via forceIDs.
func TestBatchPush_StateIDOnlyOnTransition(t *testing.T) {
	for _, forced := range []bool{false, true} {
		for _, tc := range stateTransitionCases {
			name := tc.name
			if forced {
				name = "forced/" + name
			}
			t.Run(name, func(t *testing.T) {
				fake, tr := newTransitionFixture(t, tc)
				local := transitionLocalIssue(tc.beadStatus)
				var forceIDs map[string]bool
				if forced {
					forceIDs = map[string]bool{local.ID: true}
				}
				result, err := tr.BatchPush(context.Background(), []*types.Issue{local}, forceIDs)
				if err != nil {
					t.Fatalf("BatchPush: %v", err)
				}
				if len(result.Errors) != 0 || len(result.Updated) != 1 {
					t.Fatalf("BatchPush result: updated=%v errors=%v skipped=%v", result.Updated, result.Errors, result.Skipped)
				}
				assertStateIDDecision(t, fake, tc)
			})
		}
	}
}

// TestShouldPushStateID pins the decision helper, including the conservative
// handling of an unknown remote state or missing config.
func TestShouldPushStateID(t *testing.T) {
	cfg := transitionTestConfig()
	if !shouldPushStateID(nil, types.StatusOpen, cfg) {
		t.Error("nil remote state: want stateId pushed (state unknown)")
	}
	if !shouldPushStateID(&State{Name: "Backlog", Type: "backlog"}, types.StatusOpen, nil) {
		t.Error("nil config: want stateId pushed")
	}
	if shouldPushStateID(&State{Name: "Canceled", Type: "canceled"}, types.StatusClosed, cfg) {
		t.Error("Canceled remote with closed bead: want stateId omitted")
	}
	if !shouldPushStateID(&State{Name: "Canceled", Type: "canceled"}, types.StatusOpen, cfg) {
		t.Error("Canceled remote with open bead: want stateId pushed")
	}
}
