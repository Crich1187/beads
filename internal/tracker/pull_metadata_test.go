package tracker

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// siblingMetadata mirrors the real shapes other sync passes keep in a bead's
// metadata column: linear.relations (.27 ownership ledger, nested),
// linear.milestone_assigned (.29 marker, nested), the top-level dotted key
// "linear.comment_watermark" (.28 watermark), and a non-tracker key.
const siblingMetadata = `{
	"linear": {
		"project_milestone": {"id": "m1", "name": "A", "targetDate": "2026-05-01"},
		"relations": [{"id": "rel-1", "type": "blocks"}],
		"milestone_assigned": "m1"
	},
	"linear.comment_watermark": {"last_id": "c9"},
	"owner": "carl"
}`

func decodeForTest(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	var v map[string]interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func linearNS(t *testing.T, m map[string]interface{}) map[string]interface{} {
	t.Helper()
	ns, ok := m["linear"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata.linear missing or not an object: %#v", m)
	}
	return ns
}

func assertSiblingsSurvive(t *testing.T, m map[string]interface{}) {
	t.Helper()
	ns := linearNS(t, m)
	wantRelations := []interface{}{map[string]interface{}{"id": "rel-1", "type": "blocks"}}
	if !reflect.DeepEqual(ns["relations"], wantRelations) {
		t.Errorf("linear.relations = %#v, want %#v", ns["relations"], wantRelations)
	}
	if ns["milestone_assigned"] != "m1" {
		t.Errorf("linear.milestone_assigned = %#v, want m1", ns["milestone_assigned"])
	}
	wantWM := map[string]interface{}{"last_id": "c9"}
	if !reflect.DeepEqual(m["linear.comment_watermark"], wantWM) {
		t.Errorf("linear.comment_watermark = %#v, want %#v", m["linear.comment_watermark"], wantWM)
	}
	if m["owner"] != "carl" {
		t.Errorf("owner = %#v, want carl", m["owner"])
	}
}

func milestoneMeta(ms interface{}) map[string]interface{} {
	return map[string]interface{}{"linear": map[string]interface{}{"project_milestone": ms}}
}

// typedNilMilestone models the Linear tracker's value: a typed nil pointer
// stored in an interface{} must still be read as JSON null (delete).
type typedNilMilestone struct{ ID string }

func TestMergePulledMetadataReplacesMilestoneAndKeepsSiblings(t *testing.T) {
	incoming := milestoneMeta(map[string]interface{}{"id": "m2", "name": "B"})
	merged, changed, err := mergePulledMetadata(json.RawMessage(siblingMetadata), incoming)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for a new milestone")
	}
	m := decodeForTest(t, merged)
	assertSiblingsSurvive(t, m)
	// Wholesale replace of the leaf: the stale targetDate from milestone A
	// must not leak into milestone B (a fully recursive merge would keep it).
	want := map[string]interface{}{"id": "m2", "name": "B"}
	if got := linearNS(t, m)["project_milestone"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("linear.project_milestone = %#v, want %#v", got, want)
	}
}

func TestMergePulledMetadataMilestoneRemovalClearsOnlyThatKey(t *testing.T) {
	for name, incoming := range map[string]map[string]interface{}{
		"untyped nil": milestoneMeta(nil),
		"typed nil":   milestoneMeta((*typedNilMilestone)(nil)),
	} {
		t.Run(name, func(t *testing.T) {
			merged, changed, err := mergePulledMetadata(json.RawMessage(siblingMetadata), incoming)
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			if !changed {
				t.Fatal("changed = false, want true when a milestone is removed")
			}
			m := decodeForTest(t, merged)
			assertSiblingsSurvive(t, m)
			if _, ok := linearNS(t, m)["project_milestone"]; ok {
				t.Fatalf("linear.project_milestone still present after removal: %s", merged)
			}
		})
	}
}

func TestMergePulledMetadataRemovalOfOnlyKeyDropsEmptyNamespace(t *testing.T) {
	merged, changed, err := mergePulledMetadata(
		json.RawMessage(`{"linear":{"project_milestone":{"id":"m1"}},"owner":"carl"}`), milestoneMeta(nil))
	if err != nil || !changed {
		t.Fatalf("merge: changed=%v err=%v", changed, err)
	}
	if got, want := string(merged), `{"owner":"carl"}`; got != want {
		t.Fatalf("merged = %s, want %s", got, want)
	}
}

