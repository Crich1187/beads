package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// The Dolt stores provide everything the comment pass needs.
var _ linear.CommentPullStore = (storage.DoltStorage)(nil)

// pullLinearCommentsForStore runs the post-sync comment pull pass: new Linear
// comments on linked issues become bead comments (see linear.PullComments).
// It is read-only toward Linear and never pushes bead comments.
//
// commentStore is the raw storage value; it must implement
// linear.CommentPullStore (the Dolt stores do; the proxied-server tracker
// adapter does not). When it does not, the pass is skipped with a warning
// rather than silently.
//
// Pinned and hooked beads are excluded, as are ephemeral beads. Failures are
// surfaced through warnings, like the parent reconcile pass; an issue that
// fails keeps its previous watermark, so the next sync retries it.
func pullLinearCommentsForStore(ctx context.Context, st tracker.Store, commentStore interface{}, lt *linear.Tracker, dryRun, jsonOutput bool, warnings *[]string) {
	if lt == nil || st == nil {
		return
	}
	cs, ok := commentStore.(linear.CommentPullStore)
	if !ok || cs == nil || usesProxiedServer() {
		*warnings = append(*warnings, "comment pull: skipped: this storage mode cannot import Linear comments")
		return
	}
	targets, err := buildLinearCommentPullTargets(ctx, st, lt)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("comment pull: building target set failed: %v", err))
		return
	}
	if len(targets) == 0 {
		return
	}
	stats, err := lt.PullComments(ctx, cs, targets, linear.CommentPullOptions{DryRun: dryRun, Actor: actor})
	if stats != nil && !jsonOutput {
		if dryRun {
			if stats.WouldImport > 0 {
				fmt.Printf("[dry-run] Would import %d Linear comment%s\n", stats.WouldImport, plural(stats.WouldImport))
			}
		} else if stats.Imported > 0 {
			fmt.Printf("✓ Imported %d Linear comment%s\n", stats.Imported, plural(stats.Imported))
		}
	}
	if stats != nil {
		for _, w := range stats.Warnings {
			*warnings = append(*warnings, "comment pull: "+w)
		}
		for _, e := range stats.Errors {
			*warnings = append(*warnings, fmt.Sprintf("comment pull: %v", e))
		}
	}
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("comment pull: aborted: %v", err))
	}
}

// buildLinearCommentPullTargets lists local beads linked to a Linear issue,
// excluding pinned, hooked and ephemeral beads.
func buildLinearCommentPullTargets(ctx context.Context, st tracker.Store, lt *linear.Tracker) ([]linear.CommentPullTarget, error) {
	issues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, err
	}
	var targets []linear.CommentPullTarget
	for _, issue := range issues {
		if issue == nil || issue.ExternalRef == nil || issue.Ephemeral {
			continue
		}
		if issue.Status == types.StatusPinned || issue.Status == types.StatusHooked {
			continue
		}
		ref := strings.TrimSpace(*issue.ExternalRef)
		if !lt.IsExternalRef(ref) {
			continue
		}
		ident := lt.ExtractIdentifier(ref)
		if ident == "" {
			continue
		}
		targets = append(targets, linear.CommentPullTarget{BeadID: issue.ID, Identifier: ident})
	}
	return targets, nil
}
