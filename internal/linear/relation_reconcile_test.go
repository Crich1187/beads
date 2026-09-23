package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// relationMock is a stateful GraphQL fake for the relation pass: creates
// and deletes change what the next IssueRelationsByIdentifier fetch returns,
// so tests can prove idempotency across consecutive runs.
//
// Routing is by exact operation token so the two mutations (both containing
// "issueRelation") can never be confused:
//   - "IssueRelationsByIdentifier" → issue + outgoing relations
//   - "issueRelationCreate("       → records input, appends relation
//   - "issueRelationDelete("       → records id, removes relation
type relationMock struct {
	t  *testing.T
	mu sync.Mutex

	issues map[string]*relationIssue // identifier → issue (with relations)

	creates []map[string]interface{} // recorded issueRelationCreate inputs
	deletes []string                 // recorded issueRelationDelete ids
	fetches map[string]int           // identifier → fetch count

	hasNextPage     map[string]bool // identifier → report pageInfo.hasNextPage
	failCreate      bool            // create returns a GraphQL error, nothing lands
	landThenFail    bool            // create lands, then returns a GraphQL error
	failDelete      bool            // delete returns a GraphQL error, nothing removed
	serverAssignsID string          // when set, create ignores the client id and uses this
}

func newRelationMock(t *testing.T) *relationMock {
	return &relationMock{
		t:           t,
		issues:      map[string]*relationIssue{},
		fetches:     map[string]int{},
		hasNextPage: map[string]bool{},
	}
}

// addIssue registers an issue "TEAM-N" with UUID "uuid-N".
func (m *relationMock) addIssue(identifier string) *relationIssue {
	ri := &relationIssue{ID: "uuid-" + strings.TrimPrefix(identifier, "TEAM-"), Identifier: identifier}
	m.issues[identifier] = ri
	return ri
}

// addRelation adds an outgoing relation on from → to of the given type.
func (m *relationMock) addRelation(id, relType, from, to string) {
	rel := Relation{ID: id, Type: relType}
	rel.RelatedIssue.ID = m.issues[to].ID
	rel.RelatedIssue.Identifier = to
	m.issues[from].Relations.Nodes = append(m.issues[from].Relations.Nodes, rel)
}

// relationsOf returns (type, relatedIdentifier, id) triples for an issue.
func (m *relationMock) relationsOf(identifier string) []Relation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Relation(nil), m.issues[identifier].Relations.Nodes...)
}

func (m *relationMock) byUUID(id string) *relationIssue {
	for _, ri := range m.issues {
		if ri.ID == id {
			return ri
		}
	}
	return nil
}

func (m *relationMock) mutationCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.creates) + len(m.deletes)
}

func writeGQLError(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"errors": []map[string]interface{}{{"message": msg}},
	})
}

func (m *relationMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req GraphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Fatalf("mock: bad request JSON: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.Contains(req.Query, "IssueRelationsByIdentifier"):
		filter, _ := req.Variables["filter"].(map[string]interface{})
		number, _ := filter["number"].(map[string]interface{})
		eq, _ := number["eq"].(float64)
		nodes := []interface{}{}
		for ident, ri := range m.issues {
			if ident != "TEAM-"+itoa(int(eq)) {
				continue
			}
			m.fetches[ident]++
			rels := []interface{}{}
			for _, rel := range ri.Relations.Nodes {
				rels = append(rels, map[string]interface{}{
					"id":   rel.ID,
					"type": rel.Type,
					"relatedIssue": map[string]interface{}{
						"id": rel.RelatedIssue.ID, "identifier": rel.RelatedIssue.Identifier,
					},
				})
			}
			nodes = append(nodes, map[string]interface{}{
				"id":         ri.ID,
				"identifier": ri.Identifier,
				"relations": map[string]interface{}{
					"nodes":    rels,
					"pageInfo": map[string]interface{}{"hasNextPage": m.hasNextPage[ident]},
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"issues": map[string]interface{}{"nodes": nodes}},
		})
		return

	case strings.Contains(req.Query, "issueRelationCreate("):
		input, _ := req.Variables["input"].(map[string]interface{})
		m.creates = append(m.creates, input)
		if m.failCreate {
			writeGQLError(w, "create failed")
			return
		}
		blocker := m.byUUID(input["issueId"].(string))
		blocked := m.byUUID(input["relatedIssueId"].(string))
		if blocker == nil || blocked == nil {
			writeGQLError(w, "entity not found")
			return
		}
		id, _ := input["id"].(string)
		if m.serverAssignsID != "" {
			id = m.serverAssignsID
		}
		rel := Relation{ID: id, Type: input["type"].(string)}
		rel.RelatedIssue.ID = blocked.ID
		rel.RelatedIssue.Identifier = blocked.Identifier
		blocker.Relations.Nodes = append(blocker.Relations.Nodes, rel)
		if m.landThenFail {
			writeGQLError(w, "gateway timeout after commit")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"issueRelationCreate": map[string]interface{}{
					"success":       true,
					"issueRelation": map[string]interface{}{"id": id, "type": rel.Type},
				},
			},
		})
		return

	case strings.Contains(req.Query, "issueRelationDelete("):
		id, _ := req.Variables["id"].(string)
		m.deletes = append(m.deletes, id)
		if m.failDelete {
			writeGQLError(w, "delete failed")
			return
		}
		removed := false
		for _, ri := range m.issues {
			nodes := ri.Relations.Nodes[:0]
			for _, rel := range ri.Relations.Nodes {
				if rel.ID == id {
					removed = true
					continue
				}
				nodes = append(nodes, rel)
			}
			ri.Relations.Nodes = nodes
		}
		if !removed {
			writeGQLError(w, "entity not found")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"issueRelationDelete": map[string]interface{}{"success": true}},
		})
		return
	}
	m.t.Fatalf("mock: unexpected query: %s", req.Query)
}