func TestMergePulledMetadataNoMilestoneIsNoOpWithoutLinearMetadata(t *testing.T) {
	for _, existing := range []string{"", "  ", "null", "{}", `{"owner":"carl"}`, `{"linear":{"relations":[]}}`} {
		merged, changed, err := mergePulledMetadata(json.RawMessage(existing), milestoneMeta(nil))
		if err != nil {
			t.Fatalf("existing %q: %v", existing, err)
		}
		if changed {
			t.Fatalf("existing %q: changed = true, merged %s; want no-op (no empty linear namespace written)", existing, merged)
		}
		if strings.Contains(string(merged), "project_milestone") || strings.Contains(string(merged), `"linear":{}`) {
			t.Fatalf("existing %q: merged = %s, want no project_milestone and no empty namespace", existing, merged)
		}
	}
}

func TestMergePulledMetadataNilIncomingMeansNoChange(t *testing.T) {
	existing := json.RawMessage(siblingMetadata)
	merged, changed, err := mergePulledMetadata(existing, nil)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want no change", changed, err)
	}
	if string(merged) != string(existing) {
		t.Fatalf("merged = %s, want existing returned untouched", merged)
	}
	// Non-object existing must not error when there is nothing to merge.
	if _, changed, err := mergePulledMetadata(json.RawMessage(`"legacy"`), nil); err != nil || changed {
		t.Fatalf("nil incoming over non-object existing: changed=%v err=%v", changed, err)
	}
}

func TestMergePulledMetadataEmptyExistingTreatedAsObject(t *testing.T) {
	incoming := milestoneMeta(map[string]interface{}{"id": "m1"})
	for _, existing := range []string{"", " \n ", "null"} {
		merged, changed, err := mergePulledMetadata(json.RawMessage(existing), incoming)
		if err != nil || !changed {
			t.Fatalf("existing %q: changed=%v err=%v", existing, changed, err)
		}
		if got, want := string(merged), `{"linear":{"project_milestone":{"id":"m1"}}}`; got != want {
			t.Fatalf("existing %q: merged = %s, want %s", existing, got, want)
		}
	}
}

func TestMergePulledMetadataRejectsNonObjectExisting(t *testing.T) {
	incoming := milestoneMeta(map[string]interface{}{"id": "m1"})
	for _, existing := range []string{`"legacy string"`, `[1,2]`, `42`, `true`, `{not json`} {
		merged, changed, err := mergePulledMetadata(json.RawMessage(existing), incoming)
		if err == nil {
			t.Fatalf("existing %q: err = nil, want an error (merging would destroy it)", existing)
		}
		if changed {
			t.Fatalf("existing %q: changed = true on error", existing)
		}
		if string(merged) != existing {
			t.Fatalf("existing %q: merged = %s, want existing returned unchanged", existing, merged)
		}
	}
}

func TestMergePulledMetadataSemanticEqualityIsUnchanged(t *testing.T) {
	// Same milestone, different key order and whitespace, plus unrelated
	// local siblings: nothing to write.
	existing := json.RawMessage(`{ "owner":"carl", "linear": { "relations": [], "project_milestone": { "name":"A", "progress": 60.5, "id":"m1" } } }`)
	incoming := milestoneMeta(map[string]interface{}{"id": "m1", "name": "A", "progress": 60.5})
	_, changed, err := mergePulledMetadata(existing, incoming)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if changed {
		t.Fatal("changed = true for a semantically identical milestone; pull would rewrite every sync")
	}
}

func TestMergePulledMetadataFlatTopLevelKeys(t *testing.T) {
	// Flat trackers (ADO/Jira style) replace only their own keys; top-level
	// null deletes; an object replacing a scalar is taken as-is.
	existing := json.RawMessage(`{"ado.rev":3,"ado.area_path":"A","owner":"carl","linear":"legacy"}`)
	incoming := map[string]interface{}{"ado.rev": 4, "ado.area_path": nil, "linear": map[string]interface{}{"project_milestone": map[string]interface{}{"id": "m1"}}}
	merged, changed, err := mergePulledMetadata(existing, incoming)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := `{"ado.rev":4,"linear":{"project_milestone":{"id":"m1"}},"owner":"carl"}`
	if string(merged) != want {
		t.Fatalf("merged = %s, want %s", merged, want)
	}
}

