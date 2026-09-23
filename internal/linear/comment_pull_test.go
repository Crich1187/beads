package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// ---- fake Linear (read-only GraphQL server) ----

// commentPullFakeLinear serves the CommentPullIssueComments query from an
// in-memory comment list per issue number, paginating with opaque cursors.
// It records every request body so tests can assert no mutation was sent.
type commentPullFakeLinear struct {
	t        *testing.T
	mu       sync.Mutex
	pageSize int
	issues   map[int]string              // number -> identifier
	comments map[string][]map[string]any // identifier -> comment nodes (server order)
	queries  []string                    // every request's query text
	failPage map[string]int              // identifier -> page index (0-based) that returns HTTP 500
	vars     []map[string]interface{}    // every request's variables
}

func newCommentPullFakeLinear(t *testing.T) *commentPullFakeLinear {
	return &commentPullFakeLinear{
		t:        t,
		pageSize: 0,
		issues:   map[int]string{},
		comments: map[string][]map[string]any{},
		failPage: map[string]int{},
	}
}

func (f *commentPullFakeLinear) addIssue(identifier string) {
	parts := strings.Split(identifier, "-")
	n, _ := strconv.Atoi(parts[len(parts)-1])
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[n] = identifier
}

func (f *commentPullFakeLinear) addComment(identifier, id, body, createdAt, name, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	node := map[string]any{"id": id, "body": body, "createdAt": createdAt, "user": nil}
	if name != "" || email != "" {
		node["user"] = map[string]any{"id": "u-" + name, "name": name, "email": email, "displayName": strings.ToLower(name)}
	}
	f.comments[identifier] = append(f.comments[identifier], node)
}

func (f *commentPullFakeLinear) mutationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if strings.Contains(strings.ToLower(q), "mutation") {
			n++
		}
	}
	return n
}

func (f *commentPullFakeLinear) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *commentPullFakeLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req GraphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Fatalf("fake linear: bad request JSON: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, req.Query)
	f.vars = append(f.vars, req.Variables)
	w.Header().Set("Content-Type", "application/json")

	if !strings.Contains(req.Query, "CommentPullIssueComments") {
		// Anything else (including any mutation) is answered with an
		// empty payload; the mutation counter is what the tests check.
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		return
	}

	filter, _ := req.Variables["filter"].(map[string]interface{})
	number, _ := filter["number"].(map[string]interface{})
	eq, _ := number["eq"].(float64)
	identifier, ok := f.issues[int(eq)]
	if !ok {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": []any{}}}})
		return
	}

	start := 0
	if after, _ := req.Variables["after"].(string); after != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(after, "cursor-"))
		if err != nil {
			f.t.Fatalf("fake linear: bad cursor %q", after)
		}
		start = n
	}
	size := f.pageSize
	if size <= 0 {
		size = int(req.Variables["first"].(float64))
	}
	all := f.comments[identifier]
	end := start + size
	if end > len(all) {
		end = len(all)
	}
	pageIndex := start / size
	if fp, ok := f.failPage[identifier]; ok && fp == pageIndex {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
		return
	}
	nodes := all[start:end]
	if nodes == nil {
		nodes = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{
			"issues": map[string]any{
				"nodes": []any{map[string]any{
					"id":         "uuid-" + identifier,
					"identifier": identifier,
					"comments": map[string]any{
						"nodes": nodes,
						"pageInfo": map[string]any{
							"hasNextPage": end < len(all),
							"endCursor":   fmt.Sprintf("cursor-%d", end),
						},
					},
				}},
			},
		},
	})
}

// ---- fake bead store ----

type commentPullFakeStore struct {
	mu            sync.Mutex
	issues        map[string]*types.Issue
	comments      map[string][]*types.Comment
	mergeCalls    int
	importCalls   int
	failImportOn  map[int]bool // 1-based import call number that fails
	failMerge     bool
	nextCommentID int
}

