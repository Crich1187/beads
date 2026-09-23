package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// milestoneMock is a stateful fake Linear GraphQL endpoint for the milestone
// pass. Unlike linearMockHandler it APPLIES projectMilestoneId updates to the
// stored issue, so a second pass observes the first pass's writes (needed to
// prove idempotent reruns). It counts every fetch and every issueUpdate.
type milestoneMock struct {
	t           *testing.T
	mu          sync.Mutex
	issues      map[string]*Issue // identifier → issue
	updateCalls int
	fetchCalls  int
	updates     []milestoneMockUpdate
	failUpdate  bool
}

type milestoneMockUpdate struct {
	IssueID string
	Input   map[string]interface{}
}

func newMilestoneMock(t *testing.T) *milestoneMock {
	return &milestoneMock{t: t, issues: map[string]*Issue{}}
}

func (m *milestoneMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req GraphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Fatalf("milestoneMock: bad request JSON: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.Contains(req.Query, "IssueByIdentifier"):
		m.fetchCalls++
		filter, _ := req.Variables["filter"].(map[string]interface{})
		number, _ := filter["number"].(map[string]interface{})
		eq, _ := number["eq"].(float64)
		nodes := []interface{}{}
		for ident, iss := range m.issues {
			if strings.HasSuffix(ident, "-"+itoa(int(eq))) {
				b, _ := json.Marshal(iss)
				var raw map[string]interface{}
				_ = json.Unmarshal(b, &raw)
				nodes = append(nodes, raw)
				break
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"issues": map[string]interface{}{"nodes": nodes}},
		})
	case strings.Contains(req.Query, "issueUpdate"):
		m.updateCalls++
		id, _ := req.Variables["id"].(string)
		input, _ := req.Variables["input"].(map[string]interface{})
		m.updates = append(m.updates, milestoneMockUpdate{IssueID: id, Input: input})
		if m.failUpdate {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"errors": []interface{}{map[string]interface{}{"message": "projectMilestoneId: milestone not in issue's project"}},
			})
			return
		}
		for _, iss := range m.issues {
			if iss.ID != id {
				continue
			}
			if msID, ok := input["projectMilestoneId"].(string); ok {
				iss.ProjectMilestone = &ProjectMilestone{ID: msID}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"issueUpdate": map[string]interface{}{
					"success": true,
					"issue":   map[string]interface{}{"id": id, "identifier": "ECHO"},
				},
			},
		})
	default:
		m.t.Errorf("milestoneMock: unexpected query: %s", req.Query)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{}})
	}
}

func (m *milestoneMock) counts() (fetches, updates int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fetchCalls, m.updateCalls
}

func newMilestoneTestTracker(t *testing.T, m *milestoneMock) *Tracker {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return newTestLinearTracker(t, srv.URL)
}

func (m *milestoneMock) setMilestone(ident string, msID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msID == "" {
		m.issues[ident].ProjectMilestone = nil
		return
	}
	m.issues[ident].ProjectMilestone = &ProjectMilestone{ID: msID}
}

func (m *milestoneMock) milestoneOf(ident string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pm := m.issues[ident].ProjectMilestone; pm != nil {
		return pm.ID
	}
	return ""
}

// milestoneFakeStore is an in-memory store implementing both
// MilestoneLinkStore and MilestoneMarkerStore. UpdateIssue persists the
// metadata blob so later passes observe the marker.
type milestoneFakeStore struct {
	issues      []*types.Issue
	deps        map[string][]*types.IssueWithDependencyMetadata
	searchErr   error
	depsErr     error
	updateErr   error
	updateCalls int
	onUpdate    func(id string)
}

func (s *milestoneFakeStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return s.issues, nil
}

func (s *milestoneFakeStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*types.IssueWithDependencyMetadata, error) {
	if s.depsErr != nil {
		return nil, s.depsErr
	}
	return s.deps[id], nil
}

func (s *milestoneFakeStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	for _, iss := range s.issues {
		if iss.ExternalRef != nil && *iss.ExternalRef == ref {
			return iss, nil
		}
	}
	return nil, errors.New("not found")
}

func (s *milestoneFakeStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	s.updateCalls++
	if s.onUpdate != nil {
		s.onUpdate(id)
	}
	if s.updateErr != nil {
		return s.updateErr
	}
	for _, iss := range s.issues {
		if iss.ID != id {
			continue
		}
		raw, ok := updates["metadata"].(json.RawMessage)
		if !ok || len(updates) != 1 {
			return errors.New("fake store: only a metadata update is expected")
		}
		iss.Metadata = raw
		return nil
	}
	return errors.New("fake store: no such bead")
}

