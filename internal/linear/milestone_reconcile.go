package linear

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// ProjectMilestoneExternalRefPrefix marks a local epic that mirrors a Linear
// project milestone (pulled with `bd linear sync --milestones`). The suffix
// is the milestone's Linear UUID, i.e. the same value IssueUpdateInput's
// projectMilestoneId expects.
//
// cmd/bd keeps its own copy of this prefix for the pull side; the two must
// stay identical.
const ProjectMilestoneExternalRefPrefix = "linear:project-milestone:"

// ProjectMilestoneIDFromExternalRef returns the milestone UUID carried by a
// `linear:project-milestone:<id>` external ref, or "" when ref is not a
// milestone ref (or has an empty id).
func ProjectMilestoneIDFromExternalRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, ProjectMilestoneExternalRefPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(ref, ProjectMilestoneExternalRefPrefix))
}

// MilestoneLink describes the desired Linear project milestone for one
// child issue. MilestoneIDs normally holds exactly one id: the milestone of
// the child's direct parent epic. It holds several only when the bead has
// more than one milestone-epic parent (e.g. Carl moved the issue in Linear
// and a --milestones pull added a second parent edge without removing the
// first). In that case the pass never picks one: it leaves Linear alone when
// the current milestone is one of the candidates and reports an error
// otherwise.
type MilestoneLink struct {
	ChildIdentifier string
	MilestoneIDs    []string
}

// MilestoneLinkStore is the narrow read-only store surface the milestone
// link builder needs. tracker.Store satisfies it.
type MilestoneLinkStore interface {
	SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error)
	GetDependenciesWithMetadata(context.Context, string) ([]*types.IssueWithDependencyMetadata, error)
}

// BuildMilestoneLinks enumerates local beads that have a Linear issue ref and
// a direct parent-child edge to an epic whose external_ref is
// `linear:project-milestone:<id>`. Only the direct parent is considered:
// grandchildren of a milestone epic hang beneath ordinary epic issues and
// are not assigned (design 5.3).
//
// Beads without a milestone parent produce no link, so the pass never
// touches (or clears) a milestone Carl set by hand in Linear.
//
// Any store error aborts the build before any Linear call is made.
func BuildMilestoneLinks(ctx context.Context, st MilestoneLinkStore) ([]MilestoneLink, error) {
	if st == nil {
		return nil, errors.New("database not available")
	}
	issues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, fmt.Errorf("listing local issues: %w", err)
	}

	milestoneByBead := make(map[string]string)
	childIdent := make(map[string]string)
	for _, issue := range issues {
		if issue == nil || issue.ExternalRef == nil {
			continue
		}
		ref := strings.TrimSpace(*issue.ExternalRef)
		if msID := ProjectMilestoneIDFromExternalRef(ref); msID != "" {
			milestoneByBead[issue.ID] = msID
			continue
		}
		if !IsLinearExternalRef(ref) {
			continue
		}
		if ident := ExtractLinearIdentifier(ref); ident != "" {
			childIdent[issue.ID] = ident
		}
	}
	if len(milestoneByBead) == 0 || len(childIdent) == 0 {
		return nil, nil
	}

	links := make([]MilestoneLink, 0)
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		ident, ok := childIdent[issue.ID]
		if !ok {
			continue
		}
		deps, err := st.GetDependenciesWithMetadata(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("loading deps for %s: %w", issue.ID, err)
		}
		seen := make(map[string]bool)
		var ids []string
		for _, d := range deps {
			if d == nil || d.DependencyType != types.DepParentChild {
				continue
			}
			msID, ok := milestoneByBead[d.Issue.ID]
			if !ok || seen[msID] {
				continue
			}
			seen[msID] = true
			ids = append(ids, msID)
		}
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		links = append(links, MilestoneLink{ChildIdentifier: ident, MilestoneIDs: ids})
	}
	return links, nil
}

// MilestoneMutation records one projectMilestoneId assignment that was
// applied (wet-run) or would be applied (dry-run).
type MilestoneMutation struct {
	ChildIdentifier string
	MilestoneID     string
}

