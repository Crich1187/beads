package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// Live evidence, acceptance run 20260924T162500Z (T08): bead
// Test-Sandbox-o6s (the Needs Carl template) as stored in bd, and TEST-4 as
// Linear stored it after the push. Linear inserted a blank line after every
// "## " heading; every cycle then re-sent the unchanged description.
const (
	evidenceO6SBead     = "Parked for a decision (positive fixture: Needs Carl template present).\n\n## Decision needed\nWhether sandbox epics map to a Linear project milestone.\n\n## Options\n1. One milestone per epic.\n2. No milestones in the sandbox.\n3. Defer until the milestone pass (cycle step 6) lands.\n\n## Recommendation\nOption 3.\n\n## Evidence\nTopology 5.7 step 6 is absent from the household build (bd-baseline-comparison-20260922).\n\n## Cost of waiting\nNone for the sandbox; production binding is gated on 001.21.\n\n## Next step\nCarl answers in a Linear comment; the comment pass delivers it and the agent resumes."
	evidenceTEST4Linear = "Parked for a decision (positive fixture: Needs Carl template present).\n\n## Decision needed\n\nWhether sandbox epics map to a Linear project milestone.\n\n## Options\n\n1. One milestone per epic.\n2. No milestones in the sandbox.\n3. Defer until the milestone pass (cycle step 6) lands.\n\n## Recommendation\n\nOption 3.\n\n## Evidence\n\nTopology 5.7 step 6 is absent from the household build (bd-baseline-comparison-20260922).\n\n## Cost of waiting\n\nNone for the sandbox; production binding is gated on 001.21.\n\n## Next step\n\nCarl answers in a Linear comment; the comment pass delivers it and the agent resumes."
	// Same run: the harness sent "[acc22 suite, ...]" and Linear stored the
	// brackets escaped (TEST-11, TEST-15).
	evidenceBracketSent   = "[acc22 suite, Carl-side action via TEST key] pollution probe (bound)"
	evidenceBracketLinear = "\\[acc22 suite, Carl-side action via TEST key\\] pollution probe (bound)"
)

func TestLinearDescriptionsEqualIgnoresLinearRenormalization(t *testing.T) {
	for _, tc := range []struct {
		name          string
		local, remote string
	}{
		{name: "evidence: blank line inserted after headings (TEST-4)", local: evidenceO6SBead, remote: evidenceTEST4Linear},
		{name: "evidence: escaped brackets (TEST-15)", local: evidenceBracketSent, remote: evidenceBracketLinear},
		{name: "CRLF line endings", local: "## A\r\nbody\r\n", remote: "## A\n\nbody"},
		{name: "trailing whitespace and trailing newlines", local: "line one  \nline two\t\n\n", remote: "line one\nline two"},
		{name: "extra blank lines after a heading", local: "## A\n\n\n\nbody", remote: "## A\n\nbody"},
		{name: "heading as the last line", local: "body\n\n## Next step", remote: "body\n\n## Next step\n"},
		{name: "heading directly before a code fence", local: "## Code\n```\nx\n```", remote: "## Code\n\n```\nx\n```"},
		{name: "all heading levels", local: "# A\na\n\n###### F\nf", remote: "# A\n\na\n\n###### F\n\nf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !linearDescriptionsEqual(tc.local, tc.remote) {
				t.Fatalf("descriptions compare unequal:\nlocal:  %q\nremote: %q\nnorm local:  %q\nnorm remote: %q",
					tc.local, tc.remote, normalizeLinearDescriptionForCompare(tc.local), normalizeLinearDescriptionForCompare(tc.remote))
			}
		})
	}
}

func TestLinearDescriptionsEqualDetectsRealEdits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		local, remote string
	}{
		{name: "a word changed", local: strings.Replace(evidenceO6SBead, "Option 3.", "Option 2.", 1), remote: evidenceTEST4Linear},
		{name: "a line added", local: evidenceO6SBead + "\nCarl: approved.", remote: evidenceTEST4Linear},
		{name: "a list item added under a heading", local: strings.Replace(evidenceO6SBead, "1. One milestone per epic.", "1. One milestone per epic.\n1b. Two milestones per epic.", 1), remote: evidenceTEST4Linear},
		{name: "a section removed", local: strings.Replace(evidenceO6SBead, "\n\n## Evidence\nTopology 5.7 step 6 is absent from the household build (bd-baseline-comparison-20260922).", "", 1), remote: evidenceTEST4Linear},
		{name: "a paragraph break removed", local: "first\nsecond", remote: "first\n\nsecond"},
		{name: "heading text changed", local: "## Decision\nbody", remote: "## Decision needed\n\nbody"},
		{name: "code fence content is not normalized", local: "```\n## not a heading\nx\n```", remote: "```\n## not a heading\n\nx\n```"},
		{name: "hashtag is not a heading", local: "#tag\nbody", remote: "#tag\n\nbody"},
		{name: "leading indentation changed", local: "    indented", remote: "indented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if linearDescriptionsEqual(tc.local, tc.remote) {
				t.Fatalf("real edit compared equal:\nlocal:  %q\nremote: %q", tc.local, tc.remote)
			}
		})
	}
}