// ledgerSink records every ledger write, keyed by bead id.
type ledgerSink struct {
	writes int
	latest map[string][]OwnedRelation
}

func newLedgerSink() *ledgerSink { return &ledgerSink{latest: map[string][]OwnedRelation{}} }

func (s *ledgerSink) persist(_ context.Context, beadID string, ledger []OwnedRelation) error {
	s.writes++
	s.latest[beadID] = append([]OwnedRelation(nil), ledger...)
	return nil
}

func setupRelationMock(t *testing.T, idents ...string) (*relationMock, *Tracker) {
	t.Helper()
	m := newRelationMock(t)
	for _, id := range idents {
		m.addIssue(id)
	}
	server := httptest.NewServer(m)
	t.Cleanup(server.Close)
	return m, newTestLinearTracker(t, server.URL)
}

// Add: a missing blocks link is created exactly once, with the blocker on
// the issueId side and the blocked bead on the relatedIssueId side, and its
// id is written to the blocked bead's ledger before the create is sent.
func TestReconcileRelations_AddCreatesWithCorrectDirection(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	sink := newLedgerSink()

	// Bead TEAM-1 depends on TEAM-2 (blocks) → TEAM-2 blocks TEAM-1.
	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{
		{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}},
	}, false, sink.persist)
	if err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	if stats.Created != 1 || stats.Deleted != 0 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want Created=1", stats)
	}
	if len(m.creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(m.creates))
	}
	in := m.creates[0]
	if in["issueId"] != "uuid-2" || in["relatedIssueId"] != "uuid-1" || in["type"] != "blocks" {
		t.Fatalf("create input = %v, want issueId=uuid-2 (blocker) relatedIssueId=uuid-1 (blocked) type=blocks", in)
	}
	ledger := sink.latest["bd-a"]
	if len(ledger) != 1 {
		t.Fatalf("ledger = %+v, want 1 entry", ledger)
	}
	want := OwnedRelation{ID: in["id"].(string), Type: "blocks", Blocker: "TEAM-2", Blocked: "TEAM-1"}
	if ledger[0] != want || want.ID == "" {
		t.Fatalf("ledger[0] = %+v, want %+v", ledger[0], want)
	}
	rels := m.relationsOf("TEAM-2")
	if len(rels) != 1 || rels[0].ID != want.ID || rels[0].RelatedIssue.Identifier != "TEAM-1" {
		t.Fatalf("TEAM-2 relations = %+v", rels)
	}
	if len(m.relationsOf("TEAM-1")) != 0 {
		t.Fatalf("blocked side must carry no outgoing relation: %+v", m.relationsOf("TEAM-1"))
	}
}

