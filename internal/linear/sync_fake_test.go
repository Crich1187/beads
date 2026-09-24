package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// fakeLinear is a small stateful Linear GraphQL server for sync tests. It
// answers the queries and the issueUpdate mutation the push and pull paths
// send, returns issue URLs in Linear's slug form, hides archived issues unless
// the query passes includeArchived: true, and records every mutation.
type fakeLinear struct {
	t      *testing.T
	mu     sync.Mutex
	issues map[string]*fakeLinearIssue // by identifier
	labels []Label                     // team labels

	updates             []map[string]interface{} // issueUpdate inputs, in order
	updatedIdentifiers  []string
	includeArchivedSeen int // IssueByIdentifier queries that passed includeArchived: true
	archivedFieldSeen   int // IssueByIdentifier queries that selected archivedAt
	server              *httptest.Server
}

type fakeLinearIssue struct {
	id, identifier, title, description string
	priority                           int
	labelIDs                           []string
	archivedAt                         string
	updatedAt                          time.Time
}

func newFakeLinear(t *testing.T, labelNames ...string) *fakeLinear {
	t.Helper()
	f := &fakeLinear{t: t, issues: map[string]*fakeLinearIssue{}}
	for i, name := range labelNames {
		f.labels = append(f.labels, Label{ID: fmt.Sprintf("label-%d", i+1), Name: name})
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLinear) addIssue(identifier, title, description string, labelNames ...string) *fakeLinearIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue := &fakeLinearIssue{
		id: "uuid-" + identifier, identifier: identifier, title: title, description: description,
		priority: 3, labelIDs: f.labelIDsLocked(labelNames), updatedAt: time.Now().UTC(),
	}
	f.issues[identifier] = issue
	return issue
}

// setLabels is a Linear-side (Carl) label edit.
func (f *fakeLinear) setLabels(identifier string, labelNames ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue := f.issues[identifier]
	issue.labelIDs = f.labelIDsLocked(labelNames)
	issue.updatedAt = time.Now().UTC()
}

func (f *fakeLinear) archive(identifier string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[identifier].archivedAt = "2026-09-24T18:13:30.000Z"
}

func (f *fakeLinear) labelNames(identifier string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := f.labelNamesLocked(f.issues[identifier].labelIDs)
	sort.Strings(names)
	return names
}

func (f *fakeLinear) labelIDsLocked(names []string) []string {
	var ids []string
	for _, name := range names {
		for _, label := range f.labels {
			if label.Name == name {
				ids = append(ids, label.ID)
			}
		}
	}
	return ids
}

func (f *fakeLinear) labelNamesLocked(ids []string) []string {
	var names []string
	for _, id := range ids {
		for _, label := range f.labels {
			if label.ID == id {
				names = append(names, label.Name)
			}
		}
	}
	return names
}