func newCommentPullFakeStore(beadIDs ...string) *commentPullFakeStore {
	s := &commentPullFakeStore{
		issues:       map[string]*types.Issue{},
		comments:     map[string][]*types.Comment{},
		failImportOn: map[int]bool{},
	}
	for _, id := range beadIDs {
		s.issues[id] = &types.Issue{ID: id}
	}
	return s
}

func (s *commentPullFakeStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, ok := s.issues[id]
	if !ok {
		return nil, nil
	}
	cp := *iss
	cp.Metadata = append(json.RawMessage(nil), iss.Metadata...)
	return &cp, nil
}

func (s *commentPullFakeStore) GetIssueComments(_ context.Context, issueID string) ([]*types.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*types.Comment, 0, len(s.comments[issueID]))
	for _, c := range s.comments[issueID] {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (s *commentPullFakeStore) ImportIssueComment(_ context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.importCalls++
	if s.failImportOn[s.importCalls] {
		return nil, errors.New("injected import failure")
	}
	if _, ok := s.issues[issueID]; !ok {
		return nil, fmt.Errorf("issue %s not found", issueID)
	}
	s.nextCommentID++
	c := &types.Comment{ID: fmt.Sprintf("c%d", s.nextCommentID), IssueID: issueID, Author: author, Text: text, CreatedAt: createdAt}
	s.comments[issueID] = append(s.comments[issueID], c)
	return c, nil
}

func (s *commentPullFakeStore) MergeMetadata(_ context.Context, issueID, key string, value json.RawMessage, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeCalls++
	if s.failMerge {
		return errors.New("injected metadata failure")
	}
	iss, ok := s.issues[issueID]
	if !ok {
		return fmt.Errorf("issue %s not found", issueID)
	}
	m := map[string]json.RawMessage{}
	if len(iss.Metadata) > 0 {
		if err := json.Unmarshal(iss.Metadata, &m); err != nil {
			return err
		}
	}
	m[key] = value
	b, _ := json.Marshal(m)
	iss.Metadata = b
	return nil
}

// replaceMetadata mimics a pull that writes the whole metadata column.
func (s *commentPullFakeStore) replaceMetadata(issueID string, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[issueID].Metadata = json.RawMessage(raw)
}

func (s *commentPullFakeStore) linearIDs(issueID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, c := range s.comments[issueID] {
		if id := CommentPullMarkerID(c.Author, c.Text); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *commentPullFakeStore) watermark(t *testing.T, issueID string) *commentPullWatermark {
	t.Helper()
	s.mu.Lock()
	md := append(json.RawMessage(nil), s.issues[issueID].Metadata...)
	s.mu.Unlock()
	raw, wm, err := commentPullReadWatermark(md)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if raw == nil {
		return nil
	}
	return wm
}

// ---- helpers ----

func commentPullSetup(t *testing.T) (*commentPullFakeLinear, *Tracker, func()) {
	t.Helper()
	fake := newCommentPullFakeLinear(t)
	server := httptest.NewServer(fake)
	tr := newCommentPullTracker(server.URL)
	return fake, tr, server.Close
}

func newCommentPullTracker(url string) *Tracker {
	return &Tracker{
		teamIDs: []string{"team-uuid"},
		clients: map[string]*Client{"team-uuid": NewClient("test-key", "team-uuid").WithEndpoint(url)},
	}
}

func commentPullRun(t *testing.T, tr *Tracker, st CommentPullStore, targets ...CommentPullTarget) *CommentPullStats {
	t.Helper()
	stats, err := tr.PullComments(context.Background(), st, targets, CommentPullOptions{Actor: "test"})
	if err != nil {
		t.Fatalf("PullComments: %v", err)
	}
	return stats
}

func commentPullAssertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("imported Linear ids = %v, want %v", got, want)
	}
}

var commentPullTarget = CommentPullTarget{BeadID: "bd-1", Identifier: "TEAM-7"}

// ---- tests ----