// No-op / idempotent: a second run fed the first run's persisted ledger
// issues zero mutations and zero ledger writes.
func TestReconcileRelations_IdempotentRerun(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2", "TEAM-3")
	sink := newLedgerSink()
	bead := BlockedBead{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2", "TEAM-3"}}

	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if len(m.creates) != 2 {
		t.Fatalf("run 1 creates = %d, want 2", len(m.creates))
	}
	mutationsAfter1, writesAfter1 := m.mutationCount(), sink.writes

	bead.Ledger = sink.latest["bd-a"]
	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if m.mutationCount() != mutationsAfter1 {
		t.Fatalf("run 2 issued %d mutations, want 0", m.mutationCount()-mutationsAfter1)
	}
	if sink.writes != writesAfter1 {
		t.Fatalf("run 2 wrote the ledger %d times, want 0", sink.writes-writesAfter1)
	}
	if stats.Skipped != 2 || stats.Created != 0 || stats.Deleted != 0 || stats.Released != 0 {
		t.Fatalf("run 2 stats = %+v, want Skipped=2 only", stats)
	}
}

// Remove: when the blocks dependency disappears from beads, the relation
// this integration created is deleted and dropped from the ledger; a third
// run is a no-op.
func TestReconcileRelations_RemoveDeletesOwnedRelation(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	sink := newLedgerSink()
	bead := BlockedBead{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}}
	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	createdID := sink.latest["bd-a"][0].ID

	// Dependency removed in beads.
	bead.Blockers = nil
	bead.Ledger = sink.latest["bd-a"]
	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if stats.Deleted != 1 || len(m.deletes) != 1 || m.deletes[0] != createdID {
		t.Fatalf("stats = %+v deletes = %v, want exactly delete of %s", stats, m.deletes, createdID)
	}
	if len(m.relationsOf("TEAM-2")) != 0 {
		t.Fatalf("relation still present in Linear: %+v", m.relationsOf("TEAM-2"))
	}
	if got := sink.latest["bd-a"]; len(got) != 0 {
		t.Fatalf("ledger after delete = %+v, want empty", got)
	}

	before := m.mutationCount()
	bead.Ledger = sink.latest["bd-a"]
	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if m.mutationCount() != before {
		t.Fatalf("run 3 issued mutations; want none")
	}
}

// Foreign-relation preservation: with no desired blockers, the pass deletes
// nothing it did not create — a manual blocks relation (not in the ledger),
// a related relation, and a ledgered relation whose type a human changed to
// related all survive. The retyped one is released from the ledger.
func TestReconcileRelations_PreservesForeignRelations(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2", "TEAM-3", "TEAM-4")
	m.addRelation("manual-blocks", "blocks", "TEAM-2", "TEAM-1")
	m.addRelation("manual-related", "related", "TEAM-3", "TEAM-1")
	m.addRelation("ours-retyped", "related", "TEAM-4", "TEAM-1")
	sink := newLedgerSink()

	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{{
		BeadID:     "bd-a",
		Identifier: "TEAM-1",
		Blockers:   nil,
		Ledger:     []OwnedRelation{{ID: "ours-retyped", Type: "blocks", Blocker: "TEAM-4", Blocked: "TEAM-1"}},
	}}, false, sink.persist)
	if err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	if len(m.deletes) != 0 || len(m.creates) != 0 {
		t.Fatalf("mutations: creates=%v deletes=%v, want none", m.creates, m.deletes)
	}
	if len(m.relationsOf("TEAM-2")) != 1 || len(m.relationsOf("TEAM-3")) != 1 || len(m.relationsOf("TEAM-4")) != 1 {
		t.Fatalf("foreign relations were touched")
	}
	if stats.Released != 1 {
		t.Fatalf("Released = %d, want 1 (retyped relation leaves the ledger)", stats.Released)
	}
	if got := sink.latest["bd-a"]; len(got) != 0 {
		t.Fatalf("ledger = %+v, want empty", got)
	}
}

// A manual blocks relation satisfies a desired link (no duplicate create) but
// is not adopted into the ledger, so removing the dependency later leaves it.
func TestReconcileRelations_ManualBlocksSatisfiesButIsNotAdopted(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.addRelation("manual-blocks", "blocks", "TEAM-2", "TEAM-1")
	sink := newLedgerSink()
	bead := BlockedBead{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}}

	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if stats.Skipped != 1 || len(m.creates) != 0 || sink.writes != 0 {
		t.Fatalf("stats=%+v creates=%d writes=%d, want Skipped=1 and nothing else", stats, len(m.creates), sink.writes)
	}

	bead.Blockers = nil
	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(m.deletes) != 0 || len(m.relationsOf("TEAM-2")) != 1 {
		t.Fatalf("manual relation was deleted: deletes=%v", m.deletes)
	}
}