func (f *fakeLinear) nodeLocked(issue *fakeLinearIssue, withArchived bool) map[string]interface{} {
	labelNodes := []interface{}{}
	for _, id := range issue.labelIDs {
		for _, label := range f.labels {
			if label.ID == id {
				labelNodes = append(labelNodes, map[string]interface{}{"id": label.ID, "name": label.Name})
			}
		}
	}
	node := map[string]interface{}{
		"id":          issue.id,
		"identifier":  issue.identifier,
		"title":       issue.title,
		"description": issue.description,
		// Linear returns the slug form; bd must store the canonical one.
		"url":       "https://linear.app/ws/issue/" + issue.identifier + "/" + strings.ToLower(strings.ReplaceAll(issue.title, " ", "-")),
		"priority":  issue.priority,
		"state":     map[string]interface{}{"id": "state-started", "name": "In Progress", "type": "started"},
		"labels":    map[string]interface{}{"nodes": labelNodes},
		"createdAt": "2026-09-24T16:00:00Z",
		"updatedAt": issue.updatedAt.Format(time.RFC3339Nano),
	}
	if withArchived && issue.archivedAt != "" {
		node["archivedAt"] = issue.archivedAt
	}
	return node
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req GraphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("fake linear: bad request body: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(data map[string]interface{}) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	}
	switch {
	case strings.Contains(req.Query, "TeamStates"):
		reply(map[string]interface{}{"team": map[string]interface{}{"id": "team-1", "states": map[string]interface{}{"nodes": []interface{}{
			map[string]interface{}{"id": "state-started", "name": "In Progress", "type": "started"},
		}}}})
	case strings.Contains(req.Query, "TeamLabels"):
		nodes := []interface{}{}
		for _, label := range f.labels {
			nodes = append(nodes, map[string]interface{}{"id": label.ID, "name": label.Name})
		}
		reply(map[string]interface{}{"team": map[string]interface{}{"id": "team-1", "labels": map[string]interface{}{
			"nodes": nodes, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
		}}})
	case strings.Contains(req.Query, "IssueByIdentifier"):
		includeArchived := strings.Contains(req.Query, "includeArchived: true")
		if includeArchived {
			f.includeArchivedSeen++
		}
		if strings.Contains(req.Query, "archivedAt") {
			f.archivedFieldSeen++
		}
		nodes := []interface{}{}
		filter, _ := req.Variables["filter"].(map[string]interface{})
		number, _ := filter["number"].(map[string]interface{})
		for _, issue := range f.issues {
			if fmt.Sprintf("%v", number["eq"]) != strings.TrimPrefix(issue.identifier, "TEST-") {
				continue
			}
			if issue.archivedAt != "" && !includeArchived {
				continue
			}
			nodes = append(nodes, f.nodeLocked(issue, true))
		}
		reply(map[string]interface{}{"issues": map[string]interface{}{"nodes": nodes}})
	case strings.Contains(req.Query, "query Issues("):
		// The default issues query hides archived issues.
		var ids []string
		for id, issue := range f.issues {
			if issue.archivedAt == "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		nodes := []interface{}{}
		for _, id := range ids {
			nodes = append(nodes, f.nodeLocked(f.issues[id], false))
		}
		reply(map[string]interface{}{"issues": map[string]interface{}{
			"nodes": nodes, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
		}})
	case strings.Contains(req.Query, "issueUpdate"):
		id, _ := req.Variables["id"].(string)
		input, _ := req.Variables["input"].(map[string]interface{})
		var target *fakeLinearIssue
		for _, issue := range f.issues {
			if issue.id == id || issue.identifier == id {
				target = issue
			}
		}
		if target == nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "Entity not found"}}})
			return
		}
		f.updates = append(f.updates, input)
		f.updatedIdentifiers = append(f.updatedIdentifiers, target.identifier)
		if v, ok := input["title"].(string); ok {
			target.title = v
		}
		if v, ok := input["description"].(string); ok {
			target.description = v
		}
		if raw, ok := input["labelIds"].([]interface{}); ok {
			target.labelIDs = nil
			for _, v := range raw {
				target.labelIDs = append(target.labelIDs, fmt.Sprint(v))
			}
		}
		target.updatedAt = time.Now().UTC()
		reply(map[string]interface{}{"issueUpdate": map[string]interface{}{"success": true, "issue": f.nodeLocked(target, false)}})
	default:
		f.t.Errorf("fake linear: unexpected query: %s", req.Query)
		reply(map[string]interface{}{})
	}
}

func (f *fakeLinear) tracker() *Tracker {
	cfg := DefaultMappingConfig()
	cfg.ExplicitStateMap = map[string]string{"in progress": "in_progress"}
	return &Tracker{
		teamIDs: []string{"team-1"},
		clients: map[string]*Client{"team-1": NewClient("key", "team-1").WithEndpoint(f.server.URL)},
		config:  cfg,
	}
}

func (f *fakeLinear) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

// memTrackerStore is an in-memory tracker.Store (plus IssueUpdater) that
// records external_ref writes, for driving tracker.Engine against fakeLinear.
type memTrackerStore struct {
	mu            sync.Mutex
	issues        map[string]*types.Issue
	order         []string
	config        map[string]string
	localMetadata map[string]string
	refWrites     []string
	nextID        int
}

