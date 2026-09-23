package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// Comment pull pass (fork patch 5).
//
// PullComments copies Linear comments on linked issues into bead comments.
// It is strictly read-only toward Linear: it issues GraphQL queries only,
// never a mutation, and it never pushes bead comments to Linear (v1).
//
// # Watermark
//
// Each bead keeps a per-issue watermark in its metadata under
// CommentPullWatermarkKey:
//
//	{"v":1,"created_at":"2026-09-22T10:00:00.123Z","id":"<linear comment uuid>",
//	 "seen":{"<linear comment uuid>":"2026-09-22T10:00:00.123Z", ...}}
//
// (created_at, id) is the high-water mark: the newest processed comment, with
// the id used only as a deterministic tie-break between equal timestamps.
// Linear comment ids are random UUIDs, so ids are never ordered lexically on
// their own. "seen" holds every processed comment id whose createdAt lies
// within commentPullGraceWindow of the high-water mark, so a comment that
// shares a timestamp with (or arrives late behind) the mark is still picked
// up exactly once. A comment is new when its id is not in "seen" and it is
// not older than created_at minus the grace window.
//
// The watermark is written only after every comment for that issue has been
// committed, and only when its serialized form changes. A failure anywhere in
// an issue's fetch or import leaves that issue's watermark untouched.
//
// # Replay protection
//
// The watermark is an optimization, not the sole guard. Every imported
// comment ends with a marker line ("linear-comment-id: <uuid>") and is
// authored "linear:...". Before importing, the pass reads the bead's
// existing comments and skips any Linear id that already has a marker. That
// covers a crash between the comment write and the watermark write, and a
// watermark lost because a pull replaced the whole metadata column.
//
// # Attribution
//
// The bead comment author is "linear:<name> <email>" when Linear returns
// both, otherwise "linear:<name>", "linear:<email>" or "linear:unknown"
// (for integration or deleted-user comments with no user). The "linear:"
// prefix keeps imported comments distinguishable from agent comments, and
// the email disambiguates people who share a display name. The original
// Linear createdAt is preserved on the bead comment.

// CommentPullWatermarkKey is the bead metadata key holding the per-issue
// comment pull watermark.
const CommentPullWatermarkKey = "linear.comment_watermark"

// CommentPullAuthorPrefix prefixes the author of every bead comment imported
// from Linear.
const CommentPullAuthorPrefix = "linear:"

// CommentPullMarkerPrefix starts the final line of every bead comment imported
// from Linear; the Linear comment id follows it.
const CommentPullMarkerPrefix = "linear-comment-id: "

// commentPullGraceWindow bounds how far behind the high-water mark an unseen
// comment is still accepted, and how long processed ids stay in "seen".
const commentPullGraceWindow = 24 * time.Hour

// commentPullPageSize is the Linear comments page size.
const commentPullPageSize = 50

// commentPullMaxPages caps pagination per issue so a misbehaving cursor
// cannot loop forever (50 * 200 = 10,000 comments on one issue).
const commentPullMaxPages = 200

// commentPullWatermarkVersion is the "v" field of the stored watermark.
const commentPullWatermarkVersion = 1

// CommentPullStore is the storage surface the comment pass needs. The Dolt
// stores (storage.DoltStorage) satisfy it.
type CommentPullStore interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error)
	ImportIssueComment(ctx context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error)
	MergeMetadata(ctx context.Context, issueID, key string, value json.RawMessage, actor string) error
}

// CommentPullTarget is one linked bead whose Linear issue should be scanned.
type CommentPullTarget struct {
	BeadID     string
	Identifier string // Linear identifier, e.g. "TEAM-123"
}

// CommentPullOptions controls a PullComments run.
type CommentPullOptions struct {
	// DryRun fetches and counts but writes nothing (no comments, no watermark).
	DryRun bool
	// Actor is recorded on the watermark metadata write.
	Actor string
}