// First pull: every existing Linear comment becomes one bead comment, in
// createdAt order, attributed to its Linear author, with the original
// timestamp and a marker; the watermark records the newest.
func TestCommentPull_FirstPull(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-b", "second", "2026-09-22T10:05:00.000Z", "Carl Richards", "carl@example.com")
	fake.addComment("TEAM-7", "c-a", "first", "2026-09-22T10:00:00.000Z", "Carl Richards", "carl@example.com")
	fake.addComment("TEAM-7", "c-c", "bot note", "2026-09-22T10:06:00.000Z", "", "")
	st := newCommentPullFakeStore("bd-1")

	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 3 || stats.WatermarksWritten != 1 || len(stats.Errors) != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "c-a", "c-b", "c-c")
	got := st.comments["bd-1"]
	if got[0].Author != "linear:Carl Richards <carl@example.com>" {
		t.Errorf("author = %q", got[0].Author)
	}
	if got[2].Author != "linear:unknown" {
		t.Errorf("userless author = %q", got[2].Author)
	}
	if got[0].Text != "first\n\nlinear-comment-id: c-a" {
		t.Errorf("text = %q", got[0].Text)
	}
	if want := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC); !got[0].CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want %v", got[0].CreatedAt, want)
	}
	wm := st.watermark(t, "bd-1")
	if wm == nil || wm.ID != "c-c" || wm.CreatedAt != "2026-09-22T10:06:00Z" || len(wm.Seen) != 3 {
		t.Fatalf("watermark = %+v", wm)
	}
}

// Incremental pull: only comments added after the previous run are
// imported; the watermark advances.
func TestCommentPull_Incremental(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	commentPullRun(t, tr, st, commentPullTarget)

	fake.addComment("TEAM-7", "c-2", "two", "2026-09-22T11:00:00Z", "Carl", "")
	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 1 || stats.Deduplicated != 0 || stats.WatermarksWritten != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "c-1", "c-2")
	if wm := st.watermark(t, "bd-1"); wm.ID != "c-2" {
		t.Fatalf("watermark id = %q, want c-2", wm.ID)
	}
}

// Restart: a brand-new Tracker (fresh process, nothing in memory) over the
// same store imports nothing and writes nothing.
func TestCommentPull_RestartNoReplay(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	fake.addComment("TEAM-7", "c-2", "two", "2026-09-22T10:01:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	commentPullRun(t, tr, st, commentPullTarget)
	mergesBefore := st.mergeCalls

	server2 := httptest.NewServer(fake)
	defer server2.Close()
	restarted := newCommentPullTracker(server2.URL)
	stats := commentPullRun(t, restarted, st, commentPullTarget)
	if stats.Imported != 0 || stats.Deduplicated != 0 || stats.WatermarksWritten != 0 {
		t.Fatalf("restart stats = %+v", stats)
	}
	if st.mergeCalls != mergesBefore {
		t.Fatalf("restart wrote metadata %d times", st.mergeCalls-mergesBefore)
	}
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "c-1", "c-2")
}

// Out-of-order ids: Linear ids are random, so ordering must come from
// createdAt. A newer comment whose id sorts before the watermark id is
// imported, and so is an unseen comment sharing the watermark's timestamp
// with a smaller id, and a late-arriving one slightly older than the mark.
func TestCommentPull_OutOfOrderIDs(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "zzz", "first", "2026-09-22T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	commentPullRun(t, tr, st, commentPullTarget)
	if wm := st.watermark(t, "bd-1"); wm.ID != "zzz" {
		t.Fatalf("watermark id = %q", wm.ID)
	}

	fake.addComment("TEAM-7", "aaa", "newer, small id", "2026-09-22T10:10:00Z", "Carl", "")
	fake.addComment("TEAM-7", "mmm", "same ts as mark, smaller id", "2026-09-22T10:00:00Z", "Carl", "")
	fake.addComment("TEAM-7", "bbb", "late arrival, older than mark", "2026-09-22T09:59:00Z", "Carl", "")
	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	// Imported in (createdAt, id) order.
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "zzz", "bbb", "mmm", "aaa")
	wm := st.watermark(t, "bd-1")
	if wm.ID != "aaa" || wm.CreatedAt != "2026-09-22T10:10:00Z" {
		t.Fatalf("watermark = %+v", wm)
	}
	for _, id := range []string{"zzz", "mmm", "bbb", "aaa"} {
		if _, ok := wm.Seen[id]; !ok {
			t.Errorf("seen missing %s: %v", id, wm.Seen)
		}
	}

	// And nothing replays on the next run.
	if stats := commentPullRun(t, tr, st, commentPullTarget); stats.Imported != 0 || stats.WatermarksWritten != 0 {
		t.Fatalf("rerun stats = %+v", stats)
	}
}