// MilestoneReconcileStats summarizes a ReconcileMilestones run. Fields
// mirror ParentReconcileStats.
type MilestoneReconcileStats struct {
	// Updated counts issues whose projectMilestoneId was set. Zero in dry-run.
	Updated int
	// WouldUpdate counts mutations the pass would issue. Dry-run only.
	WouldUpdate int
	// Mutations lists assignments applied (wet-run, appended only after the
	// API call succeeds) or planned (dry-run).
	Mutations []MilestoneMutation
	// Skipped counts links whose Linear milestone already matched.
	Skipped int
	// NotFound lists child identifiers that did not resolve to a Linear issue.
	NotFound []string
	// Errors collects per-link failures that did not abort the pass.
	Errors []error
}

// ReconcileMilestones sets Linear's projectMilestoneId on each child issue
// whose bead sits directly under a milestone epic (design 5.3, milestone
// assignment rule). A milestone is not an issue parent, so ReconcileParents
// cannot carry it.
//
// Idempotent: each child is fetched first and IssueUpdate is sent only when
// the current projectMilestone differs from the desired one. The pass only
// ever sets a milestone id; it never clears one and never creates or edits
// milestones themselves.
//
// When dryRun is true the fetches still run but no mutation is sent;
// WouldUpdate is incremented instead of Updated.
//
// Returns a non-nil error for setup failures and for a tripped rate-limit
// circuit breaker (the pass stops at once). Other per-link failures are
// collected in Stats.Errors.
func (t *Tracker) ReconcileMilestones(ctx context.Context, links []MilestoneLink, dryRun bool) (*MilestoneReconcileStats, error) {
	stats := &MilestoneReconcileStats{}
	if len(links) == 0 {
		return stats, nil
	}
	if t.primaryClient() == nil {
		return nil, errors.New("no Linear client available")
	}

	for _, link := range links {
		desired := nonEmptyMilestoneIDs(link.MilestoneIDs)
		if link.ChildIdentifier == "" || len(desired) == 0 {
			continue
		}

		child, client, err := t.fetchIssueAcrossTeams(ctx, link.ChildIdentifier)
		if err != nil {
			if isRateLimitExhausted(err) {
				return stats, fmt.Errorf("fetch child %s: %w", link.ChildIdentifier, err)
			}
			stats.Errors = append(stats.Errors,
				fmt.Errorf("fetch child %s: %w", link.ChildIdentifier, err))
			continue
		}
		if child == nil {
			stats.NotFound = append(stats.NotFound, link.ChildIdentifier)
			continue
		}

		current := ""
		if child.ProjectMilestone != nil {
			current = strings.TrimSpace(child.ProjectMilestone.ID)
		}
		if current != "" && milestoneIDsContain(desired, current) {
			stats.Skipped++
			continue
		}
		if len(desired) > 1 {
			stats.Errors = append(stats.Errors, fmt.Errorf(
				"%s has %d milestone-epic parents (%s) and its Linear milestone %q is none of them; not changing it",
				link.ChildIdentifier, len(desired), strings.Join(desired, ", "), current))
			continue
		}
		target := desired[0]

		if dryRun {
			stats.Mutations = append(stats.Mutations, MilestoneMutation{ChildIdentifier: link.ChildIdentifier, MilestoneID: target})
			stats.WouldUpdate++
			continue
		}

		if _, err := client.UpdateIssue(ctx, child.ID, map[string]interface{}{
			"projectMilestoneId": target,
		}); err != nil {
			if isRateLimitExhausted(err) {
				return stats, fmt.Errorf("set milestone of %s → %s: %w", link.ChildIdentifier, target, err)
			}
			stats.Errors = append(stats.Errors,
				fmt.Errorf("set milestone of %s → %s: %w", link.ChildIdentifier, target, err))
			continue
		}
		stats.Mutations = append(stats.Mutations, MilestoneMutation{ChildIdentifier: link.ChildIdentifier, MilestoneID: target})
		stats.Updated++
	}
	return stats, nil
}

func nonEmptyMilestoneIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !milestoneIDsContain(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func milestoneIDsContain(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