// CommentPullStats summarizes a PullComments run.
type CommentPullStats struct {
	// IssuesScanned counts targets whose Linear comments were fetched.
	IssuesScanned int
	// Imported counts bead comments created. Zero in dry-run.
	Imported int
	// WouldImport counts comments a wet run would import. Dry-run only.
	WouldImport int
	// Deduplicated counts new-looking Linear comments skipped because the
	// bead already holds a comment carrying their marker.
	Deduplicated int
	// WatermarksWritten counts metadata watermark writes.
	WatermarksWritten int
	// NotFound lists identifiers that no configured team returned.
	NotFound []string
	// Warnings are non-fatal notes (e.g. an unreadable watermark was rebuilt).
	Warnings []string
	// Errors collects per-issue failures. An issue with an error had its
	// watermark left unchanged.
	Errors []error
}

// commentPullComment is one Linear comment as returned by the query.
type commentPullComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	User      *User  `json:"user"`

	createdAt time.Time
}

type commentPullWatermark struct {
	Version   int               `json:"v"`
	CreatedAt string            `json:"created_at,omitempty"`
	ID        string            `json:"id,omitempty"`
	Seen      map[string]string `json:"seen,omitempty"`

	createdAt time.Time
	seenAt    map[string]time.Time
}

// commentPullIssueCommentsQuery reuses the team+number lookup envelope of
// FetchIssueByIdentifier and nests the issue's comments connection.
const commentPullIssueCommentsQuery = `
	query CommentPullIssueComments($filter: IssueFilter!, $first: Int!, $after: String) {
		issues(filter: $filter, first: 1) {
			nodes {
				id
				identifier
				comments(first: $first, after: $after) {
					nodes {
						id
						body
						createdAt
						user {
							id
							name
							email
							displayName
						}
					}
					pageInfo {
						hasNextPage
						endCursor
					}
				}
			}
		}
	}
`