// A related relation between the same pair does not satisfy a desired
// blocks link: a blocks relation is created alongside it, and the related
// relation is untouched.
func TestReconcileRelations_OtherTypeDoesNotSatisfyBlocks(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.addRelation("manual-related", "related", "TEAM-2", "TEAM-1")
	sink := newLedgerSink()

	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{
		{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}},
	}, false, sink.persist); err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	rels := m.relationsOf("TEAM-2")
	if len(rels) != 2 || rels[0].ID != "manual-related" || rels[0].Type != "related" || rels[1].Type != "blocks" {
		t.Fatalf("TEAM-2 relations = %+v, want related kept + new blocks", rels)
	}
	if len(m.deletes) != 0 {
		t.Fatalf("deletes = %v, want none", m.deletes)
	}
}

// Dry run: reads happen, but no create, no delete, and no ledger write.
func TestReconcileRelations_DryRunNoMutations(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2", "TEAM-3")
	m.addRelation("ours", "blocks", "TEAM-3", "TEAM-1")
	sink := newLedgerSink()

	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{{
		BeadID:     "bd-a",
		Identifier: "TEAM-1",
		Blockers:   []string{"TEAM-2"},
		Ledger:     []OwnedRelation{{ID: "ours", Type: "blocks", Blocker: "TEAM-3", Blocked: "TEAM-1"}},
	}}, true, sink.persist)
	if err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	if m.mutationCount() != 0 || sink.writes != 0 {
		t.Fatalf("dry run mutated: mutations=%d writes=%d", m.mutationCount(), sink.writes)
	}
	if stats.WouldCreate != 1 || stats.WouldDelete != 1 || stats.Created != 0 || stats.Deleted != 0 {
		t.Fatalf("stats = %+v, want WouldCreate=1 WouldDelete=1", stats)
	}
	if len(stats.Mutations) != 2 {
		t.Fatalf("Mutations = %+v, want 2 planned", stats.Mutations)
	}
}

// Fail closed on a truncated relation page: error, no mutations.
func TestReconcileRelations_PaginationOverflowFailsClosed(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.hasNextPage["TEAM-2"] = true
	sink := newLedgerSink()

	_, err := tr.ReconcileRelations(context.Background(), []BlockedBead{
		{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}},
	}, false, sink.persist)
	if err == nil || !strings.Contains(err.Error(), "more relations than one page") {
		t.Fatalf("err = %v, want pagination refusal", err)
	}
	if m.mutationCount() != 0 || sink.writes != 0 {
		t.Fatalf("mutated despite refusal: mutations=%d writes=%d", m.mutationCount(), sink.writes)
	}
}

// Fail closed on create error: the pass returns the error, the ledger id was
// written before the create, and a rerun (API healthy) converges to exactly
// one relation with no stale ledger entry.
func TestReconcileRelations_CreateErrorFailsClosedAndRecovers(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.failCreate = true
	sink := newLedgerSink()
	bead := BlockedBead{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}}

	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist); err == nil {
		t.Fatal("expected error from failed create")
	}
	if got := sink.latest["bd-a"]; len(got) != 1 {
		t.Fatalf("ledger must be pre-written before create; got %+v", got)
	}

	m.failCreate = false
	bead.Ledger = sink.latest["bd-a"]
	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if stats.Created != 1 || stats.Released != 1 {
		t.Fatalf("rerun stats = %+v, want Created=1 Released=1", stats)
	}
	rels := m.relationsOf("TEAM-2")
	ledger := sink.latest["bd-a"]
	if len(rels) != 1 || len(ledger) != 1 || ledger[0].ID != rels[0].ID {
		t.Fatalf("rels=%+v ledger=%+v, want one relation owned by the ledger", rels, ledger)
	}
}

