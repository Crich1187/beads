package main

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// commentPullSearchStore answers SearchIssues only; any other tracker.Store
// call panics on the nil embedded interface.
type commentPullSearchStore struct {
	tracker.Store
	issues []*types.Issue
}

func (s *commentPullSearchStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	return s.issues, nil
}

func commentPullRef(s string) *string { return &s }

func TestBuildLinearCommentPullTargetsExclusions(t *testing.T) {
	st := &commentPullSearchStore{issues: []*types.Issue{
		{ID: "bd-open", Status: types.StatusOpen, ExternalRef: commentPullRef("https://linear.app/team/issue/ENG-1/a")},
		{ID: "bd-closed", Status: types.StatusClosed, ExternalRef: commentPullRef("https://linear.app/team/issue/ENG-2/b")},
		{ID: "bd-pinned", Status: types.StatusPinned, ExternalRef: commentPullRef("https://linear.app/team/issue/ENG-3/c")},
		{ID: "bd-hooked", Status: types.StatusHooked, ExternalRef: commentPullRef("https://linear.app/team/issue/ENG-4/d")},
		{ID: "bd-wisp", Status: types.StatusOpen, Ephemeral: true, ExternalRef: commentPullRef("https://linear.app/team/issue/ENG-5/e")},
		{ID: "bd-gh", Status: types.StatusOpen, ExternalRef: commentPullRef("https://github.com/o/r/issues/6")},
		{ID: "bd-local", Status: types.StatusOpen},
	}}
	targets, err := buildLinearCommentPullTargets(context.Background(), st, &linear.Tracker{})
	if err != nil {
		t.Fatalf("build targets: %v", err)
	}
	var got []string
	for _, tg := range targets {
		got = append(got, tg.BeadID+"="+tg.Identifier)
	}
	if strings.Join(got, ",") != "bd-open=ENG-1,bd-closed=ENG-2" {
		t.Fatalf("targets = %v", got)
	}
}

func TestPullLinearCommentsWarnsWhenStoreCannotImport(t *testing.T) {
	var warnings []string
	st := &commentPullSearchStore{}
	pullLinearCommentsForStore(context.Background(), st, st, &linear.Tracker{}, false, true, &warnings)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "comment pull: skipped") {
		t.Fatalf("warnings = %v", warnings)
	}
}