// ---- engine-level tests: both pull write paths -------------------------

type appliedPullUpdate struct {
	id      string
	updates map[string]interface{}
}

// metaPullStore is a pure-Go tracker Store + IssueUpdater that records every
// update map and applies metadata/title so tests can read the resulting row.
type metaPullStore struct {
	issues        map[string]*types.Issue
	applied       []appliedPullUpdate
	created       []*types.Issue
	searchFilters []types.IssueFilter
}

var _ Store = (*metaPullStore)(nil)
var _ IssueUpdater = (*metaPullStore)(nil)

func newMetaPullStore(issues ...*types.Issue) *metaPullStore {
	s := &metaPullStore{issues: map[string]*types.Issue{}}
	for _, issue := range issues {
		s.issues[issue.ID] = issue
	}
	return s
}

func (s *metaPullStore) GetConfig(context.Context, string) (string, error) { return "", nil }
func (s *metaPullStore) GetAllConfig(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (s *metaPullStore) GetLocalMetadata(context.Context, string) (string, error) { return "", nil }
func (s *metaPullStore) SetLocalMetadata(context.Context, string, string) error   { return nil }
func (s *metaPullStore) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.searchFilters = append(s.searchFilters, filter)
	var out []*types.Issue
	for _, issue := range s.issues {
		if len(filter.IDs) > 0 {
			match := false
			for _, id := range filter.IDs {
				match = match || id == issue.ID
			}
			if !match {
				continue
			}
		}
		out = append(out, issue)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *metaPullStore) GetIssueByExternalRef(_ context.Context, ref string) (*types.Issue, error) {
	for _, issue := range s.issues {
		if issue.ExternalRef != nil && *issue.ExternalRef == ref {
			return issue, nil
		}
	}
	return nil, nil
}
func (s *metaPullStore) GetDependentsWithMetadata(context.Context, string) ([]*types.IssueWithDependencyMetadata, error) {
	return nil, nil
}
func (s *metaPullStore) GetDependenciesWithMetadata(context.Context, string) ([]*types.IssueWithDependencyMetadata, error) {
	return nil, nil
}
func (s *metaPullStore) CreateIssue(_ context.Context, issue *types.Issue, _ string) error {
	s.created = append(s.created, issue)
	return nil
}
func (s *metaPullStore) UpdateIssue(context.Context, string, map[string]interface{}, string) error {
	return nil
}
func (s *metaPullStore) AddDependency(context.Context, *types.Dependency, string) error { return nil }
func (s *metaPullStore) ApplyIssueUpdate(_ context.Context, id string, updates map[string]interface{}, _ []string, _ string) error {
	copied := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		copied[k] = v
	}
	s.applied = append(s.applied, appliedPullUpdate{id: id, updates: copied})
	issue := s.issues[id]
	if issue == nil {
		return nil
	}
	if title, ok := updates["title"].(string); ok {
		issue.Title = title
	}
	switch v := updates["metadata"].(type) {
	case json.RawMessage:
		issue.Metadata = v
	case string:
		issue.Metadata = json.RawMessage(v)
	}
	return nil
}

const metaTestRef = "https://test.test/EXT-1"

func metaLocalIssue(metadata string) *types.Issue {
	ref := metaTestRef
	return &types.Issue{
		ID: "bd-meta", Title: "same", Status: types.StatusOpen, Priority: 2,
		IssueType: types.TypeTask, ExternalRef: &ref, Metadata: json.RawMessage(metadata),
	}
}

// newMetaPullEngine wires a tracker returning one remote issue whose scalar
// fields equal metaLocalIssue unless title is changed.
func newMetaPullEngine(store *metaPullStore, title string, metadata map[string]interface{}) (*Engine, *[]string) {
	mock := newMockTracker("test")
	mock.issues = []TrackerIssue{{ID: "EXT-1", Identifier: "EXT-1", Title: title, Metadata: metadata}}
	mock.fieldMapper = &mockMapper{issueToBeads: func(ti *TrackerIssue) *IssueConversion {
		return &IssueConversion{Issue: &types.Issue{
			Title: ti.Title, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		}}
	}}
	engine := NewEngine(mock, store, "sync")
	var warnings []string
	engine.OnWarning = func(msg string) { warnings = append(warnings, msg) }
	return engine, &warnings
}