// Comments older than the grace window behind the mark are treated as
// processed and pruned from "seen", keeping the watermark bounded.
func TestCommentPull_GraceWindowPrunes(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "old", "old", "2026-09-01T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	commentPullRun(t, tr, st, commentPullTarget)
	fake.addComment("TEAM-7", "new", "new", "2026-09-22T10:00:00Z", "Carl", "")
	commentPullRun(t, tr, st, commentPullTarget)
	wm := st.watermark(t, "bd-1")
	if _, ok := wm.Seen["old"]; ok || len(wm.Seen) != 1 {
		t.Fatalf("seen not pruned: %v", wm.Seen)
	}
	if stats := commentPullRun(t, tr, st, commentPullTarget); stats.Imported != 0 {
		t.Fatalf("pruned comment replayed: %+v", stats)
	}
}

// Crash between the comment write and the watermark write: the comments
// are committed but the watermark never lands. The rerun finds their
// markers and imports nothing twice; it then writes the watermark.
func TestCommentPull_CrashBetweenWritesNoDuplicate(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	fake.addComment("TEAM-7", "c-2", "two", "2026-09-22T10:01:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	st.failMerge = true

	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 2 || stats.WatermarksWritten != 0 || len(stats.Errors) != 1 {
		t.Fatalf("crash-run stats = %+v", stats)
	}
	if st.watermark(t, "bd-1") != nil {
		t.Fatal("watermark written despite failure")
	}

	st.failMerge = false
	stats = commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 0 || stats.Deduplicated != 2 || stats.WatermarksWritten != 1 {
		t.Fatalf("recovery stats = %+v", stats)
	}
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "c-1", "c-2")
}

// A pull that replaces the whole metadata column (milestone metadata does
// this today) wipes the watermark; markers still prevent duplicates and
// the watermark is rebuilt without touching unrelated keys.
func TestCommentPull_MetadataWipeNoDuplicate(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	commentPullRun(t, tr, st, commentPullTarget)

	st.replaceMetadata("bd-1", `{"linear":{"project_milestone":{"id":"m1"}}}`)
	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 0 || stats.Deduplicated != 1 || stats.WatermarksWritten != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "c-1")
	if !strings.Contains(string(st.issues["bd-1"].Metadata), `"project_milestone"`) {
		t.Fatalf("unrelated metadata lost: %s", st.issues["bd-1"].Metadata)
	}
}