func (s *milestoneFakeStore) bead(id string) *types.Issue {
	for _, iss := range s.issues {
		if iss.ID == id {
			return iss
		}
	}
	return nil
}

func (s *milestoneFakeStore) marker(t *testing.T, id string) string {
	t.Helper()
	marker, _, err := readMilestoneGuards(s.bead(id).Metadata)
	if err != nil {
		t.Fatalf("read marker of %s: %v", id, err)
	}
	return marker
}

func milestoneBead(id, ref string, issueType types.IssueType) *types.Issue {
	r := ref
	return &types.Issue{ID: id, ExternalRef: &r, IssueType: issueType}
}

func (s *milestoneFakeStore) parent(child string, parent *types.Issue) {
	if s.deps == nil {
		s.deps = map[string][]*types.IssueWithDependencyMetadata{}
	}
	s.deps[child] = append(s.deps[child], &types.IssueWithDependencyMetadata{Issue: *parent, DependencyType: types.DepParentChild})
}

// milestoneFixture: milestone epic bd-ms (ms-a) with direct children
// bd-c<N> → TEST-<N>, one per identifier number given.
func milestoneFixture(nums ...int) *milestoneFakeStore {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	st := &milestoneFakeStore{issues: []*types.Issue{ms}}
	for _, n := range nums {
		id := "bd-c" + itoa(n)
		st.issues = append(st.issues, milestoneBead(id, "https://linear.app/team/issue/TEST-"+itoa(n)+"/c", types.TypeTask))
		st.parent(id, ms)
	}
	return st
}

// runMilestonePass is the full pass as cmd/bd wires it: build links from the
// store, reconcile against the fake client, write markers to the store.
func runMilestonePass(t *testing.T, st *milestoneFakeStore, tr *Tracker, dryRun bool) (*MilestoneReconcileStats, error) {
	t.Helper()
	links, perBead, err := BuildMilestoneLinks(context.Background(), st)
	if err != nil {
		t.Fatalf("BuildMilestoneLinks: %v", err)
	}
	if len(perBead) != 0 {
		t.Fatalf("per-bead build errors: %v", perBead)
	}
	mark := func(ctx context.Context, link MilestoneLink, msID string) error {
		return WriteMilestoneAssignedMarker(ctx, st, link, msID, "test")
	}
	return tr.ReconcileMilestones(context.Background(), links, mark, dryRun)
}

