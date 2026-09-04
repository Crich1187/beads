package main

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/validation"
)

// TestCreateHelpShowsDecisionRequiredSections guards the papercut fixed by
// root-Papercut-BeadsCreate-010: bd create --help must make Decision /
// Rationale / Alternatives requirements discoverable before --validate rejects.
//
// Asserts the cobra Long text and --validate flag usage that --help renders,
// plus a captured Help() pass (cobra Help writes via os.Stdout here).
func TestCreateHelpShowsDecisionRequiredSections(t *testing.T) {
	required := []string{
		"## Decision",
		"## Rationale",
		"## Alternatives Considered",
		"Required description sections by --type",
	}
	for _, want := range required {
		if !strings.Contains(createCmd.Long, want) {
			t.Errorf("createCmd.Long missing %q\n%s", want, createCmd.Long)
		}
	}

	flag := createCmd.Flags().Lookup("validate")
	if flag == nil {
		t.Fatal("missing --validate flag")
	}
	for _, want := range []string{"## Decision", "## Rationale", "## Alternatives Considered"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("--validate usage missing %q: %s", want, flag.Usage)
		}
	}

	help := captureStdout(t, createCmd.Help)
	for _, want := range append(required, "--validate") {
		if !strings.Contains(help, want) {
			t.Errorf("bd create --help missing %q\nhelp:\n%s", want, help)
		}
	}
}

func TestValidateFlagHelpNamesDecisionSections(t *testing.T) {
	help := validation.ValidateFlagHelp()
	for _, want := range []string{"## Decision", "## Rationale", "## Alternatives Considered"} {
		if !strings.Contains(help, want) {
			t.Errorf("ValidateFlagHelp missing %q: %s", want, help)
		}
	}
}

func TestFormatRequiredSectionsHelpMatchesRequiredSections(t *testing.T) {
	help := validation.FormatRequiredSectionsHelp()
	if !strings.Contains(help, "decision:") {
		t.Fatalf("help missing decision type:\n%s", help)
	}
	for _, want := range []string{"## Decision", "## Rationale", "## Alternatives Considered"} {
		if !strings.Contains(help, want) {
			t.Errorf("FormatRequiredSectionsHelp missing %q:\n%s", want, help)
		}
	}
}
