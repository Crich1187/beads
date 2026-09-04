//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestCreateValidateDecisionEmbedded covers root-Papercut-BeadsCreate-010
// acceptance beyond help text: actionable missing-section diagnostics, valid
// decision creation in an isolated scratch DB, and no mutation on failure.
func TestCreateValidateDecisionEmbedded(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt create validate tests")
	}

	bd := buildEmbeddedBD(t)

	t.Run("missing_sections_diagnostics_no_mutation", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "pcv", "--non-interactive", "--skip-hooks", "--skip-agents")

		out := bdCreateFail(t, bd, dir,
			"--type", "decision",
			"--title", "Design planning decision",
			"--description", "Planning notes without required sections",
			"--validate",
		)
		for _, want := range []string{
			"## Decision",
			"## Rationale",
			"## Alternatives Considered",
			"bd create --help",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("validation error missing %q\n%s", want, out)
			}
		}

		listOut, err := bdRunWithFlockRetry(t, bd, dir, "list", "--json")
		if err != nil {
			t.Fatalf("bd list --json failed: %v\n%s", err, listOut)
		}
		var issues []map[string]any
		if err := json.Unmarshal(listOut, &issues); err != nil {
			t.Fatalf("parse list json: %v\n%s", err, listOut)
		}
		if len(issues) != 0 {
			t.Fatalf("validation failure mutated DB: got %d issues, want 0", len(issues))
		}
	})

	t.Run("valid_decision_creates", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "pcd", "--non-interactive", "--skip-hooks", "--skip-agents")
		desc := `## Decision
Use Forgejo remotes for beads Dolt backups.
## Rationale
Self-hosted and already fleet-standard.
## Alternatives Considered
GitHub private remotes — rejected due to self-host bias.`
		issue := bdCreate(t, bd, dir,
			"--type", "decision",
			"--title", "Use Forgejo for beads backups",
			"--description", desc,
			"--validate",
		)
		if issue.ID == "" {
			t.Fatal("expected issue ID")
		}
		if !strings.HasPrefix(issue.ID, "pcd-") {
			t.Errorf("unexpected id prefix: %s", issue.ID)
		}
		if string(issue.IssueType) != "decision" {
			t.Errorf("type: got %q, want decision", issue.IssueType)
		}
	})
}
