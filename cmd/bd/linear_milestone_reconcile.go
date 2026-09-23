package main

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/tracker"
)

// reconcileLinearMilestonesForStore runs the post-sync milestone pass
// (design 5.3 milestone assignment rule): a bead whose direct parent epic
// carries external_ref linear:project-milestone:<id> gets that
// projectMilestoneId on its Linear issue when the current value differs.
//
// Same shape and gating as reconcileLinearParentsForStore: idempotent
// (fetch-before-update), read-only in dry-run, skipped on scoped syncs
// because it walks every local bead, human output suppressed under --json,
// and every failure surfaces as a "milestone reconcile:" warning.
//
// Milestones remain pull-only otherwise: the pass never clears a milestone
// and never creates or edits milestones themselves.
func reconcileLinearMilestonesForStore(ctx context.Context, st tracker.Store, lt *linear.Tracker, opts *tracker.SyncOptions, dryRun, jsonOutput bool, warnings *[]string) {
	if lt == nil || st == nil || syncIsScoped(opts) {
		return
	}
	links, err := linear.BuildMilestoneLinks(ctx, st)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: building link set failed: %v", err))
		return
	}
	if len(links) == 0 {
		return
	}
	stats, err := lt.ReconcileMilestones(ctx, links, dryRun)
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
	}
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: %v", err))
		return
	}
	for _, e := range stats.Errors {
		*warnings = append(*warnings, fmt.Sprintf("milestone reconcile: %v", e))
	}
}