var _ tracker.Store = (*memTrackerStore)(nil)
var _ tracker.IssueUpdater = (*memTrackerStore)(nil)

func newMemTrackerStore() *memTrackerStore {
	return &memTrackerStore{issues: map[string]*types.Issue{}, config: map[string]string{}, localMetadata: map[string]string{}}
}

func cloneTestIssue(issue *types.Issue) *types.Issue {
	c := *issue
	c.Labels = append([]string(nil), issue.Labels...)
	if issue.ExternalRef != nil {
		ref := *issue.ExternalRef
		c.ExternalRef = &ref
	}
	return &c
}

func (s *memTrackerStore) issue(id string) *types.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTestIssue(s.issues[id])
}

// only returns the single stored issue (tests that pull exactly one).
func (s *memTrackerStore) only(t *testing.T) *types.Issue {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) != 1 {
		t.Fatalf("store holds %d issues, want 1", len(s.order))
	}
	return cloneTestIssue(s.issues[s.order[0]])
}

// localEdit models an agent edit through bd: fields bump updated_at, labels
// do not (bd's label writes never touch issues.updated_at).
func (s *memTrackerStore) localEdit(id string, fn func(*types.Issue), bumpUpdatedAt bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.issues[id])
	if bumpUpdatedAt {
		s.issues[id].UpdatedAt = time.Now().UTC().Add(2 * time.Second)
	}
}

func (s *memTrackerStore) GetConfig(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config[key], nil
}

func (s *memTrackerStore) GetAllConfig(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.config {
		out[k] = v
	}
	return out, nil
}

func (s *memTrackerStore) GetLocalMetadata(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localMetadata[key], nil
}

func (s *memTrackerStore) SetLocalMetadata(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localMetadata[key] = value
	return nil
}

func (s *memTrackerStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*types.Issue, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, cloneTestIssue(s.issues[id]))
	}
	return out, nil
}

func (s *memTrackerStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		if issue := s.issues[id]; issue.ExternalRef != nil && *issue.ExternalRef == ref {
			return cloneTestIssue(issue), nil
		}
	}
	return nil, nil
}

func (s *memTrackerStore) GetDependentsWithMetadata(context.Context, string) ([]*types.IssueWithDependencyMetadata, error) {
	return nil, nil
}

func (s *memTrackerStore) GetDependenciesWithMetadata(context.Context, string) ([]*types.IssueWithDependencyMetadata, error) {
	return nil, nil
}

func (s *memTrackerStore) AddDependency(context.Context, *types.Dependency, string) error { return nil }

func (s *memTrackerStore) CreateIssue(_ context.Context, issue *types.Issue, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue.ID == "" {
		s.nextID++
		issue.ID = fmt.Sprintf("bd-%d", s.nextID)
	}
	now := time.Now().UTC()
	stored := cloneTestIssue(issue)
	stored.CreatedAt, stored.UpdatedAt = now, now
	s.issues[stored.ID] = stored
	s.order = append(s.order, stored.ID)
	return nil
}

func (s *memTrackerStore) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return s.ApplyIssueUpdate(ctx, id, updates, nil, actor)
}

func (s *memTrackerStore) ApplyIssueUpdate(_ context.Context, id string, updates map[string]interface{}, labels []string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue := s.issues[id]
	if issue == nil {
		return fmt.Errorf("issue %s not found", id)
	}
	for key, value := range updates {
		switch key {
		case "title":
			issue.Title = value.(string)
		case "description":
			issue.Description = value.(string)
		case "assignee":
			issue.Assignee = value.(string)
		case "external_ref":
			ref := value.(string)
			issue.ExternalRef = &ref
			s.refWrites = append(s.refWrites, id+"="+ref)
		}
	}
	if labels != nil {
		issue.Labels = append([]string(nil), labels...)
	}
	issue.UpdatedAt = time.Now().UTC()
	return nil
}

func sortedLabels(labels []string) []string {
	out := append([]string(nil), labels...)
	sort.Strings(out)
	return out
}