func TestPushFieldsEqualTreatsLinearNormalizedDescriptionAsEqual(t *testing.T) {
	cfg := DefaultMappingConfig()
	cfg.ExplicitStateMap = map[string]string{"backlog": "open"}
	local := &types.Issue{ID: "Test-Sandbox-o6s", Title: "T", Description: evidenceO6SBead, Status: types.StatusOpen, Priority: 4}
	remote := &Issue{Title: "T", Description: evidenceTEST4Linear, Priority: 0, State: &State{ID: "s", Name: "Backlog", Type: "backlog"}}
	if !PushFieldsEqual(local, remote, cfg, nil) {
		t.Fatal("PushFieldsEqual: Linear-renormalized description reported as changed")
	}
	if !PushFieldsEqualToBeads(local, &types.Issue{Title: "T", Description: evidenceTEST4Linear, Status: types.StatusOpen, Priority: 4}) {
		t.Fatal("PushFieldsEqualToBeads: Linear-renormalized description reported as changed")
	}
	edited := *local
	edited.Description = strings.Replace(evidenceO6SBead, "Option 3.", "Option 1.", 1)
	if PushFieldsEqual(&edited, remote, cfg, nil) {
		t.Fatal("PushFieldsEqual: a real description edit compared equal")
	}
}

// TestBatchPush_SkipsLinearNormalizedDescription reproduces T08 against the
// fake Linear server: the remote holds Linear's re-serialized form of the
// bead's description, so BatchPush must send no issueUpdate. When the bead
// really changed, the update carries the bead's text byte-for-byte (the
// normalization is compare-only).
func TestBatchPush_SkipsLinearNormalizedDescription(t *testing.T) {
	for _, tc := range []struct {
		name       string
		local      string
		wantUpdate bool
	}{
		{name: "unchanged bead is skipped", local: evidenceO6SBead},
		{name: "edited bead is pushed verbatim", local: strings.Replace(evidenceO6SBead, "Option 3.", "Option 1.", 1), wantUpdate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sentDescriptions []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req GraphQLRequest
				_ = json.Unmarshal(body, &req)
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(req.Query, "TeamStates"):
					_ = json.NewEncoder(w).Encode(teamStatesResp("team-1", "state-open", "Backlog", "backlog"))
				case strings.Contains(req.Query, "TeamLabels"):
					_ = json.NewEncoder(w).Encode(teamLabelsEmptyResp("team-1"))
				case strings.Contains(req.Query, "IssueByIdentifier"):
					_ = json.NewEncoder(w).Encode(issueByIdentifierResp("remote-uuid", "TEST-4", "Needs Carl", evidenceTEST4Linear, 0, "state-open", "Backlog", "backlog"))
				case strings.Contains(req.Query, "issueUpdate"):
					input, _ := req.Variables["input"].(map[string]interface{})
					desc, _ := input["description"].(string)
					sentDescriptions = append(sentDescriptions, desc)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"issueUpdate": map[string]interface{}{
						"success": true,
						"issue":   map[string]interface{}{"id": "remote-uuid", "url": "https://linear.app/team/issue/TEST-4/needs-carl", "updatedAt": "2026-01-01T00:00:00Z"},
					}}})
				}
			}))
			defer server.Close()

			cfg := DefaultMappingConfig()
			cfg.ExplicitStateMap = map[string]string{"backlog": "open"}
			ref := "https://linear.app/team/issue/TEST-4"
			local := &types.Issue{ID: "Test-Sandbox-o6s", Title: "Needs Carl", Description: tc.local, Status: types.StatusOpen, Priority: 4, ExternalRef: &ref}
			tr := &Tracker{
				teamIDs: []string{"team-1"},
				clients: map[string]*Client{"team-1": NewClient("key", "team-1").WithEndpoint(server.URL)},
				config:  cfg,
			}
			result, err := tr.BatchPush(context.Background(), []*types.Issue{local}, nil)
			if err != nil {
				t.Fatalf("BatchPush: %v", err)
			}
			if !tc.wantUpdate {
				if len(sentDescriptions) != 0 || len(result.Skipped) != 1 {
					t.Fatalf("want skip and no issueUpdate; sent=%d skipped=%v updated=%v", len(sentDescriptions), result.Skipped, result.Updated)
				}
				return
			}
			if len(sentDescriptions) != 1 || sentDescriptions[0] != tc.local {
				t.Fatalf("want one issueUpdate carrying the bead text verbatim; got %q", sentDescriptions)
			}
		})
	}
}