func singleApplied(t *testing.T, store *metaPullStore) map[string]interface{} {
	t.Helper()
	if len(store.applied) != 1 {
		t.Fatalf("ApplyIssueUpdate calls = %d, want 1", len(store.applied))
	}
	return store.applied[0].updates
}

func TestDoPullMergesMetadataPreservingSiblings(t *testing.T) {
	local := metaLocalIssue(siblingMetadata)
	store := newMetaPullStore(local)
	engine, _ := newMetaPullEngine(store, "same", milestoneMeta(map[string]interface{}{"id": "m2", "name": "B"}))

	stats, err := engine.doPull(context.Background(), SyncOptions{Pull: true}, nil, nil)
	if err != nil {
		t.Fatalf("doPull: %v", err)
	}
	// Milestone-only change (scalars equal) must still be applied, not skipped.
	if stats.Updated != 1 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want Updated=1 Skipped=0 for a milestone-only change", stats)
	}
	if _, ok := singleApplied(t, store)["metadata"]; !ok {
		t.Fatal("update has no metadata key")
	}
	m := decodeForTest(t, local.Metadata)
	assertSiblingsSurvive(t, m)
	if got := linearNS(t, m)["project_milestone"]; !reflect.DeepEqual(got, map[string]interface{}{"id": "m2", "name": "B"}) {
		t.Fatalf("linear.project_milestone = %#v, want m2", got)
	}
}

func TestDoPullMilestoneRemovalClearsOnlyMilestone(t *testing.T) {
	local := metaLocalIssue(siblingMetadata)
	store := newMetaPullStore(local)
	engine, _ := newMetaPullEngine(store, "same", milestoneMeta(nil))

	stats, err := engine.doPull(context.Background(), SyncOptions{Pull: true}, nil, nil)
	if err != nil {
		t.Fatalf("doPull: %v", err)
	}
	if stats.Updated != 1 {
		t.Fatalf("stats = %+v, want Updated=1 for a removed milestone", stats)
	}
	m := decodeForTest(t, local.Metadata)
	assertSiblingsSurvive(t, m)
	if _, ok := linearNS(t, m)["project_milestone"]; ok {
		t.Fatalf("linear.project_milestone survived removal: %s", local.Metadata)
	}
}

func TestDoPullUnrelatedLocalMetadataDoesNotForceUpdate(t *testing.T) {
	local := metaLocalIssue(siblingMetadata)
	store := newMetaPullStore(local)
	// Same milestone as stored; only unrelated local sibling keys differ from
	// what the tracker reports.
	engine, _ := newMetaPullEngine(store, "same",
		milestoneMeta(map[string]interface{}{"id": "m1", "name": "A", "targetDate": "2026-05-01"}))

	stats, err := engine.doPull(context.Background(), SyncOptions{Pull: true}, nil, nil)
	if err != nil {
		t.Fatalf("doPull: %v", err)
	}
	if stats.Skipped != 1 || stats.Updated != 0 || len(store.applied) != 0 {
		t.Fatalf("stats = %+v applied=%d, want Skipped=1 and no write", stats, len(store.applied))
	}
}

func TestDoPullNonObjectMetadataWarnsAndKeepsIt(t *testing.T) {
	local := metaLocalIssue(`"legacy string"`)
	store := newMetaPullStore(local)
	engine, warnings := newMetaPullEngine(store, "remote title", milestoneMeta(map[string]interface{}{"id": "m1"}))

	stats, err := engine.doPull(context.Background(), SyncOptions{Pull: true}, nil, nil)
	if err != nil {
		t.Fatalf("doPull: %v", err)
	}
	if stats.Updated != 1 {
		t.Fatalf("stats = %+v, want Updated=1 (scalar fields still apply)", stats)
	}
	updates := singleApplied(t, store)
	if _, ok := updates["metadata"]; ok {
		t.Fatalf("update overwrote non-object metadata: %#v", updates["metadata"])
	}
	if local.Title != "remote title" {
		t.Fatalf("title = %q, want remote title applied", local.Title)
	}
	if string(local.Metadata) != `"legacy string"` {
		t.Fatalf("metadata = %s, want untouched", local.Metadata)
	}
	if len(*warnings) == 0 || !strings.Contains(strings.Join(*warnings, "\n"), "bd-meta") {
		t.Fatalf("warnings = %q, want a warning naming the issue", *warnings)
	}
}