type commentPullResponse struct {
	Issues struct {
		Nodes []struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
			Comments   struct {
				Nodes    []commentPullComment `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"issues"`
}

// commentPullFetchPage fetches one page of an issue's comments. found is false
// when this client's team has no issue with that identifier.
func (c *Client) commentPullFetchPage(ctx context.Context, identifier, after string) (page []commentPullComment, next string, hasNext, found bool, err error) {
	parts := strings.Split(identifier, "-")
	number, convErr := strconv.Atoi(parts[len(parts)-1])
	if len(parts) < 2 || convErr != nil {
		return nil, "", false, false, fmt.Errorf("cannot parse Linear identifier %q", identifier)
	}
	variables := map[string]interface{}{
		"filter": map[string]interface{}{
			"team":   map[string]interface{}{"id": map[string]interface{}{"eq": c.TeamID}},
			"number": map[string]interface{}{"eq": number},
		},
		"first": commentPullPageSize,
	}
	if after != "" {
		variables["after"] = after
	}
	data, err := c.Execute(ctx, &GraphQLRequest{Query: commentPullIssueCommentsQuery, Variables: variables})
	if err != nil {
		return nil, "", false, false, fmt.Errorf("fetch comments for %s: %w", identifier, err)
	}
	var resp commentPullResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, "", false, false, fmt.Errorf("parse comments for %s: %w", identifier, err)
	}
	for _, node := range resp.Issues.Nodes {
		if node.Identifier != identifier {
			continue
		}
		pi := node.Comments.PageInfo
		return node.Comments.Nodes, pi.EndCursor, pi.HasNextPage, true, nil
	}
	return nil, "", false, false, nil
}

// commentPullFetchAll returns every comment on the issue, or an error. It never
// returns a partial list: a failure on any page fails the whole issue.
func (t *Tracker) commentPullFetchAll(ctx context.Context, identifier string) ([]commentPullComment, bool, error) {
	var clients []*Client
	if len(t.teamIDs) <= 1 {
		if c := t.primaryClient(); c != nil {
			clients = append(clients, c)
		}
	} else {
		for _, teamID := range t.teamIDs {
			if c := t.clients[teamID]; c != nil {
				clients = append(clients, c)
			}
		}
	}
	if len(clients) == 0 {
		return nil, false, errors.New("no Linear client available")
	}

	var host *Client
	var all []commentPullComment
	var cursor string
	var hasNext bool
	var probeErr error
	for _, c := range clients {
		page, next, more, found, err := c.commentPullFetchPage(ctx, identifier, "")
		if err != nil {
			if isRateLimitExhausted(err) || ctx.Err() != nil {
				return nil, false, err
			}
			// Keep probing other teams, but remember the failure: an
			// unreachable team must not be reported as "not found".
			if probeErr == nil {
				probeErr = err
			}
			continue
		}
		if found {
			host, all, cursor, hasNext = c, page, next, more
			break
		}
	}
	if host == nil {
		if probeErr != nil {
			return nil, false, probeErr
		}
		return nil, false, nil
	}

	for pages := 1; hasNext; pages++ {
		if pages >= commentPullMaxPages {
			return nil, true, fmt.Errorf("fetch comments for %s: exceeded %d pages", identifier, commentPullMaxPages)
		}
		if cursor == "" {
			return nil, true, fmt.Errorf("fetch comments for %s: hasNextPage without endCursor", identifier)
		}
		page, next, more, found, err := host.commentPullFetchPage(ctx, identifier, cursor)
		if err != nil {
			return nil, true, err
		}
		if !found {
			return nil, true, fmt.Errorf("fetch comments for %s: issue disappeared during pagination", identifier)
		}
		if more && next == cursor {
			return nil, true, fmt.Errorf("fetch comments for %s: cursor did not advance", identifier)
		}
		all = append(all, page...)
		cursor, hasNext = next, more
	}
	return all, true, nil
}

// PullComments imports new Linear comments on each target's issue as bead
// comments. It issues only GraphQL queries against Linear.
//
// A nil error means the pass ran to completion; per-issue failures are in
// stats.Errors. A non-nil error means the pass aborted (rate-limit circuit
// breaker tripped or context canceled); issues processed before the abort
// keep their committed comments and watermarks.
func (t *Tracker) PullComments(ctx context.Context, st CommentPullStore, targets []CommentPullTarget, opts CommentPullOptions) (*CommentPullStats, error) {
	stats := &CommentPullStats{}
	if len(targets) == 0 {
		return stats, nil
	}
	if st == nil {
		return nil, errors.New("comment pull: no store")
	}
	if t.primaryClient() == nil {
		return nil, errors.New("no Linear client available")
	}
	for _, target := range targets {
		if target.BeadID == "" || target.Identifier == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := t.commentPullIssue(ctx, st, target, opts, stats); err != nil {
			if isRateLimitExhausted(err) || ctx.Err() != nil {
				return stats, fmt.Errorf("%s (%s): %w", target.BeadID, target.Identifier, err)
			}
			stats.Errors = append(stats.Errors, fmt.Errorf("%s (%s): %w", target.BeadID, target.Identifier, err))
		}
	}
	return stats, nil
}

func (t *Tracker) commentPullIssue(ctx context.Context, st CommentPullStore, target CommentPullTarget, opts CommentPullOptions, stats *CommentPullStats) error {
	bead, err := st.GetIssue(ctx, target.BeadID)
	if err != nil {
		return fmt.Errorf("read bead: %w", err)
	}
	if bead == nil {
		return errors.New("read bead: not found")
	}
	storedRaw, wm, err := commentPullReadWatermark(bead.Metadata)
	if err != nil {
		stats.Warnings = append(stats.Warnings, fmt.Sprintf("%s: unreadable comment watermark, rescanning (marker dedupe prevents duplicates): %v", target.BeadID, err))
		storedRaw, wm = nil, &commentPullWatermark{}
	}

	comments, found, err := t.commentPullFetchAll(ctx, target.Identifier)
	if err != nil {
		return err
	}
	if !found {
		stats.NotFound = append(stats.NotFound, target.Identifier)
		return nil
	}
	stats.IssuesScanned++

	for i := range comments {
		c := &comments[i]
		if c.ID == "" {
			return errors.New("Linear comment without id")
		}
		ts, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err != nil {
			return fmt.Errorf("Linear comment %s: bad createdAt %q: %w", c.ID, c.CreatedAt, err)
		}
		c.createdAt = ts.UTC()
	}
	sort.Slice(comments, func(i, j int) bool {
		return commentPullLess(comments[i].createdAt, comments[i].ID, comments[j].createdAt, comments[j].ID)
	})

	var candidates []commentPullComment
	for _, c := range comments {
		if wm.isProcessed(c) {
			continue
		}
		candidates = append(candidates, c)
	}

	var processed []commentPullComment
	if len(candidates) > 0 {
		existing, err := st.GetIssueComments(ctx, target.BeadID)
		if err != nil {
			return fmt.Errorf("read bead comments: %w", err)
		}
		imported := commentPullImportedIDs(existing)
		for _, c := range candidates {
			if imported[c.ID] {
				stats.Deduplicated++
				processed = append(processed, c)
				continue
			}
			if opts.DryRun {
				stats.WouldImport++
				continue
			}
			if _, err := st.ImportIssueComment(ctx, target.BeadID, CommentPullAuthor(c.User), CommentPullText(c.Body, c.ID), c.createdAt); err != nil {
				// Fail closed: leave the watermark where it was. Comments
				// already committed above carry markers, so a rerun
				// skips them and imports only what is missing.
				return fmt.Errorf("import Linear comment %s: %w", c.ID, err)
			}
			imported[c.ID] = true
			stats.Imported++
			processed = append(processed, c)
		}
	}

	if opts.DryRun {
		return nil
	}
	next := wm.advance(processed)
	nextRaw, err := next.marshal()
	if err != nil {
		return err
	}
	if nextRaw == nil || (storedRaw != nil && string(storedRaw) == string(nextRaw)) {
		return nil
	}
	if err := st.MergeMetadata(ctx, target.BeadID, CommentPullWatermarkKey, nextRaw, opts.Actor); err != nil {
		return fmt.Errorf("write comment watermark: %w", err)
	}
	stats.WatermarksWritten++
	return nil
}

// CommentPullAuthor renders the bead comment author for a Linear user.
func CommentPullAuthor(u *User) string {
	if u == nil {
		return CommentPullAuthorPrefix + "unknown"
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = strings.TrimSpace(u.DisplayName)
	}
	email := strings.TrimSpace(u.Email)
	switch {
	case name != "" && email != "":
		return CommentPullAuthorPrefix + name + " <" + email + ">"
	case name != "":
		return CommentPullAuthorPrefix + name
	case email != "":
		return CommentPullAuthorPrefix + email
	}
	return CommentPullAuthorPrefix + "unknown"
}

// CommentPullText renders the bead comment text: the Linear body followed by a
// marker line carrying the Linear comment id.
func CommentPullText(body, linearCommentID string) string {
	marker := CommentPullMarkerPrefix + linearCommentID
	body = strings.TrimRight(body, " \t\r\n")
	if body == "" {
		return marker
	}
	return body + "\n\n" + marker
}

// CommentPullMarkerID returns the Linear comment id carried by an imported bead
// comment, or "" when the comment was not imported by this pass. Only the
// final line is trusted, and only on "linear:"-authored comments, so a body
// that quotes a marker cannot suppress a different comment.
func CommentPullMarkerID(author, text string) string {
	if !strings.HasPrefix(author, CommentPullAuthorPrefix) {
		return ""
	}
	text = strings.TrimRight(text, " \t\r\n")
	last := text
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		last = text[i+1:]
	}
	last = strings.TrimSpace(last)
	if !strings.HasPrefix(last, CommentPullMarkerPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(last, CommentPullMarkerPrefix))
}