// Sets when Linear is empty and there is no marker: exactly one issueUpdate
// carrying only projectMilestoneId, then the marker is recorded.
func TestMilestonePass_SetsWhenLinearEmptyAndNoMarker(t *testing.T) {
	st := milestoneFixture(1)
	m := newMilestoneMock(t)
	m.issues["TEST-1"] = &Issue{ID: "uuid-1", Identifier: "TEST-1"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if stats.Updated != 1 || len(stats.Errors) != 0 || len(stats.Preserved) != 0 {
		t.Fatalf("stats = %+v, want Updated=1", stats)
	}
	if len(m.updates) != 1 || len(m.updates[0].Input) != 1 || m.updates[0].Input["projectMilestoneId"] != "ms-a" {
		t.Fatalf("updates = %+v, want one update with only projectMilestoneId=ms-a", m.updates)
	}
	if got := st.marker(t, "bd-c1"); got != "ms-a" {
		t.Fatalf("marker = %q, want ms-a", got)
	}
}

// Carl's move is preserved: Linear holds a different milestone, so nothing
// is sent, no marker is written, and it is an informational skip only.
func TestMilestonePass_PreservesDifferentLinearMilestone(t *testing.T) {
	st := milestoneFixture(2)
	m := newMilestoneMock(t)
	m.issues["TEST-2"] = &Issue{ID: "uuid-2", Identifier: "TEST-2", ProjectMilestone: &ProjectMilestone{ID: "ms-carl"}}
	tr := newMilestoneTestTracker(t, m)

	stats, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if _, updates := m.counts(); updates != 0 {
		t.Fatalf("issueUpdate calls = %d, want 0", updates)
	}
	if len(stats.Preserved) != 1 || stats.Preserved[0].LinearMilestone != "ms-carl" || len(stats.Errors) != 0 {
		t.Fatalf("stats = %+v, want one Preserved and no errors", stats)
	}
	if st.updateCalls != 0 || m.milestoneOf("TEST-2") != "ms-carl" {
		t.Fatalf("store writes=%d linear=%q, want 0 / ms-carl", st.updateCalls, m.milestoneOf("TEST-2"))
	}
}

// No re-set after Carl clears: once the marker is present, a cleared Linear
// milestone stays cleared — zero fetches and zero updates.
func TestMilestonePass_NoResetAfterCarlClears(t *testing.T) {
	st := milestoneFixture(3)
	m := newMilestoneMock(t)
	m.issues["TEST-3"] = &Issue{ID: "uuid-3", Identifier: "TEST-3"}
	tr := newMilestoneTestTracker(t, m)
	if _, err := runMilestonePass(t, st, tr, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	m.setMilestone("TEST-3", "") // Carl clears it in Linear
	fetchesBefore, updatesBefore := m.counts()
	stats, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	fetches, updates := m.counts()
	if fetches != fetchesBefore || updates != updatesBefore {
		t.Fatalf("second pass fetches=%d updates=%d, want 0/0", fetches-fetchesBefore, updates-updatesBefore)
	}
	if stats.AlreadyAssigned != 1 || m.milestoneOf("TEST-3") != "" {
		t.Fatalf("stats=%+v linear=%q, want AlreadyAssigned=1 and still cleared", stats, m.milestoneOf("TEST-3"))
	}
}

// Pull replaces the whole bead metadata blob when the Linear issue has a
// milestone, which wipes the marker. The pulled project_milestone evidence
// must still block a re-set after Carl clears.
func TestMilestonePass_NoResetAfterPullWipesMarker(t *testing.T) {
	st := milestoneFixture(4)
	m := newMilestoneMock(t)
	m.issues["TEST-4"] = &Issue{ID: "uuid-4", Identifier: "TEST-4"}
	tr := newMilestoneTestTracker(t, m)
	if _, err := runMilestonePass(t, st, tr, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Exactly what the tracker engine writes on pull for an issue with a milestone.
	st.bead("bd-c4").Metadata = json.RawMessage(`{"linear":{"project_milestone":{"id":"ms-a","name":"A","description":"","progress":0}}}`)
	m.setMilestone("TEST-4", "") // then Carl clears it
	_, updatesBefore := m.counts()
	stats, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if _, updates := m.counts(); updates != updatesBefore {
		t.Fatalf("second pass issued %d updates, want 0", updates-updatesBefore)
	}
	if stats.AlreadyAssigned != 1 {
		t.Fatalf("stats = %+v, want AlreadyAssigned=1", stats)
	}
}

// Marker is written only after a successful Linear update: the store write
// observes the update already made, and a failed update writes no marker.
func TestMilestonePass_MarkerOnlyAfterSuccessfulUpdate(t *testing.T) {
	st := milestoneFixture(5)
	m := newMilestoneMock(t)
	m.issues["TEST-5"] = &Issue{ID: "uuid-5", Identifier: "TEST-5"}
	tr := newMilestoneTestTracker(t, m)
	st.onUpdate = func(string) {
		if _, updates := m.counts(); updates < 1 {
			t.Fatal("marker written before the Linear update")
		}
	}
	if _, err := runMilestonePass(t, st, tr, false); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if st.updateCalls != 1 {
		t.Fatalf("store writes = %d, want 1", st.updateCalls)
	}

	failSt := milestoneFixture(6)
	failM := newMilestoneMock(t)
	failM.failUpdate = true
	failM.issues["TEST-6"] = &Issue{ID: "uuid-6", Identifier: "TEST-6"}
	stats, err := runMilestonePass(t, failSt, newMilestoneTestTracker(t, failM), false)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(stats.Errors) != 1 || stats.Updated != 0 || failSt.updateCalls != 0 || failSt.marker(t, "bd-c6") != "" {
		t.Fatalf("stats=%+v writes=%d, want 1 error, no marker", stats, failSt.updateCalls)
	}
}

// A failed marker write surfaces as an error and aborts the pass: later
// links are not processed.
func TestMilestonePass_MarkerWriteFailureSurfaces(t *testing.T) {
	st := milestoneFixture(7, 8)
	st.updateErr = errors.New("dolt write refused")
	m := newMilestoneMock(t)
	m.issues["TEST-7"] = &Issue{ID: "uuid-7", Identifier: "TEST-7"}
	m.issues["TEST-8"] = &Issue{ID: "uuid-8", Identifier: "TEST-8"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := runMilestonePass(t, st, tr, false)
	if err == nil || !strings.Contains(err.Error(), "marker not recorded") || !strings.Contains(err.Error(), "dolt write refused") {
		t.Fatalf("err = %v, want marker write failure", err)
	}
	if stats == nil || stats.Updated != 1 {
		t.Fatalf("stats = %+v, want the one applied Linear update counted", stats)
	}
	if _, updates := m.counts(); updates != 1 {
		t.Fatalf("issueUpdate calls = %d, want 1 (pass must stop after the failed marker)", updates)
	}
}

// The marker write merges one nested key and keeps every sibling key.
func TestWriteMilestoneAssignedMarker_PreservesOtherMetadata(t *testing.T) {
	st := milestoneFixture(9)
	st.bead("bd-c9").Metadata = json.RawMessage(`{"owner":"athena","linear":{"relation_ledger":["rel-1"],"comment_watermark":"c-9"}}`)
	link := MilestoneLink{BeadID: "bd-c9", ExternalRef: *st.bead("bd-c9").ExternalRef}
	if err := WriteMilestoneAssignedMarker(context.Background(), st, link, "ms-a", "test"); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(st.bead("bd-c9").Metadata, &got); err != nil {
		t.Fatal(err)
	}
	lin, _ := got["linear"].(map[string]interface{})
	if got["owner"] != "athena" || lin["comment_watermark"] != "c-9" || lin["relation_ledger"] == nil || lin["milestone_assigned"] != "ms-a" {
		t.Fatalf("metadata = %s", st.bead("bd-c9").Metadata)
	}

	// Wrong bead behind the ref is refused.
	if err := WriteMilestoneAssignedMarker(context.Background(), st, MilestoneLink{BeadID: "bd-other", ExternalRef: link.ExternalRef}, "ms-a", "test"); err == nil {
		t.Fatal("want error when the ref resolves to a different bead")
	}
}

// Linear already equal (e.g. marker write failed last time): no-op.
func TestMilestonePass_NoOpWhenEqual(t *testing.T) {
	st := milestoneFixture(10)
	m := newMilestoneMock(t)
	m.issues["TEST-10"] = &Issue{ID: "uuid-10", Identifier: "TEST-10", ProjectMilestone: &ProjectMilestone{ID: "ms-a"}}
	tr := newMilestoneTestTracker(t, m)

	stats, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if stats.Skipped != 1 || stats.Updated != 0 || len(stats.Preserved) != 0 {
		t.Fatalf("stats = %+v, want Skipped=1", stats)
	}
	if _, updates := m.counts(); updates != 0 || st.updateCalls != 0 {
		t.Fatalf("linear updates=%d store writes=%d, want 0/0", updates, st.updateCalls)
	}
}

// Idempotent rerun: a second pass makes zero Linear mutations, zero store
// writes and zero fetches (every bead now carries the marker).
func TestMilestonePass_IdempotentRerun(t *testing.T) {
	st := milestoneFixture(11, 12)
	m := newMilestoneMock(t)
	m.issues["TEST-11"] = &Issue{ID: "uuid-11", Identifier: "TEST-11"}
	m.issues["TEST-12"] = &Issue{ID: "uuid-12", Identifier: "TEST-12"}
	tr := newMilestoneTestTracker(t, m)

	first, err := runMilestonePass(t, st, tr, false)
	if err != nil || first.Updated != 2 {
		t.Fatalf("first pass stats=%+v err=%v, want Updated=2", first, err)
	}
	fetches1, updates1 := m.counts()
	writes1 := st.updateCalls

	second, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	fetches2, updates2 := m.counts()
	if updates2 != updates1 || fetches2 != fetches1 || st.updateCalls != writes1 {
		t.Fatalf("second pass: fetches +%d updates +%d writes +%d, want 0/0/0",
			fetches2-fetches1, updates2-updates1, st.updateCalls-writes1)
	}
	if second.AlreadyAssigned != 2 || second.Updated != 0 {
		t.Fatalf("second pass stats = %+v, want AlreadyAssigned=2", second)
	}
}

// Dry-run: fetches run, nothing is written to Linear or to the store.
func TestMilestonePass_DryRun(t *testing.T) {
	st := milestoneFixture(13)
	m := newMilestoneMock(t)
	m.issues["TEST-13"] = &Issue{ID: "uuid-13", Identifier: "TEST-13"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := runMilestonePass(t, st, tr, true)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if stats.WouldUpdate != 1 || stats.Updated != 0 || len(stats.Mutations) != 1 {
		t.Fatalf("stats = %+v, want WouldUpdate=1", stats)
	}
	if fetches, updates := m.counts(); fetches != 1 || updates != 0 || st.updateCalls != 0 {
		t.Fatalf("fetches=%d updates=%d writes=%d, want 1/0/0", fetches, updates, st.updateCalls)
	}
	if _, err := tr.ReconcileMilestones(context.Background(), []MilestoneLink{{ChildIdentifier: "TEST-13", MilestoneIDs: []string{"ms-a"}}}, nil, false); err == nil {
		t.Fatal("wet run without a marker writer must fail at setup")
	}
}

// Several milestone-epic parents: never pick. Linear holding one of them is
// a no-op; Linear empty is a per-link error with no mutation.
func TestMilestonePass_MultipleCandidates(t *testing.T) {
	msA := milestoneBead("bd-ms-a", "linear:project-milestone:ms-a", types.TypeEpic)
	msB := milestoneBead("bd-ms-b", "linear:project-milestone:ms-b", types.TypeEpic)
	c14 := milestoneBead("bd-c14", "https://linear.app/team/issue/TEST-14/c", types.TypeTask)
	c15 := milestoneBead("bd-c15", "https://linear.app/team/issue/TEST-15/c", types.TypeTask)
	st := &milestoneFakeStore{issues: []*types.Issue{msA, msB, c14, c15}}
	for _, c := range []string{"bd-c14", "bd-c15"} {
		st.parent(c, msA)
		st.parent(c, msB)
	}
	m := newMilestoneMock(t)
	m.issues["TEST-14"] = &Issue{ID: "uuid-14", Identifier: "TEST-14", ProjectMilestone: &ProjectMilestone{ID: "ms-b"}}
	m.issues["TEST-15"] = &Issue{ID: "uuid-15", Identifier: "TEST-15"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := runMilestonePass(t, st, tr, false)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if stats.Skipped != 1 || stats.Updated != 0 || len(stats.Errors) != 1 || !strings.Contains(stats.Errors[0].Error(), "TEST-15") {
		t.Fatalf("stats = %+v, want Skipped=1 and one error naming TEST-15", stats)
	}
	if _, updates := m.counts(); updates != 0 || st.updateCalls != 0 {
		t.Fatalf("linear updates=%d store writes=%d, want 0/0", updates, st.updateCalls)
	}
}

// No touch when the parent has no milestone ref: children under an ordinary
// epic, or parentless, get no link — zero fetches, zero updates.
func TestMilestonePass_NoTouchWhenParentHasNoRef(t *testing.T) {
	epic := milestoneBead("bd-epic", "https://linear.app/team/issue/TEST-20/epic", types.TypeEpic)
	underEpic := milestoneBead("bd-a", "https://linear.app/team/issue/TEST-21/a", types.TypeTask)
	orphan := milestoneBead("bd-b", "https://linear.app/team/issue/TEST-22/b", types.TypeTask)
	msEpic := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	st := &milestoneFakeStore{issues: []*types.Issue{epic, underEpic, orphan, msEpic}}
	st.parent("bd-a", epic)

	m := newMilestoneMock(t)
	m.issues["TEST-21"] = &Issue{ID: "uuid-21", Identifier: "TEST-21"}
	m.issues["TEST-22"] = &Issue{ID: "uuid-22", Identifier: "TEST-22", ProjectMilestone: &ProjectMilestone{ID: "ms-carl"}}
	tr := newMilestoneTestTracker(t, m)
	if _, err := runMilestonePass(t, st, tr, false); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if fetches, updates := m.counts(); fetches != 0 || updates != 0 || st.updateCalls != 0 {
		t.Fatalf("fetches=%d updates=%d writes=%d, want 0/0/0", fetches, updates, st.updateCalls)
	}
}

// Builder: only the DIRECT parent counts, empty milestone ids are dropped,
// duplicate edges collapse, non-Linear children are ignored, guards are read.
func TestBuildMilestoneLinks_DirectParentOnly(t *testing.T) {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	msEmpty := milestoneBead("bd-ms-empty", "linear:project-milestone:  ", types.TypeEpic)
	child := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-30/child", types.TypeEpic)
	child.Metadata = json.RawMessage(`{"linear":{"milestone_assigned":"ms-a"}}`)
	grandchild := milestoneBead("bd-gc", "https://linear.app/team/issue/TEST-31/gc", types.TypeTask)
	emptyChild := milestoneBead("bd-ec", "https://linear.app/team/issue/TEST-32/ec", types.TypeTask)
	local := milestoneBead("bd-local", "gh-123", types.TypeTask)
	st := &milestoneFakeStore{issues: []*types.Issue{ms, msEmpty, child, grandchild, emptyChild, local}}
	st.parent("bd-child", ms)
	st.parent("bd-child", ms) // duplicate edge
	st.parent("bd-gc", child)
	st.parent("bd-ec", msEmpty)
	st.parent("bd-local", ms)

	links, perBead, err := BuildMilestoneLinks(context.Background(), st)
	if err != nil || len(perBead) != 0 {
		t.Fatalf("BuildMilestoneLinks: err=%v perBead=%v", err, perBead)
	}
	if len(links) != 1 || links[0].ChildIdentifier != "TEST-30" || links[0].BeadID != "bd-child" ||
		len(links[0].MilestoneIDs) != 1 || links[0].MilestoneIDs[0] != "ms-a" || links[0].AssignedMarker != "ms-a" {
		t.Fatalf("links = %+v, want only TEST-30 → ms-a with marker", links)
	}
}

// Builder fails closed: store errors abort; malformed metadata on a child
// is a per-bead error (never read as "no marker") and yields no link.
func TestBuildMilestoneLinks_FailsClosed(t *testing.T) {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	child := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-40/child", types.TypeTask)
	for name, st := range map[string]*milestoneFakeStore{
		"search": {searchErr: errors.New("boom")},
		"deps":   {issues: []*types.Issue{ms, child}, depsErr: errors.New("boom")},
	} {
		links, _, err := BuildMilestoneLinks(context.Background(), st)
		if err == nil || links != nil {
			t.Fatalf("%s: links=%v err=%v, want error and no links", name, links, err)
		}
	}
	if _, _, err := BuildMilestoneLinks(context.Background(), nil); err == nil {
		t.Fatal("nil store: want error")
	}

	for _, bad := range []string{`[1,2]`, `{"linear":"x"}`, `{"linear":{"milestone_assigned":7}}`, `{"linear":{"project_milestone":"x"}}`} {
		c := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-40/child", types.TypeTask)
		c.Metadata = json.RawMessage(bad)
		st := &milestoneFakeStore{issues: []*types.Issue{ms, c}}
		st.parent("bd-child", ms)
		links, perBead, err := BuildMilestoneLinks(context.Background(), st)
		if err != nil || len(links) != 0 || len(perBead) != 1 {
			t.Fatalf("metadata %s: links=%v perBead=%v err=%v, want 1 per-bead error, no link", bad, links, perBead, err)
		}
	}
}

// End to end over the fake store and fake client: the direct child gets the
// milestone and the marker, the grandchild gets nothing, and a rerun is a
// complete no-op.
func TestMilestonePass_EndToEnd(t *testing.T) {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	child := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-50/child", types.TypeEpic)
	grandchild := milestoneBead("bd-gc", "https://linear.app/team/issue/TEST-51/gc", types.TypeTask)
	st := &milestoneFakeStore{issues: []*types.Issue{ms, child, grandchild}}
	st.parent("bd-child", ms)
	st.parent("bd-gc", child)

	m := newMilestoneMock(t)
	m.issues["TEST-50"] = &Issue{ID: "uuid-50", Identifier: "TEST-50"}
	m.issues["TEST-51"] = &Issue{ID: "uuid-51", Identifier: "TEST-51"}
	tr := newMilestoneTestTracker(t, m)

	for pass := 1; pass <= 2; pass++ {
		if _, err := runMilestonePass(t, st, tr, false); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if _, updates := m.counts(); updates != 1 || st.updateCalls != 1 {
		t.Fatalf("over two passes: linear updates=%d store writes=%d, want 1/1", updates, st.updateCalls)
	}
	if m.milestoneOf("TEST-50") != "ms-a" || st.marker(t, "bd-child") != "ms-a" {
		t.Fatalf("TEST-50 milestone=%q marker=%q, want ms-a/ms-a", m.milestoneOf("TEST-50"), st.marker(t, "bd-child"))
	}
	if m.milestoneOf("TEST-51") != "" || st.marker(t, "bd-gc") != "" {
		t.Fatal("grandchild must not be assigned a milestone")
	}
}