func TestDoPullNilTrackerMetadataWritesNoMetadata(t *testing.T) {
	local := metaLocalIssue(siblingMetadata)
	store := newMetaPullStore(local)
	engine, _ := newMetaPullEngine(store, "remote title", nil)

	if _, err := engine.doPull(context.Background(), SyncOptions{Pull: true}, nil, nil); err != nil {
		t.Fatalf("doPull: %v", err)
	}
	if _, ok := singleApplied(t, store)["metadata"]; ok {
		t.Fatal("tracker with nil metadata produced a metadata update")
	}
	if string(local.Metadata) != siblingMetadata {
		t.Fatalf("metadata changed: %s", local.Metadata)
	}
}

func TestDoPullCreateStripsNullMilestone(t *testing.T) {
	for name, tc := range map[string]struct {
		metadata map[string]interface{}
		want     string
	}{
		"no milestone":   {milestoneMeta(nil), ""},
		"with milestone": {milestoneMeta(map[string]interface{}{"id": "m1"}), `{"linear":{"project_milestone":{"id":"m1"}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			store := newMetaPullStore()
			engine, _ := newMetaPullEngine(store, "new", tc.metadata)
			stats, err := engine.doPull(context.Background(), SyncOptions{Pull: true}, nil, nil)
			if err != nil {
				t.Fatalf("doPull: %v", err)
			}
			if stats.Created != 1 || len(store.created) != 1 {
				t.Fatalf("stats = %+v created=%d, want one create", stats, len(store.created))
			}
			if got := string(store.created[0].Metadata); got != tc.want {
				t.Fatalf("created metadata = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReimportIssueMergesMetadataPreservingSiblings(t *testing.T) {
	for name, tc := range map[string]struct {
		metadata      map[string]interface{}
		wantMilestone interface{}
	}{
		"new milestone":     {milestoneMeta(map[string]interface{}{"id": "m2"}), map[string]interface{}{"id": "m2"}},
		"removed milestone": {milestoneMeta(nil), nil},
	} {
		t.Run(name, func(t *testing.T) {
			local := metaLocalIssue(siblingMetadata)
			store := newMetaPullStore(local)
			engine, _ := newMetaPullEngine(store, "remote title", tc.metadata)

			engine.reimportIssue(context.Background(), Conflict{IssueID: local.ID, ExternalIdentifier: "EXT-1"})

			if _, ok := singleApplied(t, store)["metadata"]; !ok {
				t.Fatal("reimport update has no metadata key")
			}
			if len(store.searchFilters) == 0 || !reflect.DeepEqual(store.searchFilters[len(store.searchFilters)-1].IDs, []string{local.ID}) {
				t.Fatalf("reimport metadata lookup filters = %#v, want IDs=[%s]", store.searchFilters, local.ID)
			}
			m := decodeForTest(t, local.Metadata)
			assertSiblingsSurvive(t, m)
			got, present := linearNS(t, m)["project_milestone"]
			if tc.wantMilestone == nil {
				if present {
					t.Fatalf("linear.project_milestone survived removal: %s", local.Metadata)
				}
			} else if !reflect.DeepEqual(got, tc.wantMilestone) {
				t.Fatalf("linear.project_milestone = %#v, want %#v", got, tc.wantMilestone)
			}
			if local.Title != "remote title" {
				t.Fatalf("title = %q, want remote title", local.Title)
			}
		})
	}
}

func TestReimportIssueMissingLocalKeepsMetadataUnwritten(t *testing.T) {
	store := newMetaPullStore() // local row not findable
	engine, warnings := newMetaPullEngine(store, "remote title", milestoneMeta(map[string]interface{}{"id": "m1"}))

	engine.reimportIssue(context.Background(), Conflict{IssueID: "bd-gone", ExternalIdentifier: "EXT-1"})

	if _, ok := singleApplied(t, store)["metadata"]; ok {
		t.Fatal("reimport wrote metadata without reading the local row (would replace the whole column)")
	}
	if len(*warnings) == 0 {
		t.Fatal("expected a warning when local metadata cannot be read")
	}
}
