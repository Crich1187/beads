//go:build cgo

package linear

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
)

var _ CommentPullStore = (*embeddeddolt.EmbeddedDoltStore)(nil)

// Round trip against the real embedded Dolt store: the marker and author
// survive storage exactly, so reruns and a wiped watermark never duplicate.
func TestCommentPull_EmbeddedDoltRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := embeddeddolt.Open(ctx, filepath.Join(t.TempDir(), ".beads"), "cpull", "main")
	if err != nil {
		t.Fatalf("Open embedded store: %v", err)
	}
	defer store.Close()
	if err := store.SetConfig(ctx, "issue_prefix", "cpull"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	bead := &types.Issue{ID: "cpull-1", Title: "decision", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, bead, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	fake := newCommentPullFakeLinear(t)
	server := httptest.NewServer(fake)
	defer server.Close()
	tr := newCommentPullTracker(server.URL)
	fake.addIssue("TEAM-7")
	fake.addComment("TEAM-7", "c-1", "Option B.\nGo ahead.", "2026-09-22T10:00:00.250Z", "Carl Richards", "carl@example.com")
	fake.addComment("TEAM-7", "c-2", "", "2026-09-22T10:00:01.750Z", "", "")
	target := []CommentPullTarget{{BeadID: "cpull-1", Identifier: "TEAM-7"}}
	run := func() *CommentPullStats {
		t.Helper()
		stats, err := tr.PullComments(ctx, store, target, CommentPullOptions{Actor: "tester"})
		if err != nil {
			t.Fatalf("PullComments: %v", err)
		}
		if len(stats.Errors) != 0 || len(stats.Warnings) != 0 {
			t.Fatalf("errors=%v warnings=%v", stats.Errors, stats.Warnings)
		}
		return stats
	}

	if s := run(); s.Imported != 2 || s.WatermarksWritten != 1 {
		t.Fatalf("first run = %+v", s)
	}
	if s := run(); s.Imported != 0 || s.Deduplicated != 0 || s.WatermarksWritten != 0 {
		t.Fatalf("second run = %+v", s)
	}
	// A pull that replaces the whole metadata column wipes the watermark.
	if err := store.UpdateIssue(ctx, "cpull-1", map[string]interface{}{"metadata": `{"linear":{"x":1}}`}, "tester"); err != nil {
		t.Fatalf("replace metadata: %v", err)
	}
	if s := run(); s.Imported != 0 || s.Deduplicated != 2 || s.WatermarksWritten != 1 {
		t.Fatalf("after wipe = %+v", s)
	}

	comments, err := store.GetIssueComments(ctx, "cpull-1")
	if err != nil {
		t.Fatalf("GetIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].Author != "linear:Carl Richards <carl@example.com>" ||
		comments[0].Text != "Option B.\nGo ahead.\n\nlinear-comment-id: c-1" {
		t.Fatalf("comment 0 = %q / %q", comments[0].Author, comments[0].Text)
	}
	if comments[1].Author != "linear:unknown" || comments[1].Text != "linear-comment-id: c-2" {
		t.Fatalf("comment 1 = %q / %q", comments[1].Author, comments[1].Text)
	}
	if want := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC); !comments[0].CreatedAt.Truncate(time.Second).Equal(want) {
		t.Fatalf("createdAt = %v, want %v (second precision)", comments[0].CreatedAt, want)
	}
	issue, err := store.GetIssue(ctx, "cpull-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	md := string(issue.Metadata)
	if !strings.Contains(md, `"linear.comment_watermark"`) || !strings.Contains(md, `"linear"`) {
		t.Fatalf("metadata = %s", md)
	}
	if fake.mutationCount() != 0 {
		t.Fatal("mutation sent to Linear")
	}
}
