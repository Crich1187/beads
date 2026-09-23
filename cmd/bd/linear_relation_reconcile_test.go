package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/types"
)

// relationLedgerStore extends parentLinkStore with the two calls the ledger
// writer needs (re-read by external_ref, metadata update).
type relationLedgerStore struct {
	parentLinkStore
	updates map[string]map[string]interface{}
}

func (s *relationLedgerStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	for _, issue := range s.issues {
		if issue.ExternalRef != nil && *issue.ExternalRef == ref {
			return issue, nil
		}
	}
	return nil, nil
}

func (s *relationLedgerStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	if s.updates == nil {
		s.updates = map[string]map[string]interface{}{}
	}
	s.updates[id] = updates
	return nil
}

// Direction: bead A (ENG-2) depends on bead B (ENG-1) via a blocks dep, so
// B blocks A: A is the blocked bead and ENG-1 is its blocker. Parent-child
// and other dependency types are ignored; a blocker without a Linear ref is
// skipped.
func TestBuildLinearBlockedBeadsForStore_Direction(t *testing.T) {
	refB := "https://linear.app/team/issue/ENG-1/blocker"
	refA := "https://linear.app/team/issue/ENG-2/blocked"
	refP := "https://linear.app/team/issue/ENG-3/parent"
	b := &types.Issue{ID: "bd-b", ExternalRef: &refB}
	a := &types.Issue{ID: "bd-a", ExternalRef: &refA}
	p := &types.Issue{ID: "bd-p", ExternalRef: &refP}
	unsynced := &types.Issue{ID: "bd-u"}
	st := &parentLinkStore{
		issues: []*types.Issue{a, b, p, unsynced},
		deps: map[string][]*types.IssueWithDependencyMetadata{
			"bd-a": {
				{Issue: *b, DependencyType: types.DepBlocks},
				{Issue: *p, DependencyType: types.DepParentChild},
				{Issue: *unsynced, DependencyType: types.DepBlocks},
			},
		},
	}
	beads, refs, err := buildLinearBlockedBeadsForStore(context.Background(), st, &linear.Tracker{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(beads) != 1 {
		t.Fatalf("beads = %#v, want only bd-a", beads)
	}
	got := beads[0]
	if got.BeadID != "bd-a" || got.Identifier != "ENG-2" || len(got.Blockers) != 1 || got.Blockers[0] != "ENG-1" {
		t.Fatalf("blocked bead = %#v, want bd-a/ENG-2 blocked by ENG-1", got)
	}
	if refs["bd-a"] != refA {
		t.Fatalf("refs[bd-a] = %q", refs["bd-a"])
	}
}

// A blocker missing from the SearchIssues result (e.g. filtered out) but
// present as a hydrated dependency target with a Linear ref still counts,
// so the pass never deletes a relation whose dependency still exists.
func TestBuildLinearBlockedBeadsForStore_BlockerOutsideSearchStillDesired(t *testing.T) {
	refB := "https://linear.app/team/issue/ENG-1/blocker"
	refA := "https://linear.app/team/issue/ENG-2/blocked"
	b := &types.Issue{ID: "bd-b", ExternalRef: &refB, Status: types.StatusClosed}
	a := &types.Issue{ID: "bd-a", ExternalRef: &refA}
	st := &parentLinkStore{
		issues: []*types.Issue{a}, // bd-b deliberately absent from search
		deps: map[string][]*types.IssueWithDependencyMetadata{
			"bd-a": {{Issue: *b, DependencyType: types.DepBlocks}},
		},
	}
	beads, _, err := buildLinearBlockedBeadsForStore(context.Background(), st, &linear.Tracker{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(beads) != 1 || len(beads[0].Blockers) != 1 || beads[0].Blockers[0] != "ENG-1" {
		t.Fatalf("beads = %#v, want bd-a blocked by ENG-1", beads)
	}
}

// A bead with no blocks deps left but a non-empty ledger is still emitted,
// so the removal half of the pass can run.
func TestBuildLinearBlockedBeadsForStore_LedgerOnlyBeadIncluded(t *testing.T) {
	refA := "https://linear.app/team/issue/ENG-2/blocked"
	a := &types.Issue{
		ID:          "bd-a",
		ExternalRef: &refA,
		Metadata:    json.RawMessage(`{"linear":{"relations":[{"id":"r1","type":"blocks","blocker":"ENG-1","blocked":"ENG-2"}]}}`),
	}
	st := &parentLinkStore{issues: []*types.Issue{a}}
	beads, _, err := buildLinearBlockedBeadsForStore(context.Background(), st, &linear.Tracker{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(beads) != 1 || len(beads[0].Blockers) != 0 || len(beads[0].Ledger) != 1 || beads[0].Ledger[0].ID != "r1" {
		t.Fatalf("beads = %#v", beads)
	}
}

// Malformed ledger metadata fails closed instead of silently dropping
// ownership records.
func TestBuildLinearBlockedBeadsForStore_MalformedLedgerErrors(t *testing.T) {
	refA := "https://linear.app/team/issue/ENG-2/blocked"
	a := &types.Issue{ID: "bd-a", ExternalRef: &refA, Metadata: json.RawMessage(`{"linear":{"relations":"oops"}}`)}
	st := &parentLinkStore{issues: []*types.Issue{a}}
	if _, _, err := buildLinearBlockedBeadsForStore(context.Background(), st, &linear.Tracker{}); err == nil {
		t.Fatal("expected error for malformed ledger")
	}
}

// The ledger merge touches only linear.relations: top-level keys and
// sibling keys inside "linear" (milestone data) survive, and an empty
// ledger removes the key.
func TestMergeLinearRelationLedgerPreservesSiblings(t *testing.T) {
	existing := json.RawMessage(`{"other":1,"linear":{"kind":"project_milestone","project_milestone":{"id":"m1"}}}`)
	ledger := []linear.OwnedRelation{{ID: "r1", Type: "blocks", Blocker: "ENG-1", Blocked: "ENG-2"}}
	merged, err := mergeLinearRelationLedger(existing, ledger)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got struct {
		Other  int `json:"other"`
		Linear struct {
			Kind             string                 `json:"kind"`
			ProjectMilestone map[string]interface{} `json:"project_milestone"`
		} `json:"linear"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Other != 1 || got.Linear.Kind != "project_milestone" || got.Linear.ProjectMilestone["id"] != "m1" {
		t.Fatalf("siblings lost: %s", merged)
	}
	round, err := linearRelationLedger(merged)
	if err != nil || len(round) != 1 || round[0] != ledger[0] {
		t.Fatalf("round trip = %#v err=%v", round, err)
	}

	cleared, err := mergeLinearRelationLedger(merged, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if round, _ := linearRelationLedger(cleared); len(round) != 0 {
		t.Fatalf("ledger not cleared: %s", cleared)
	}
	if string(cleared) != `{"linear":{"kind":"project_milestone","project_milestone":{"id":"m1"}},"other":1}` {
		t.Fatalf("cleared metadata = %s", cleared)
	}

	if _, err := mergeLinearRelationLedger(json.RawMessage(`[1]`), ledger); err == nil {
		t.Fatal("non-object metadata must error")
	}
}

// The writer re-reads the bead, writes merged metadata, and skips no-ops.
func TestWriteLinearRelationLedger(t *testing.T) {
	refA := "https://linear.app/team/issue/ENG-2/blocked"
	a := &types.Issue{ID: "bd-a", ExternalRef: &refA, Metadata: json.RawMessage(`{"keep":true}`)}
	st := &relationLedgerStore{parentLinkStore: parentLinkStore{issues: []*types.Issue{a}}}
	ledger := []linear.OwnedRelation{{ID: "r1", Type: "blocks", Blocker: "ENG-1", Blocked: "ENG-2"}}

	if err := writeLinearRelationLedger(context.Background(), st, "bd-a", refA, ledger); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, ok := st.updates["bd-a"]["metadata"].(json.RawMessage)
	if !ok {
		t.Fatalf("updates = %#v, want metadata json.RawMessage", st.updates)
	}
	if string(raw) != `{"keep":true,"linear":{"relations":[{"id":"r1","type":"blocks","blocker":"ENG-1","blocked":"ENG-2"}]}}` {
		t.Fatalf("metadata = %s", raw)
	}

	// No-op when the ledger is already in place.
	a.Metadata = raw
	st.updates = nil
	if err := writeLinearRelationLedger(context.Background(), st, "bd-a", refA, ledger); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if st.updates != nil {
		t.Fatalf("no-op write issued an update: %#v", st.updates)
	}

	// Refuses to write when the ref resolves to a different bead.
	if err := writeLinearRelationLedger(context.Background(), st, "bd-other", refA, ledger); err == nil {
		t.Fatal("expected mismatch error")
	}
}
