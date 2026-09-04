package validation

import (
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// MissingSection describes a section that should be present but isn't.
type MissingSection struct {
	Heading string // The expected heading, e.g., "## Steps to Reproduce"
	Hint    string // Guidance for what to include
}

// TemplateError is returned when template validation fails.
// It contains all missing sections for a single error report.
type TemplateError struct {
	IssueType types.IssueType
	Missing   []MissingSection
}

func (e *TemplateError) Error() string {
	if len(e.Missing) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "missing required sections for %s:", e.IssueType)
	for _, m := range e.Missing {
		fmt.Fprintf(&b, "\n  - %s (%s)", m.Heading, m.Hint)
	}
	b.WriteString("\nSee 'bd create --help' for required sections by type.")
	return b.String()
}

// requiredSectionsHelpTypes is the stable display order for create --help.
// Only types with RequiredSections() entries are rendered.
var requiredSectionsHelpTypes = []types.IssueType{
	types.TypeBug,
	types.TypeFeature,
	types.TypeTask,
	types.TypeStory,
	types.TypeEpic,
	types.TypeDecision,
	types.TypeSpike,
}

// FormatRequiredSectionsHelp returns the discoverable required-section list for
// bd create --help. Content is derived from IssueType.RequiredSections so help
// cannot drift from --validate enforcement.
func FormatRequiredSectionsHelp() string {
	var b strings.Builder
	b.WriteString("Required description sections by --type (enforced with --validate):\n")
	for _, issueType := range requiredSectionsHelpTypes {
		sections := issueType.RequiredSections()
		if len(sections) == 0 {
			continue
		}
		headings := make([]string, len(sections))
		for i, section := range sections {
			headings[i] = section.Heading
		}
		fmt.Fprintf(&b, "  %-10s %s\n", string(issueType)+":", strings.Join(headings, ", "))
	}
	b.WriteString("Matching is case-insensitive; heading text may appear without markdown ## prefixes.")
	return b.String()
}

// ValidateFlagHelp is the --validate flag description shown by bd create --help.
// It names the decision sections that historically were undiscoverable from help alone.
func ValidateFlagHelp() string {
	return "Validate description contains required sections for issue type " +
		"(decision: ## Decision, ## Rationale, ## Alternatives Considered; see full list above)"
}

// ValidateTemplate checks if the description contains all required sections
// for the given issue type. Returns nil if validation passes or if the
// issue type has no required sections.
//
// Section matching is case-insensitive and looks for the heading text
// anywhere in the description (doesn't require exact markdown format).
func ValidateTemplate(issueType types.IssueType, description string) error {
	required := issueType.RequiredSections()
	if len(required) == 0 {
		return nil
	}

	descLower := strings.ToLower(description)
	var missing []MissingSection

	for _, section := range required {
		// Extract the heading text without markdown prefix for flexible matching
		// e.g., "## Steps to Reproduce" -> "steps to reproduce"
		headingText := strings.TrimPrefix(section.Heading, "## ")
		headingText = strings.TrimPrefix(headingText, "# ")
		headingLower := strings.ToLower(headingText)

		if !strings.Contains(descLower, headingLower) {
			missing = append(missing, MissingSection{
				Heading: section.Heading,
				Hint:    section.Hint,
			})
		}
	}

	if len(missing) > 0 {
		return &TemplateError{
			IssueType: issueType,
			Missing:   missing,
		}
	}
	return nil
}

// LintIssue checks an existing issue for missing template sections.
// Unlike ValidateTemplate, this operates on a full Issue struct.
// It checks both Description and AcceptanceCriteria fields.
// A non-empty AcceptanceCriteria field satisfies the "Acceptance Criteria"
// (or "Success Criteria" for epics) requirement without needing a heading. (GH#2468)
// Returns nil if the issue passes validation or has no requirements.
func LintIssue(issue *types.Issue) error {
	if issue == nil {
		return nil
	}
	text := issue.Description
	if issue.AcceptanceCriteria != "" {
		text = text + "\n" + issue.AcceptanceCriteria
	}
	err := ValidateTemplate(issue.IssueType, text)
	if err == nil || issue.AcceptanceCriteria == "" {
		return err
	}

	// A non-empty AcceptanceCriteria field satisfies "Acceptance Criteria"
	// or "Success Criteria" requirements even without the heading text.
	templateErr, ok := err.(*TemplateError)
	if !ok {
		return err
	}
	var remaining []MissingSection
	for _, m := range templateErr.Missing {
		heading := strings.ToLower(strings.TrimPrefix(m.Heading, "## "))
		if heading == "acceptance criteria" || heading == "success criteria" {
			continue // satisfied by the dedicated field
		}
		remaining = append(remaining, m)
	}
	if len(remaining) == 0 {
		return nil
	}
	templateErr.Missing = remaining
	return templateErr
}

// ValidateCloseReason checks if a close reason meets minimum quality standards.
// Returns nil if the reason is acceptable. Used by validation.on-close config.
func ValidateCloseReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || strings.EqualFold(reason, "closed") {
		return fmt.Errorf("close reason is empty or default; provide a summary of what was done")
	}
	if len(reason) < 20 {
		return fmt.Errorf("close reason is terse (%d chars); aim for 20+ characters describing what was done", len(reason))
	}
	return nil
}
