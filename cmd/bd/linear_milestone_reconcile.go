package main

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/tracker"
)

// reconcileLinearMilestonesForStore runs the post-sync milestone pass: the
// ONE-SHOT initial assignment of design 5.3. A bead whose direct parent epic
// carries external_ref linear:project-milestone:<id> gets that
// projectMilestoneId on its Linear issue only while the issue has no
// milestone and the bead has never been assigned one. The marker
// metadata.linear.milestone_assigned is written after the Linear update
// succeeds; afterwards Carl owns the milestone (design 5.6), so his moves
// and clears are never undone.
//
// Same gating as reconcileLinearParentsForStore: skipped on scoped syncs
// because it walks every local bead, read-only in dry-run, human output
// suppressed under --json, and every failure surfaces as a
// "milestone reconcile:" warning. A kept Carl milestone is informational
// only (printed, never a warning).
func reconcileLinearMilestonesForStore(ctx context.Context, st tracker.Store, lt *linear.Tracker, opts *tracker.SyncOptions, dryRun, jsonOutput bool, warnings *[]string) {
	if lt == nil || st == nil || syncIsScoped(opts) {
		return
	}
	links, perBead, err := linear.BuildMilestoneLinks(ctx, st)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: building link set failed: %v", err))
		return
	}
	for _, e := range perBead {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: %v", e))
	}
	if len(links) == 0 {
		return
	}
	mark := func(ctx context.Context, link linear.MilestoneLink, milestoneID string) error {
		return linear.WriteMilestoneAssignedMarker(ctx, st, link, milestoneID, actor)
	}
	stats, err := lt.ReconcileMilestones(ctx, links, mark, dryRun)
	if stats != nil && !jsonOutput {
		if dryRun {
			if stats.WouldUpdate > 0 {
				fmt.Printf("[dry-run] Would set %d Linear project milestone%s\n",
					stats.WouldUpdate, plural(stats.WouldUpdate))
				for _, m := range stats.Mutations {
					fmt.Printf("[dry-run] Would set milestone of %s → %s\n", m.ChildIdentifier, m.MilestoneID)
				}
			}
		} else if stats.Updated > 0 {
			fmt.Printf("✓ Set %d Linear project milestone%s\n", stats.Updated, plural(stats.Updated))
		}
		for _, p := range stats.Preserved {
			fmt.Printf("  Kept Linear milestone %s on %s (epic milestone %s)\n",
				p.LinearMilestone, p.ChildIdentifier, p.EpicMilestone)
		}
	}
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: %v", err))
		return
	}
	for _, e := range stats.Errors {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: %v", e))
	}
}
