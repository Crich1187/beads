package linear

import (
	"context"
	"encoding/json"
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

// MilestoneAssignedMetadataKey is the key, inside the bead metadata "linear"
// object, of the one-shot marker the milestone pass writes after it has set
// a child's Linear milestone: metadata.linear.milestone_assigned = <id>.
const MilestoneAssignedMetadataKey = "milestone_assigned"

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

// MilestoneLink describes the desired initial Linear project milestone for
// one child issue.
//
// MilestoneIDs normally holds exactly one id: the milestone of the child's
// direct parent epic. It holds several only when the bead has more than one
// milestone-epic parent; the pass never picks between them.
//
// AssignedMarker and PulledMilestoneID are the one-shot guards read from the
// bead's metadata. Either one being non-empty means the milestone has
// already been decided (by this pass, or by Carl in Linear), so the pass
// leaves the issue alone for good.
type MilestoneLink struct {
	BeadID          string
	ExternalRef     string
	ChildIdentifier string
	MilestoneIDs    []string
	// AssignedMarker is metadata.linear.milestone_assigned: the pass already
	// set this child's milestone once.
	AssignedMarker string
	// PulledMilestoneID is metadata.linear.project_milestone.id, which pull
	// writes whenever the Linear issue has a milestone. Pull REPLACES the
	// whole bead metadata blob in that case (tracker engine, pull update and
	// reimport paths), which wipes AssignedMarker; this pulled value is the
	// evidence that survives, so it is treated as "already decided" too.
	PulledMilestoneID string
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
// touches a milestone Carl set by hand in Linear on such an issue.
//
// Any store error aborts the build before any Linear call is made. A child
// whose metadata cannot be parsed is not linked (its one-shot guard cannot
// be read) and is reported in the returned per-bead errors instead.
func BuildMilestoneLinks(ctx context.Context, st MilestoneLinkStore) ([]MilestoneLink, []error, error) {
	if st == nil {
		return nil, nil, errors.New("database not available")
	}
	issues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing local issues: %w", err)
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
		return nil, nil, nil
	}

	links := make([]MilestoneLink, 0)
	var perBead []error
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
			return nil, nil, fmt.Errorf("loading deps for %s: %w", issue.ID, err)
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
		marker, pulled, err := readMilestoneGuards(issue.Metadata)
		if err != nil {
			perBead = append(perBead, fmt.Errorf("%s (%s): cannot read milestone marker: %w", issue.ID, ident, err))
			continue
		}
		sort.Strings(ids)
		links = append(links, MilestoneLink{
			BeadID:            issue.ID,
			ExternalRef:       strings.TrimSpace(*issue.ExternalRef),
			ChildIdentifier:   ident,
			MilestoneIDs:      ids,
			AssignedMarker:    marker,
			PulledMilestoneID: pulled,
		})
	}
	return links, perBead, nil
}

// readMilestoneGuards extracts metadata.linear.milestone_assigned and
// metadata.linear.project_milestone.id. Malformed metadata is an error,
// never "no marker".
func readMilestoneGuards(raw json.RawMessage) (marker, pulled string, err error) {
	top, err := decodeMetadataObject(raw)
	if err != nil {
		return "", "", err
	}
	linearRaw, ok := top["linear"]
	if !ok {
		return "", "", nil
	}
	linearObj, err := decodeMetadataObject(linearRaw)
	if err != nil {
		return "", "", fmt.Errorf("metadata.linear: %w", err)
	}
	if v, ok := linearObj[MilestoneAssignedMetadataKey]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", "", fmt.Errorf("metadata.linear.%s is not a string: %w", MilestoneAssignedMetadataKey, err)
		}
		marker = strings.TrimSpace(s)
	}
	if v, ok := linearObj["project_milestone"]; ok {
		pm, err := decodeMetadataObject(v)
		if err != nil {
			return "", "", fmt.Errorf("metadata.linear.project_milestone: %w", err)
		}
		if idRaw, ok := pm["id"]; ok {
			var s string
			if err := json.Unmarshal(idRaw, &s); err != nil {
				return "", "", fmt.Errorf("metadata.linear.project_milestone.id is not a string: %w", err)
			}
			pulled = strings.TrimSpace(s)
		}
	}
	return marker, pulled, nil
}

// decodeMetadataObject parses a JSON object; empty or null means {}.
func decodeMetadataObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage)
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return out, nil
}

// MilestoneMarkerStore is the narrow store surface for writing the one-shot
// marker. tracker.Store satisfies it.
type MilestoneMarkerStore interface {
	GetIssueByExternalRef(context.Context, string) (*types.Issue, error)
	UpdateIssue(context.Context, string, map[string]interface{}, string) error
}