func commentPullImportedIDs(existing []*types.Comment) map[string]bool {
	ids := make(map[string]bool, len(existing))
	for _, c := range existing {
		if c == nil {
			continue
		}
		if id := CommentPullMarkerID(c.Author, c.Text); id != "" {
			ids[id] = true
		}
	}
	return ids
}

func commentPullLess(at time.Time, aID string, bt time.Time, bID string) bool {
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return aID < bID
}

// commentPullReadWatermark extracts the watermark from bead metadata. It
// returns the stored raw JSON (nil when absent) and the parsed watermark.
func commentPullReadWatermark(metadata json.RawMessage) (json.RawMessage, *commentPullWatermark, error) {
	wm := &commentPullWatermark{}
	trimmed := strings.TrimSpace(string(metadata))
	if trimmed == "" || trimmed == "null" {
		return nil, wm, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &m); err != nil {
		return nil, wm, fmt.Errorf("parse bead metadata: %w", err)
	}
	raw, ok := m[CommentPullWatermarkKey]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return nil, wm, nil
	}
	if err := json.Unmarshal(raw, wm); err != nil {
		return nil, &commentPullWatermark{}, fmt.Errorf("parse %s: %w", CommentPullWatermarkKey, err)
	}
	if wm.Version != commentPullWatermarkVersion {
		return nil, &commentPullWatermark{}, fmt.Errorf("unsupported %s version %d", CommentPullWatermarkKey, wm.Version)
	}
	if wm.CreatedAt != "" {
		ts, err := time.Parse(time.RFC3339Nano, wm.CreatedAt)
		if err != nil {
			return nil, &commentPullWatermark{}, fmt.Errorf("parse %s created_at: %w", CommentPullWatermarkKey, err)
		}
		wm.createdAt = ts.UTC()
	}
	wm.seenAt = make(map[string]time.Time, len(wm.Seen))
	for id, at := range wm.Seen {
		ts, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, &commentPullWatermark{}, fmt.Errorf("parse %s seen[%s]: %w", CommentPullWatermarkKey, id, err)
		}
		wm.seenAt[id] = ts.UTC()
	}
	// Canonical re-encoding of what was stored, so an unchanged watermark
	// compares equal and is not rewritten.
	canonical, err := wm.marshal()
	if err != nil {
		return nil, &commentPullWatermark{}, err
	}
	return canonical, wm, nil
}