// Mid-issue import failure: the watermark stays unchanged (no partial
// advance), and the rerun imports only the missing comment.
func TestCommentPull_ImportFailureNoPartialAdvance(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	fake.addComment("TEAM-7", "c-2", "two", "2026-09-22T10:01:00Z", "Carl", "")
	fake.addComment("TEAM-7", "c-3", "three", "2026-09-22T10:02:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	st.failImportOn[2] = true

	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 1 || len(stats.Errors) != 1 || stats.WatermarksWritten != 0 || st.mergeCalls != 0 {
		t.Fatalf("stats = %+v merges=%d", stats, st.mergeCalls)
	}
	stats = commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 2 || stats.Deduplicated != 1 || len(stats.Errors) != 0 {
		t.Fatalf("rerun stats = %+v", stats)
	}
	commentPullAssertIDs(t, st.linearIDs("bd-1"), "c-1", "c-2", "c-3")
}

// A fetch failure on a later page fails the whole issue: nothing imported
// from the earlier page and no watermark written.
func TestCommentPull_PageFailureImportsNothing(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.pageSize = 2
	fake.addIssue("TEAM-7")
	for i := 0; i < 5; i++ {
		fake.addComment("TEAM-7", fmt.Sprintf("c-%d", i), "x", fmt.Sprintf("2026-09-22T10:0%d:00Z", i), "Carl", "")
	}
	fake.failPage["TEAM-7"] = 1
	st := newCommentPullFakeStore("bd-1")

	stats := commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 0 || len(stats.Errors) != 1 || st.mergeCalls != 0 {
		t.Fatalf("stats = %+v merges=%d", stats, st.mergeCalls)
	}

	delete(fake.failPage, "TEAM-7")
	stats = commentPullRun(t, tr, st, commentPullTarget)
	if stats.Imported != 5 {
		t.Fatalf("paginated stats = %+v", stats)
	}
}

// A no-op run (nothing new) issues zero metadata writes, so the bead's
// updated_at is not bumped when nothing changed.
func TestCommentPull_NoOpNoMetadataWrite(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addIssue("TEAM-8")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1", "bd-2")
	targets := []CommentPullTarget{commentPullTarget, {BeadID: "bd-2", Identifier: "TEAM-8"}}
	commentPullRun(t, tr, st, targets...)
	if st.mergeCalls != 1 {
		t.Fatalf("first run merges = %d, want 1 (issue without comments writes none)", st.mergeCalls)
	}
	commentPullRun(t, tr, st, targets...)
	if st.mergeCalls != 1 {
		t.Fatalf("no-op run wrote metadata: merges = %d", st.mergeCalls)
	}
}

// Read-only toward Linear: across first pull, incremental pull, restart,
// dry-run and a not-found issue, every request is a query and none is a
// mutation.
func TestCommentPull_ZeroMutations(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.pageSize = 1
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	fake.addComment("TEAM-7", "c-2", "two", "2026-09-22T10:01:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1", "bd-9")
	targets := []CommentPullTarget{commentPullTarget, {BeadID: "bd-9", Identifier: "TEAM-99"}}

	if _, err := tr.PullComments(context.Background(), st, targets, CommentPullOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	stats := commentPullRun(t, tr, st, targets...)
	if len(stats.NotFound) != 1 || stats.NotFound[0] != "TEAM-99" {
		t.Fatalf("NotFound = %v", stats.NotFound)
	}
	fake.addComment("TEAM-7", "c-3", "three", "2026-09-22T10:02:00Z", "Carl", "")
	commentPullRun(t, tr, st, targets...)

	if fake.requestCount() == 0 {
		t.Fatal("no requests recorded")
	}
	if n := fake.mutationCount(); n != 0 {
		t.Fatalf("comment pass sent %d mutation(s) to Linear", n)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, q := range fake.queries {
		if !strings.HasPrefix(strings.TrimSpace(q), "query CommentPullIssueComments") {
			t.Fatalf("unexpected request: %s", q)
		}
	}
}

// Dry run fetches and counts but writes nothing.
func TestCommentPull_DryRunWritesNothing(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	stats, err := tr.PullComments(context.Background(), st, []CommentPullTarget{commentPullTarget}, CommentPullOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.WouldImport != 1 || stats.Imported != 0 || st.importCalls != 0 || st.mergeCalls != 0 {
		t.Fatalf("stats = %+v imports=%d merges=%d", stats, st.importCalls, st.mergeCalls)
	}
}

// Markers are trusted only on the final line of "linear:"-authored
// comments, so quoted markers and agent comments cannot suppress imports.
func TestCommentPull_MarkerParsing(t *testing.T) {
	cases := []struct {
		author, text, want string
	}{
		{"linear:Carl", "hi\n\nlinear-comment-id: abc", "abc"},
		{"linear:Carl", "linear-comment-id: abc\n", "abc"},
		{"linear:Carl", "linear-comment-id: abc\nmore text", ""},
		{"agent-x", "hi\n\nlinear-comment-id: abc", ""},
		{"linear:Carl", "no marker", ""},
	}
	for _, tc := range cases {
		if got := CommentPullMarkerID(tc.author, tc.text); got != tc.want {
			t.Errorf("CommentPullMarkerID(%q, %q) = %q, want %q", tc.author, tc.text, got, tc.want)
		}
	}
	if got := CommentPullText("", "abc"); got != "linear-comment-id: abc" {
		t.Errorf("empty-body text = %q", got)
	}
	// A body that quotes someone else's marker still gets its own marker last.
	text := CommentPullText("quoting:\nlinear-comment-id: other", "mine")
	if got := CommentPullMarkerID("linear:Carl", text); got != "mine" {
		t.Errorf("quoted marker parsed as %q", got)
	}
}

func TestCommentPull_AuthorAttribution(t *testing.T) {
	cases := []struct {
		u    *User
		want string
	}{
		{&User{Name: "Carl Richards", Email: "carl@example.com"}, "linear:Carl Richards <carl@example.com>"},
		{&User{Name: "Carl Richards"}, "linear:Carl Richards"},
		{&User{DisplayName: "carl"}, "linear:carl"},
		{&User{Email: "carl@example.com"}, "linear:carl@example.com"},
		{&User{}, "linear:unknown"},
		{nil, "linear:unknown"},
	}
	for _, tc := range cases {
		if got := CommentPullAuthor(tc.u); got != tc.want {
			t.Errorf("CommentPullAuthor(%+v) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

// An unreadable watermark is reported and rebuilt; markers prevent
// duplicates during the rescan.
func TestCommentPull_CorruptWatermarkRebuilt(t *testing.T) {
	fake, tr, done := commentPullSetup(t)
	defer done()
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "one", "2026-09-22T10:00:00Z", "Carl", "")
	st := newCommentPullFakeStore("bd-1")
	commentPullRun(t, tr, st, commentPullTarget)
	st.replaceMetadata("bd-1", `{"linear.comment_watermark":{"v":99}}`)

	stats := commentPullRun(t, tr, st, commentPullTarget)
	if len(stats.Warnings) != 1 || stats.Imported != 0 || stats.Deduplicated != 1 || stats.WatermarksWritten != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if wm := st.watermark(t, "bd-1"); wm == nil || wm.ID != "c-1" {
		t.Fatalf("watermark not rebuilt: %+v", wm)
	}
}

// The watermark serializes deterministically (sorted seen keys), so an
// unchanged watermark compares equal and is never rewritten.
func TestCommentPull_WatermarkCanonical(t *testing.T) {
	wm := &commentPullWatermark{}
	c := func(id, ts string) commentPullComment {
		at, _ := time.Parse(time.RFC3339Nano, ts)
		return commentPullComment{ID: id, createdAt: at}
	}
	next := wm.advance([]commentPullComment{c("b", "2026-09-22T10:00:00Z"), c("a", "2026-09-22T10:00:00Z")})
	raw1, err := next.marshal()
	if err != nil {
		t.Fatal(err)
	}
	md, _ := json.Marshal(map[string]json.RawMessage{CommentPullWatermarkKey: raw1})
	raw2, parsed, err := commentPullReadWatermark(md)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw1) != string(raw2) {
		t.Fatalf("not canonical:\n%s\n%s", raw1, raw2)
	}
	if parsed.ID != "b" { // equal timestamps: id tie-break picks "b"
		t.Fatalf("tie-break id = %q, want b", parsed.ID)
	}
	keys := make([]string, 0, len(parsed.Seen))
	for k := range parsed.Seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "a,b" {
		t.Fatalf("seen = %v", keys)
	}
}
