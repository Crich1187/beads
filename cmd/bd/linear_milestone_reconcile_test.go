package main

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// milestonePassStore records whether the milestone pass read the store.
// When failOnRead is set, any read fails the test: a scoped sync must not
// even enumerate local beads.
type milestonePassStore struct {
	tracker.Store
	t          *testing.T
	failOnRead bool
	reads      int
	issues     []*types.Issue
	deps       map[string][]*types.IssueWithDependencyMetadata
}

func (s *milestonePassStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	s.reads++
	if s.failOnRead {
		s.t.Fatal("milestone pass read the store during a scoped sync")
	}
	return s.issues, nil
}

func (s *milestonePassStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*types.IssueWithDependencyMetadata, error) {
	s.reads++
	if s.failOnRead {
		s.t.Fatal("milestone pass read the store during a scoped sync")
	}
	return s.deps[id], nil
}

// Scoped sync skips: --parent, --issue-id and --type syncs never run the
// milestone pass (same gate as the parent reconcile pass).
func TestReconcileLinearMilestonesSkipsScopedSync(t *testing.T) {
	for name, opts := range map[string]*tracker.SyncOptions{
		"parent":   {ParentID: "bd-foo"},
		"issue-id": {IssueIDs: []string{"bd-1"}},
		"type":     {TypeFilter: []types.IssueType{types.TypeTask}},
	} {
		st := &milestonePassStore{t: t, failOnRead: true}
		var warnings []string
		reconcileLinearMilestonesForStore(context.Background(), st, &linear.Tracker{}, opts, false, true, &warnings)
		if st.reads != 0 || len(warnings) != 0 {
			t.Fatalf("%s: reads=%d warnings=%v, want 0/none", name, st.reads, warnings)
		}
	}
}

// Unscoped sync runs the pass, and a failure surfaces as a
// "milestone reconcile:" warning rather than being swallowed.
func TestReconcileLinearMilestonesUnscopedRunsAndSurfacesErrors(t *testing.T) {
	msRef := "linear:project-milestone:ms-a"
	childRef := "https://linear.app/team/issue/TEST-1/child"
	ms := &types.Issue{ID: "bd-ms", ExternalRef: &msRef, IssueType: types.TypeEpic}
	child := &types.Issue{ID: "bd-child", ExternalRef: &childRef}
	st := &milestonePassStore{
		t:      t,
		issues: []*types.Issue{ms, child},
		deps: map[string][]*types.IssueWithDependencyMetadata{
			"bd-child": {{Issue: *ms, DependencyType: types.DepParentChild}},
		},
	}
	var warnings []string
	// A Tracker with no client makes ReconcileMilestones fail at setup.
	reconcileLinearMilestonesForStore(context.Background(), st, &linear.Tracker{}, &tracker.SyncOptions{}, false, true, &warnings)
	if st.reads == 0 {
		t.Fatal("unscoped sync did not run the milestone pass")
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "milestone reconcile: ") {
		t.Fatalf("warnings = %v, want one milestone reconcile warning", warnings)
	}
}