// isProcessed reports whether c is at or behind the watermark.
func (w *commentPullWatermark) isProcessed(c commentPullComment) bool {
	if w.ID == "" {
		return false
	}
	if _, ok := w.seenAt[c.ID]; ok {
		return true
	}
	return c.createdAt.Before(w.createdAt.Add(-commentPullGraceWindow))
}

// advance returns the watermark after the given comments are processed.
func (w *commentPullWatermark) advance(processed []commentPullComment) *commentPullWatermark {
	next := &commentPullWatermark{
		Version:   commentPullWatermarkVersion,
		ID:        w.ID,
		createdAt: w.createdAt,
		seenAt:    make(map[string]time.Time, len(w.seenAt)+len(processed)),
	}
	for id, at := range w.seenAt {
		next.seenAt[id] = at
	}
	for _, c := range processed {
		next.seenAt[c.ID] = c.createdAt
		if next.ID == "" || commentPullLess(next.createdAt, next.ID, c.createdAt, c.ID) {
			next.createdAt, next.ID = c.createdAt, c.ID
		}
	}
	floor := next.createdAt.Add(-commentPullGraceWindow)
	for id, at := range next.seenAt {
		if at.Before(floor) {
			delete(next.seenAt, id)
		}
	}
	return next
}

// marshal renders the watermark canonically; nil when it holds nothing.
func (w *commentPullWatermark) marshal() (json.RawMessage, error) {
	if w.ID == "" {
		return nil, nil
	}
	out := commentPullWatermark{
		Version:   commentPullWatermarkVersion,
		CreatedAt: w.createdAt.UTC().Format(time.RFC3339Nano),
		ID:        w.ID,
		Seen:      make(map[string]string, len(w.seenAt)),
	}
	for id, at := range w.seenAt {
		out.Seen[id] = at.UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", CommentPullWatermarkKey, err)
	}
	return b, nil
}
