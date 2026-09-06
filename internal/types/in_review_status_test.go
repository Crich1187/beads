package types

import "testing"

// Regression tests for root-w0xy1: the live store legitimately contains the
// status "in_review" (written by bd's own tracker syncs, e.g. GitLab/Jira
// mapping), but the built-in status set omitted it. The exporter dumps what
// the store holds, so a bd export could not be re-imported by bd — breaking
// the disaster-recovery / auto-import round-trip.

func TestInReviewIsBuiltInStatus(t *testing.T) {
	s := Status("in_review")
	if !s.IsValid() {
		t.Errorf(`Status("in_review").IsValid() = false, want true: in_review is written by bd's own tracker syncs and present in live stores, so it must be a built-in status for export/import round-trip (root-w0xy1)`)
	}
	if got := BuiltInStatusCategory(s); got != CategoryWIP {
		t.Errorf(`BuiltInStatusCategory("in_review") = %q, want %q (visible in bd list, excluded from bd ready)`, got, CategoryWIP)
	}
	if !builtInStatusNames["in_review"] {
		t.Errorf(`builtInStatusNames is missing "in_review": custom-status collision checks must treat it as built-in`)
	}
}

func TestValidateAcceptsInReview(t *testing.T) {
	issue := &Issue{Title: "awaiting review", Status: Status("in_review"), IssueType: TypeBug, Priority: 1}
	if err := issue.Validate(); err != nil {
		t.Errorf("Validate() with status in_review = %v, want nil", err)
	}
	if err := issue.ValidateForImport(nil); err != nil {
		t.Errorf("ValidateForImport(nil) with status in_review = %v, want nil", err)
	}
}