// Ambiguous create (lands in Linear, then errors): the pass finds its own
// client-supplied id, treats it as created, and a rerun makes no mutation.
func TestReconcileRelations_AmbiguousCreateRecoveredByID(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.landThenFail = true
	sink := newLedgerSink()
	bead := BlockedBead{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}}

	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist)
	if err != nil {
		t.Fatalf("ambiguous create should be recovered: %v", err)
	}
	if stats.Created != 1 || len(m.relationsOf("TEAM-2")) != 1 {
		t.Fatalf("stats=%+v rels=%+v", stats, m.relationsOf("TEAM-2"))
	}
	m.landThenFail = false
	before := m.mutationCount()
	bead.Ledger = sink.latest["bd-a"]
	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{bead}, false, sink.persist); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if m.mutationCount() != before {
		t.Fatalf("rerun mutated after recovered create")
	}
}

// If Linear assigns its own relation id, the ledger is re-pointed to it.
func TestReconcileRelations_ServerAssignedIDRecorded(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.serverAssignsID = "server-id"
	sink := newLedgerSink()
	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{
		{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-2"}},
	}, false, sink.persist); err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	if got := sink.latest["bd-a"]; len(got) != 1 || got[0].ID != "server-id" {
		t.Fatalf("ledger = %+v, want id server-id", got)
	}
}

// Fail closed on delete error: error returned, ownership retained so the
// next run retries the delete.
func TestReconcileRelations_DeleteErrorKeepsOwnership(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2")
	m.addRelation("ours", "blocks", "TEAM-2", "TEAM-1")
	m.failDelete = true
	sink := newLedgerSink()
	ledger := []OwnedRelation{{ID: "ours", Type: "blocks", Blocker: "TEAM-2", Blocked: "TEAM-1"}}

	_, err := tr.ReconcileRelations(context.Background(), []BlockedBead{
		{BeadID: "bd-a", Identifier: "TEAM-1", Ledger: ledger},
	}, false, sink.persist)
	if err == nil {
		t.Fatal("expected error from failed delete")
	}
	if got, written := sink.latest["bd-a"]; written && len(got) != 1 {
		t.Fatalf("ledger lost ownership after failed delete: %+v", got)
	}
	if len(m.relationsOf("TEAM-2")) != 1 {
		t.Fatalf("relation unexpectedly gone")
	}
}

// A blocker shared by several blocked beads is fetched once.
func TestReconcileRelations_SharedBlockerFetchedOnce(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1", "TEAM-2", "TEAM-9")
	sink := newLedgerSink()
	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{
		{BeadID: "bd-a", Identifier: "TEAM-1", Blockers: []string{"TEAM-9"}},
		{BeadID: "bd-b", Identifier: "TEAM-2", Blockers: []string{"TEAM-9"}},
	}, false, sink.persist)
	if err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	if stats.Created != 2 || m.fetches["TEAM-9"] != 1 {
		t.Fatalf("stats=%+v fetches[TEAM-9]=%d, want Created=2 and 1 fetch", stats, m.fetches["TEAM-9"])
	}
}

// Missing Linear issues are reported, not mutated, and ledger entries for an
// unreachable blocker are kept.
func TestReconcileRelations_NotFoundLeavesLedger(t *testing.T) {
	m, tr := setupRelationMock(t, "TEAM-1")
	sink := newLedgerSink()
	stats, err := tr.ReconcileRelations(context.Background(), []BlockedBead{{
		BeadID:     "bd-a",
		Identifier: "TEAM-1",
		Blockers:   []string{"TEAM-7"},
		Ledger:     []OwnedRelation{{ID: "ours", Type: "blocks", Blocker: "TEAM-8", Blocked: "TEAM-1"}},
	}}, false, sink.persist)
	if err != nil {
		t.Fatalf("ReconcileRelations: %v", err)
	}
	if m.mutationCount() != 0 || sink.writes != 0 {
		t.Fatalf("mutations=%d writes=%d, want none", m.mutationCount(), sink.writes)
	}
	if len(stats.NotFound) != 2 {
		t.Fatalf("NotFound = %v, want TEAM-7 and TEAM-8", stats.NotFound)
	}
}

func TestReconcileRelations_EmptyAndNoWriter(t *testing.T) {
	tr := newTestLinearTracker(t, "http://127.0.0.1:0")
	stats, err := tr.ReconcileRelations(context.Background(), nil, false, nil)
	if err != nil || stats == nil {
		t.Fatalf("empty input: stats=%v err=%v", stats, err)
	}
	if _, err := tr.ReconcileRelations(context.Background(), []BlockedBead{{BeadID: "x", Identifier: "TEAM-1"}}, false, nil); err == nil {
		t.Fatal("wet run without a ledger writer must fail closed")
	}
}
