package issueops

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// Regression for root-w0xy1: the JSONL import path (bd import, bd bootstrap,
// and maybeAutoImportJSONL) validates every imported issue through
// PrepareIssueForInsert, which rejected the status "in_review" that bd's own
// tracker syncs write. An export containing that status could not be
// re-imported, so disaster recovery from issues.jsonl failed.
func TestPrepareForInsertAcceptsInReview(t *testing.T) {
	issue := &types.Issue{
		ID:        "test-xq8ie",
		Title:     "Poison: in_review status round-trip",
		Status:    types.Status("in_review"),
		IssueType: types.TypeBug,
		Priority:  1,
	}
	if err := PrepareIssueForInsert(issue, nil, nil); err != nil {
		t.Fatalf("PrepareIssueForInsert rejected status in_review: %v", err)
	}
}

// The invalid-status guard must keep rejecting statuses that are neither
// built-in nor configured — in_review becomes built-in, not a validation bypass.
func TestPrepareForInsertStillRejectsUnknownStatus(t *testing.T) {
	issue := &types.Issue{
		ID:        "test-bad1",
		Title:     "Not a real status",
		Status:    types.Status("in_reveiw"), // typo'd, not built-in
		IssueType: types.TypeBug,
		Priority:  1,
	}
	if err := PrepareIssueForInsert(issue, nil, nil); err == nil {
		t.Fatal("PrepareIssueForInsert accepted unknown status in_reveiw, want error")
	}
}
