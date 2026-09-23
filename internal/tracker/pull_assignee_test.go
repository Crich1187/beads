package tracker

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// Agent rules from the fleet Owner registry (convention and athena), in the
// form the sync runner passes them as linear.protected_assignee_patterns.
var testProtectedAssigneePatterns = []string{
	`(?P<profile>[a-z0-9][a-z0-9_.-]*)/(?P<runtime>[a-z][a-z0-9_-]*):(?P<session>.+)`,
	`athena(?:\s*\(.*\))?|hermes`,
}

type assigneePullTx struct {
	storage.IssueLifecycleTransaction
	issue   *types.Issue
	updates []map[string]interface{}
}

func (t *assigneePullTx) UpdateIssue(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
	t.updates = append(t.updates, updates)
	if assignee, ok := updates["assignee"].(string); ok {
		t.issue.Assignee = assignee
	}
	return nil
}

type assigneePullStore struct {
	*pureTestStore
	tx *assigneePullTx
}

func (s *assigneePullStore) RunInIssueLifecycleTransaction(_ context.Context, _ string, fn func(storage.IssueLifecycleTransaction) error) error {
	return fn(s.tx)
}

func (s *assigneePullStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	for _, issue := range s.issues {
		if issue.ExternalRef != nil && *issue.ExternalRef == ref {
			return issue, nil
		}
	}
	return nil, nil
}

// runAssigneePull pulls one remote issue, identical to the local one except
// for the assignee, and returns the pull stats and the store's transaction.
func runAssigneePull(t *testing.T, localAssignee, remoteAssignee string, patterns []string) (*SyncResult, *assigneePullTx, error) {
	t.Helper()
	ref := "https://test.test/EXT-1"
	local := &types.Issue{
		ID: "bd-assignee", Title: "same", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask, Assignee: localAssignee, ExternalRef: &ref,
	}
	tx := &assigneePullTx{issue: local}
	store := &assigneePullStore{pureTestStore: newPureTestStore(local), tx: tx}
	mock := newMockTracker("test")
	mock.issues = []TrackerIssue{{ID: "EXT-1", Identifier: "EXT-1", URL: ref, Title: "same"}}
	mock.fieldMapper = &mockMapper{issueToBeads: func(*TrackerIssue) *IssueConversion {
		return &IssueConversion{Issue: &types.Issue{
			ID: "bd-assignee", Title: "same", Status: types.StatusOpen,
			Priority: 2, IssueType: types.TypeTask, Assignee: remoteAssignee,
		}}
	}}
	result, err := NewEngine(mock, store, "sync").Sync(context.Background(), SyncOptions{
		Pull: true, ProtectedAssigneePatterns: patterns,
	})
	return result, tx, err
}

func TestPullAssigneeProtection(t *testing.T) {
	const claim = "linear-sync-builder/claude:9fd3b280-B"
	for _, tc := range []struct {
		name     string
		local    string
		remote   string
		patterns []string
		want     string
		written  bool
	}{
		// Acceptance: remote assignee empty -> local untouched.
		{name: "empty remote leaves local untouched", local: "Carl Richards", remote: "", patterns: testProtectedAssigneePatterns, want: "Carl Richards"},
		// Empty remote never clears local, even with no patterns configured.
		{name: "empty remote with no patterns leaves claim untouched", local: claim, remote: "", patterns: nil, want: claim},
		// Acceptance: local assignee is an agent claim -> untouched even when remote is a human.
		{name: "agent claim survives human remote", local: claim, remote: "Carl Richards", patterns: testProtectedAssigneePatterns, want: claim},
		{name: "athena alias survives human remote", local: "Athena (PepperGateway)", remote: "carl@lymarinc.com", patterns: testProtectedAssigneePatterns, want: "Athena (PepperGateway)"},
		// Acceptance: remote human + local empty -> written.
		{name: "human remote fills empty local", local: "", remote: "Carl Richards", patterns: testProtectedAssigneePatterns, want: "Carl Richards", written: true},
		// With no patterns a non-empty remote overwrites as before.
		{name: "no patterns keeps overwrite behavior", local: claim, remote: "Carl Richards", patterns: nil, want: "Carl Richards", written: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, tx, err := runAssigneePull(t, tc.local, tc.remote, tc.patterns)
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if tx.issue.Assignee != tc.want {
				t.Fatalf("assignee = %q, want %q", tx.issue.Assignee, tc.want)
			}
			if tc.written {
				if result.PullStats.Updated != 1 || len(tx.updates) != 1 || tx.updates[0]["assignee"] != tc.remote {
					t.Fatalf("want one update writing assignee %q; stats=%+v updates=%v", tc.remote, result.PullStats, tx.updates)
				}
				return
			}
			// The only difference is the assignee, so a protected pull is a
			// no-op: no update, not even one that omits the assignee.
			if result.PullStats.Updated != 0 || result.PullStats.Skipped != 1 || len(tx.updates) != 0 {
				t.Fatalf("want no update; stats=%+v updates=%v", result.PullStats, tx.updates)
			}
		})
	}
}

func TestPullAssigneeOmittedWhenOtherFieldsChange(t *testing.T) {
	const claim = "linear-sync-builder/claude:9fd3b280-B"
	existing := &types.Issue{Title: "old", Assignee: claim}
	for _, remote := range []*types.Issue{
		{Title: "new", Assignee: ""},
		{Title: "new", Assignee: "Carl Richards"},
	} {
		protected, err := compileProtectedAssignees(testProtectedAssigneePatterns)
		if err != nil {
			t.Fatal(err)
		}
		updates := buildPullIssueUpdates(existing, remote, "", protected)
		if _, ok := updates["assignee"]; ok {
			t.Fatalf("remote %q: updates contain assignee: %v", remote.Assignee, updates)
		}
		if updates["title"] != "new" {
			t.Fatalf("remote %q: title not updated: %v", remote.Assignee, updates)
		}
	}
}

func TestPullAssigneeInvalidPatternFailsClosed(t *testing.T) {
	_, tx, err := runAssigneePull(t, "", "Carl Richards", []string{`qa(?![a-z])`})
	if err == nil || !strings.Contains(err.Error(), "invalid protected assignee pattern") {
		t.Fatalf("Sync error = %v, want invalid pattern error", err)
	}
	if len(tx.updates) != 0 || tx.issue.Assignee != "" {
		t.Fatalf("invalid pattern still wrote: assignee=%q updates=%v", tx.issue.Assignee, tx.updates)
	}
}