// WriteMilestoneAssignedMarker records metadata.linear.milestone_assigned =
// milestoneID on the bead. It re-reads the bead by external ref just before
// writing, merges only that one nested key (every other top-level key and
// every other key inside "linear" is preserved), and writes the full blob
// through UpdateIssue — the same path ensureLinearMilestoneEpic uses for
// milestone-epic metadata.
func WriteMilestoneAssignedMarker(ctx context.Context, st MilestoneMarkerStore, link MilestoneLink, milestoneID, actor string) error {
	if st == nil {
		return errors.New("database not available")
	}
	issue, err := st.GetIssueByExternalRef(ctx, link.ExternalRef)
	if err != nil {
		return fmt.Errorf("re-reading %s: %w", link.BeadID, err)
	}
	if issue == nil || issue.ID != link.BeadID {
		got := "<nil>"
		if issue != nil {
			got = issue.ID
		}
		return fmt.Errorf("re-reading %s by ref %s returned %s", link.BeadID, link.ExternalRef, got)
	}
	top, err := decodeMetadataObject(issue.Metadata)
	if err != nil {
		return fmt.Errorf("%s metadata: %w", link.BeadID, err)
	}
	linearObj := make(map[string]json.RawMessage)
	if raw, ok := top["linear"]; ok {
		if linearObj, err = decodeMetadataObject(raw); err != nil {
			return fmt.Errorf("%s metadata.linear: %w", link.BeadID, err)
		}
	}
	idJSON, err := json.Marshal(milestoneID)
	if err != nil {
		return err
	}
	linearObj[MilestoneAssignedMetadataKey] = idJSON
	linearJSON, err := json.Marshal(linearObj)
	if err != nil {
		return err
	}
	top["linear"] = linearJSON
	merged, err := json.Marshal(top)
	if err != nil {
		return err
	}
	if err := st.UpdateIssue(ctx, link.BeadID, map[string]interface{}{"metadata": json.RawMessage(merged)}, actor); err != nil {
		return fmt.Errorf("writing milestone marker on %s: %w", link.BeadID, err)
	}
	return nil
}

// MilestoneMutation records one projectMilestoneId assignment that was
// applied (wet-run) or would be applied (dry-run).
type MilestoneMutation struct {
	ChildIdentifier string
	MilestoneID     string
}

// MilestonePreserved records an informational skip: Linear already holds a
// milestone other than the parent epic's, so Carl's choice is kept.
type MilestonePreserved struct {
	ChildIdentifier string
	LinearMilestone string
	EpicMilestone   string
}

// MilestoneReconcileStats summarizes a ReconcileMilestones run.
type MilestoneReconcileStats struct {
	// Updated counts issues whose projectMilestoneId was set. Zero in dry-run.
	Updated int
	// WouldUpdate counts mutations the pass would issue. Dry-run only.
	WouldUpdate int
	// Mutations lists assignments applied (wet-run, appended only after the
	// API call succeeds) or planned (dry-run).
	Mutations []MilestoneMutation
	// Skipped counts links whose Linear milestone already equals the epic's.
	Skipped int
	// AlreadyAssigned counts links skipped without any Linear call because
	// the bead carries the one-shot marker or pulled milestone evidence.
	AlreadyAssigned int
	// Preserved lists informational skips where Linear holds a different
	// milestone (Carl moved it). Not an error and not a warning.
	Preserved []MilestonePreserved
	// NotFound lists child identifiers that did not resolve to a Linear issue.
	NotFound []string
	// Errors collects per-link failures that did not abort the pass.
	Errors []error
}

// MilestoneMarkFunc persists the one-shot marker for link after its Linear
// milestone was set. A returned error aborts the pass.
type MilestoneMarkFunc func(ctx context.Context, link MilestoneLink, milestoneID string) error

// ReconcileMilestones performs the ONE-SHOT initial milestone assignment of
// design 5.3: when an agent creates a child under a milestone epic, the
// child's Linear issue gets that epic's projectMilestoneId. Carl owns
// milestones afterwards (design 5.6), so the pass never undoes his moves or
// clears. Per link, in order:
//
//  1. Bead carries the marker or pulled milestone evidence: skip, no fetch.
//  2. Fetch the Linear issue (not found → NotFound).
//  3. Linear already holds one of the candidate milestones: skip.
//  4. Linear holds a different milestone: informational skip (Preserved).
//  5. Linear has none and there are several candidates: per-link error.
//  6. Otherwise set projectMilestoneId, then call mark to write the marker.
//
// It never clears a milestone and never creates or edits milestones. In
// dry-run nothing is written to Linear or to the store. A failed Linear
// update writes no marker; a failed marker write aborts the pass with an
// error (the Linear update it follows is still counted as applied).
// A tripped rate-limit circuit breaker also aborts the pass.
func (t *Tracker) ReconcileMilestones(ctx context.Context, links []MilestoneLink, mark MilestoneMarkFunc, dryRun bool) (*MilestoneReconcileStats, error) {
	stats := &MilestoneReconcileStats{}
	if len(links) == 0 {
		return stats, nil
	}
	if t.primaryClient() == nil {
		return nil, errors.New("no Linear client available")
	}
	if !dryRun && mark == nil {
		return nil, errors.New("no milestone marker writer configured")
	}

	for _, link := range links {
		desired := nonEmptyMilestoneIDs(link.MilestoneIDs)
		if link.ChildIdentifier == "" || len(desired) == 0 {
			continue
		}
		if strings.TrimSpace(link.AssignedMarker) != "" || strings.TrimSpace(link.PulledMilestoneID) != "" {
			stats.AlreadyAssigned++
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
		if current != "" {
			if milestoneIDsContain(desired, current) {
				stats.Skipped++
			} else {
				stats.Preserved = append(stats.Preserved, MilestonePreserved{
					ChildIdentifier: link.ChildIdentifier,
					LinearMilestone: current,
					EpicMilestone:   strings.Join(desired, ", "),
				})
			}
			continue
		}
		if len(desired) > 1 {
			stats.Errors = append(stats.Errors, fmt.Errorf(
				"%s has %d milestone-epic parents (%s) and no Linear milestone; not choosing one",
				link.ChildIdentifier, len(desired), strings.Join(desired, ", ")))
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

		if err := mark(ctx, link, target); err != nil {
			return stats, fmt.Errorf("milestone of %s set to %s in Linear but marker not recorded: %w",
				link.ChildIdentifier, target, err)
		}
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
