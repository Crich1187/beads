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

// milestoneFakeStore is an in-memory MilestoneLinkStore.
type milestoneFakeStore struct {
	issues    []*types.Issue
	deps      map[string][]*types.IssueWithDependencyMetadata
	searchErr error
	depsErr   error
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

// Set when different: a child with no milestone, and a child sitting in a
// different milestone, both get exactly one issueUpdate carrying only
// projectMilestoneId = the parent epic's milestone UUID.
func TestReconcileMilestones_SetsWhenDifferent(t *testing.T) {
	m := newMilestoneMock(t)
	m.issues["TEST-1"] = &Issue{ID: "uuid-1", Identifier: "TEST-1"}
	m.issues["TEST-2"] = &Issue{ID: "uuid-2", Identifier: "TEST-2", ProjectMilestone: &ProjectMilestone{ID: "ms-old"}}
	tr := newMilestoneTestTracker(t, m)

	stats, err := tr.ReconcileMilestones(context.Background(), []MilestoneLink{
		{ChildIdentifier: "TEST-1", MilestoneIDs: []string{"ms-a"}},
		{ChildIdentifier: "TEST-2", MilestoneIDs: []string{"ms-a"}},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileMilestones: %v", err)
	}
	if stats.Updated != 2 || stats.Skipped != 0 || len(stats.Errors) != 0 || len(stats.Mutations) != 2 {
		t.Fatalf("stats = %+v, want Updated=2", stats)
	}
	if len(m.updates) != 2 {
		t.Fatalf("issueUpdate calls = %d, want 2", len(m.updates))
	}
	for _, u := range m.updates {
		if len(u.Input) != 1 || u.Input["projectMilestoneId"] != "ms-a" {
			t.Errorf("update %s input = %v, want only projectMilestoneId=ms-a", u.IssueID, u.Input)
		}
	}
}

// No-op when equal: remote milestone already matches, so no mutation.
func TestReconcileMilestones_NoOpWhenEqual(t *testing.T) {
	m := newMilestoneMock(t)
	m.issues["TEST-3"] = &Issue{ID: "uuid-3", Identifier: "TEST-3", ProjectMilestone: &ProjectMilestone{ID: "ms-a"}}
	tr := newMilestoneTestTracker(t, m)

	stats, err := tr.ReconcileMilestones(context.Background(), []MilestoneLink{
		{ChildIdentifier: "TEST-3", MilestoneIDs: []string{"ms-a"}},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileMilestones: %v", err)
	}
	if stats.Skipped != 1 || stats.Updated != 0 {
		t.Fatalf("stats = %+v, want Skipped=1 Updated=0", stats)
	}
	if _, updates := m.counts(); updates != 0 {
		t.Fatalf("issueUpdate calls = %d, want 0", updates)
	}
}

// Idempotent rerun: after one wet pass applies the milestones, a second pass
// over the same links issues zero mutations and skips every link.
func TestReconcileMilestones_IdempotentRerun(t *testing.T) {
	m := newMilestoneMock(t)
	m.issues["TEST-4"] = &Issue{ID: "uuid-4", Identifier: "TEST-4"}
	m.issues["TEST-5"] = &Issue{ID: "uuid-5", Identifier: "TEST-5", ProjectMilestone: &ProjectMilestone{ID: "ms-x"}}
	m.issues["TEST-6"] = &Issue{ID: "uuid-6", Identifier: "TEST-6", ProjectMilestone: &ProjectMilestone{ID: "ms-b"}}
	tr := newMilestoneTestTracker(t, m)
	links := []MilestoneLink{
		{ChildIdentifier: "TEST-4", MilestoneIDs: []string{"ms-a"}},
		{ChildIdentifier: "TEST-5", MilestoneIDs: []string{"ms-a"}},
		{ChildIdentifier: "TEST-6", MilestoneIDs: []string{"ms-b"}},
	}

	first, err := tr.ReconcileMilestones(context.Background(), links, false)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Updated != 2 || first.Skipped != 1 {
		t.Fatalf("first pass stats = %+v, want Updated=2 Skipped=1", first)
	}
	_, updatesAfterFirst := m.counts()

	second, err := tr.ReconcileMilestones(context.Background(), links, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	_, updatesAfterSecond := m.counts()
	if updatesAfterSecond != updatesAfterFirst {
		t.Fatalf("second pass issued %d mutations, want 0", updatesAfterSecond-updatesAfterFirst)
	}
	if second.Updated != 0 || second.Skipped != len(links) || len(second.Errors) != 0 {
		t.Fatalf("second pass stats = %+v, want Updated=0 Skipped=%d", second, len(links))
	}
}

// No clear when the parent has no milestone ref: a child under an ordinary
// epic, and a parentless child, both carrying a hand-set Linear milestone,
// produce no link, so the pass makes zero fetches and zero mutations.
func TestReconcileMilestones_NoClearWhenParentHasNoRef(t *testing.T) {
	epic := milestoneBead("bd-epic", "https://linear.app/team/issue/TEST-10/epic", types.TypeEpic)
	underEpic := milestoneBead("bd-a", "https://linear.app/team/issue/TEST-11/a", types.TypeTask)
	orphan := milestoneBead("bd-b", "https://linear.app/team/issue/TEST-12/b", types.TypeTask)
	msEpic := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic) // exists, but not their parent
	st := &milestoneFakeStore{issues: []*types.Issue{epic, underEpic, orphan, msEpic}}
	st.parent("bd-a", epic)

	links, err := BuildMilestoneLinks(context.Background(), st)
	if err != nil {
		t.Fatalf("BuildMilestoneLinks: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("links = %+v, want none", links)
	}

	m := newMilestoneMock(t)
	m.issues["TEST-11"] = &Issue{ID: "uuid-11", Identifier: "TEST-11", ProjectMilestone: &ProjectMilestone{ID: "ms-carl"}}
	m.issues["TEST-12"] = &Issue{ID: "uuid-12", Identifier: "TEST-12", ProjectMilestone: &ProjectMilestone{ID: "ms-carl"}}
	tr := newMilestoneTestTracker(t, m)
	if _, err := tr.ReconcileMilestones(context.Background(), links, false); err != nil {
		t.Fatalf("ReconcileMilestones: %v", err)
	}
	if fetches, updates := m.counts(); fetches != 0 || updates != 0 {
		t.Fatalf("fetches=%d updates=%d, want 0/0", fetches, updates)
	}
	if m.issues["TEST-11"].ProjectMilestone.ID != "ms-carl" || m.issues["TEST-12"].ProjectMilestone.ID != "ms-carl" {
		t.Fatal("hand-set milestone was changed")
	}
}

// Dry-run: fetches run, no mutation is sent, WouldUpdate reports the plan.
func TestReconcileMilestones_DryRun(t *testing.T) {
	m := newMilestoneMock(t)
	m.issues["TEST-7"] = &Issue{ID: "uuid-7", Identifier: "TEST-7"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := tr.ReconcileMilestones(context.Background(), []MilestoneLink{
		{ChildIdentifier: "TEST-7", MilestoneIDs: []string{"ms-a"}},
	}, true)
	if err != nil {
		t.Fatalf("ReconcileMilestones: %v", err)
	}
	if stats.WouldUpdate != 1 || stats.Updated != 0 || len(stats.Mutations) != 1 || stats.Mutations[0].MilestoneID != "ms-a" {
		t.Fatalf("stats = %+v, want WouldUpdate=1 Updated=0", stats)
	}
	if fetches, updates := m.counts(); fetches != 1 || updates != 0 {
		t.Fatalf("fetches=%d updates=%d, want 1/0", fetches, updates)
	}
}

// Several milestone-epic parents: never pick one. Leave Linear alone when
// its current milestone is a candidate; report an error (no mutation) when
// it is not.
func TestReconcileMilestones_MultipleCandidates(t *testing.T) {
	m := newMilestoneMock(t)
	m.issues["TEST-8"] = &Issue{ID: "uuid-8", Identifier: "TEST-8", ProjectMilestone: &ProjectMilestone{ID: "ms-b"}}
	m.issues["TEST-9"] = &Issue{ID: "uuid-9", Identifier: "TEST-9"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := tr.ReconcileMilestones(context.Background(), []MilestoneLink{
		{ChildIdentifier: "TEST-8", MilestoneIDs: []string{"ms-a", "ms-b"}},
		{ChildIdentifier: "TEST-9", MilestoneIDs: []string{"ms-a", "ms-b"}},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileMilestones: %v", err)
	}
	if stats.Skipped != 1 || stats.Updated != 0 || len(stats.Errors) != 1 {
		t.Fatalf("stats = %+v, want Skipped=1 Updated=0 Errors=1", stats)
	}
	if !strings.Contains(stats.Errors[0].Error(), "TEST-9") {
		t.Errorf("error should name TEST-9: %v", stats.Errors[0])
	}
	if _, updates := m.counts(); updates != 0 {
		t.Fatalf("issueUpdate calls = %d, want 0", updates)
	}
}

// Fail-closed: a rejected update (e.g. milestone in another project)
// surfaces as a per-link error and is not counted as applied; unknown
// children are reported as NotFound.
func TestReconcileMilestones_ErrorsSurface(t *testing.T) {
	m := newMilestoneMock(t)
	m.failUpdate = true
	m.issues["TEST-13"] = &Issue{ID: "uuid-13", Identifier: "TEST-13"}
	tr := newMilestoneTestTracker(t, m)

	stats, err := tr.ReconcileMilestones(context.Background(), []MilestoneLink{
		{ChildIdentifier: "TEST-13", MilestoneIDs: []string{"ms-a"}},
		{ChildIdentifier: "TEST-99", MilestoneIDs: []string{"ms-a"}},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileMilestones: %v", err)
	}
	if stats.Updated != 0 || len(stats.Mutations) != 0 || len(stats.Errors) != 1 {
		t.Fatalf("stats = %+v, want Updated=0 Errors=1", stats)
	}
	if len(stats.NotFound) != 1 || stats.NotFound[0] != "TEST-99" {
		t.Fatalf("NotFound = %v, want [TEST-99]", stats.NotFound)
	}
}

// Builder: only the DIRECT parent counts, empty milestone ids are dropped,
// duplicate edges collapse, and non-Linear children are ignored.
func TestBuildMilestoneLinks_DirectParentOnly(t *testing.T) {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	msEmpty := milestoneBead("bd-ms-empty", "linear:project-milestone:  ", types.TypeEpic)
	child := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-20/child", types.TypeEpic)
	grandchild := milestoneBead("bd-gc", "https://linear.app/team/issue/TEST-21/gc", types.TypeTask)
	emptyChild := milestoneBead("bd-ec", "https://linear.app/team/issue/TEST-22/ec", types.TypeTask)
	local := milestoneBead("bd-local", "gh-123", types.TypeTask)
	st := &milestoneFakeStore{issues: []*types.Issue{ms, msEmpty, child, grandchild, emptyChild, local}}
	st.parent("bd-child", ms)
	st.parent("bd-child", ms) // duplicate edge
	st.parent("bd-gc", child)
	st.parent("bd-ec", msEmpty)
	st.parent("bd-local", ms)

	links, err := BuildMilestoneLinks(context.Background(), st)
	if err != nil {
		t.Fatalf("BuildMilestoneLinks: %v", err)
	}
	if len(links) != 1 || links[0].ChildIdentifier != "TEST-20" ||
		len(links[0].MilestoneIDs) != 1 || links[0].MilestoneIDs[0] != "ms-a" {
		t.Fatalf("links = %+v, want only TEST-20 → ms-a", links)
	}
}

// Builder fails closed: a store error aborts before any link is returned.
func TestBuildMilestoneLinks_StoreErrorAborts(t *testing.T) {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	child := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-30/child", types.TypeTask)
	for name, st := range map[string]*milestoneFakeStore{
		"search": {searchErr: errors.New("boom")},
		"deps":   {issues: []*types.Issue{ms, child}, depsErr: errors.New("boom")},
	} {
		links, err := BuildMilestoneLinks(context.Background(), st)
		if err == nil || links != nil {
			t.Fatalf("%s: links=%v err=%v, want error and no links", name, links, err)
		}
	}
	if _, err := BuildMilestoneLinks(context.Background(), nil); err == nil {
		t.Fatal("nil store: want error")
	}
}

// End to end over the fake store and fake client: build → reconcile sets the
// milestone on the direct child only; a rerun is a no-op.
func TestMilestonePass_EndToEnd(t *testing.T) {
	ms := milestoneBead("bd-ms", "linear:project-milestone:ms-a", types.TypeEpic)
	child := milestoneBead("bd-child", "https://linear.app/team/issue/TEST-40/child", types.TypeEpic)
	grandchild := milestoneBead("bd-gc", "https://linear.app/team/issue/TEST-41/gc", types.TypeTask)
	st := &milestoneFakeStore{issues: []*types.Issue{ms, child, grandchild}}
	st.parent("bd-child", ms)
	st.parent("bd-gc", child)

	m := newMilestoneMock(t)
	m.issues["TEST-40"] = &Issue{ID: "uuid-40", Identifier: "TEST-40"}
	m.issues["TEST-41"] = &Issue{ID: "uuid-41", Identifier: "TEST-41"}
	tr := newMilestoneTestTracker(t, m)

	for pass := 1; pass <= 2; pass++ {
		links, err := BuildMilestoneLinks(context.Background(), st)
		if err != nil {
			t.Fatalf("pass %d build: %v", pass, err)
		}
		if _, err := tr.ReconcileMilestones(context.Background(), links, false); err != nil {
			t.Fatalf("pass %d reconcile: %v", pass, err)
		}
	}
	if _, updates := m.counts(); updates != 1 {
		t.Fatalf("issueUpdate calls over two passes = %d, want 1", updates)
	}
	if got := m.issues["TEST-40"].ProjectMilestone; got == nil || got.ID != "ms-a" {
		t.Fatalf("TEST-40 milestone = %+v, want ms-a", got)
	}
	if m.issues["TEST-41"].ProjectMilestone != nil {
		t.Fatal("grandchild must not be assigned a milestone")
	}
}
