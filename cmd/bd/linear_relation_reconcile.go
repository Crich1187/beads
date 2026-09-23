package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// reconcileLinearRelationsForStore is the post-push relation pass: it mirrors
// bead blocks dependencies into Linear blocks relations (create missing,
// delete ones this integration created that beads no longer has) via
// linear.Tracker.ReconcileRelations. It runs in the same slot and under the
// same scoping rule as reconcileLinearParentsForStore.
//
// Unlike the parent pass, which folds per-link failures into warnings, this
// pass is fail-closed: any fetch, mutation or ledger-write failure is
// returned so the sync exits non-zero and the runner stops the cycle.
// NotFound identifiers (bead has an external_ref but Linear has no such
// issue) are not failures; they are reported as warnings and retried next
// run.
func reconcileLinearRelationsForStore(ctx context.Context, st tracker.Store, lt *linear.Tracker, dryRun, jsonOutput bool, warnings *[]string) error {
	if lt == nil || st == nil {
		return nil
	}
	beads, refs, err := buildLinearBlockedBeadsForStore(ctx, st, lt)
	if err != nil {
		return fmt.Errorf("building relation set: %w", err)
	}
	if len(beads) == 0 {
		return nil
	}
	persist := func(ctx context.Context, beadID string, ledger []linear.OwnedRelation) error {
		return writeLinearRelationLedger(ctx, st, beadID, refs[beadID], ledger)
	}
	stats, err := lt.ReconcileRelations(ctx, beads, dryRun, persist)
	if stats != nil && !jsonOutput {
		if dryRun {
			if n := stats.WouldCreate + stats.WouldDelete; n > 0 {
				fmt.Printf("[dry-run] Would reconcile %d Linear blocking relation%s\n", n, plural(n))
				for _, m := range stats.Mutations {
					fmt.Printf("[dry-run] Would %s relation: %s blocks %s\n", m.Action, m.Blocker, m.Blocked)
				}
			}
		} else if n := stats.Created + stats.Deleted; n > 0 {
			fmt.Printf("✓ Reconciled %d Linear blocking relation%s (%d created, %d deleted)\n",
				n, plural(n), stats.Created, stats.Deleted)
		}
	}
	if stats != nil {
		for _, ident := range stats.NotFound {
			*warnings = append(*warnings, fmt.Sprintf("relation reconcile: Linear issue %s not found; its relations were left unchanged", ident))
		}
	}
	return err
}

// buildLinearBlockedBeadsForStore enumerates local beads with a Linear
// external_ref and returns, for each, the Linear identifiers of the beads
// that block it plus its current relation ledger. It also returns each
// bead's external_ref (keyed by bead ID) so the ledger writer can re-read the
// bead right before writing.
//
// Direction: bead A with a blocks dependency on bead B (A depends on B)
// means B blocks A. A is the BlockedBead; B is in its Blockers.
//
// Blockers not yet synced to Linear are skipped; they are picked up on a
// later sync once they have an external_ref. A bead is included when it has
// at least one Linear-synced blocker or a non-empty ledger (so removals run
// after its last blocks dependency is gone).
func buildLinearBlockedBeadsForStore(ctx context.Context, st tracker.Store, lt *linear.Tracker) ([]linear.BlockedBead, map[string]string, error) {
	if st == nil {
		return nil, nil, fmt.Errorf("database not available")
	}
	issues, err := st.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, nil, err
	}
	idToIdent := make(map[string]string, len(issues))
	refs := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue.ExternalRef == nil {
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
		idToIdent[issue.ID] = ident
		refs[issue.ID] = ref
	}
	if len(idToIdent) == 0 {
		return nil, nil, nil
	}
	var beads []linear.BlockedBead
	for _, issue := range issues {
		ident, ok := idToIdent[issue.ID]
		if !ok {
			continue
		}
		ledger, err := linearRelationLedger(issue.Metadata)
		if err != nil {
			return nil, nil, fmt.Errorf("reading relation ledger for %s: %w", issue.ID, err)
		}
		deps, err := st.GetDependenciesWithMetadata(ctx, issue.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("loading deps for %s: %w", issue.ID, err)
		}
		var blockers []string
		for _, d := range deps {
			if d == nil || d.DependencyType != types.DepBlocks {
				continue
			}
			// issue depends on d.Issue, so d.Issue blocks issue.
			blockerIdent, ok := idToIdent[d.Issue.ID]
			if !ok {
				// The dependency target is hydrated with its own row, so
				// a blocker the search did not return (whatever the
				// reason) still counts. Dropping it would make the pass
				// delete a relation whose dependency still exists.
				blockerIdent = linearIdentifierFromRef(lt, d.Issue.ExternalRef)
			}
			if blockerIdent != "" {
				blockers = append(blockers, blockerIdent)
			}
		}
		if len(blockers) == 0 && len(ledger) == 0 {
			continue
		}
		beads = append(beads, linear.BlockedBead{
			BeadID:     issue.ID,
			Identifier: ident,
			Blockers:   blockers,
			Ledger:     ledger,
		})
	}
	return beads, refs, nil
}

// linearIdentifierFromRef returns the Linear identifier for an external_ref,
// or "" when the ref is empty or not a Linear ref.
func linearIdentifierFromRef(lt *linear.Tracker, ref *string) string {
	if ref == nil {
		return ""
	}
	trimmed := strings.TrimSpace(*ref)
	if !lt.IsExternalRef(trimmed) {
		return ""
	}
	return lt.ExtractIdentifier(trimmed)
}

// linearRelationLedger reads the relation ledger at metadata linear.relations
// (nested under the "linear" object, alongside the milestone keys). Missing
// or null metadata yields an empty ledger; metadata that is not a JSON object,
// or a ledger of the wrong shape, is an error (fail closed rather than lose
// ownership records).
func linearRelationLedger(metadata json.RawMessage) ([]linear.OwnedRelation, error) {
	trimmed := strings.TrimSpace(string(metadata))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &data); err != nil {
		return nil, fmt.Errorf("metadata is not a JSON object: %w", err)
	}
	rawLinear, ok := data["linear"]
	if !ok || strings.TrimSpace(string(rawLinear)) == "null" {
		return nil, nil
	}
	var linearMeta map[string]json.RawMessage
	if err := json.Unmarshal(rawLinear, &linearMeta); err != nil {
		return nil, fmt.Errorf("metadata.linear is not a JSON object: %w", err)
	}
	rawLedger, ok := linearMeta["relations"]
	if !ok || strings.TrimSpace(string(rawLedger)) == "null" {
		return nil, nil
	}
	var ledger []linear.OwnedRelation
	if err := json.Unmarshal(rawLedger, &ledger); err != nil {
		return nil, fmt.Errorf("metadata.linear.relations is not a relation ledger: %w", err)
	}
	return ledger, nil
}

// mergeLinearRelationLedger returns existing metadata with linear.relations
// replaced by ledger (removed when ledger is empty). All other keys,
// including siblings inside the "linear" object, are preserved.
func mergeLinearRelationLedger(existing json.RawMessage, ledger []linear.OwnedRelation) (json.RawMessage, error) {
	data := make(map[string]json.RawMessage)
	if trimmed := strings.TrimSpace(string(existing)); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(existing, &data); err != nil {
			return nil, fmt.Errorf("metadata is not a JSON object: %w", err)
		}
	}
	linearMeta := make(map[string]json.RawMessage)
	if rawLinear, ok := data["linear"]; ok && strings.TrimSpace(string(rawLinear)) != "null" {
		if err := json.Unmarshal(rawLinear, &linearMeta); err != nil {
			return nil, fmt.Errorf("metadata.linear is not a JSON object: %w", err)
		}
	}
	if len(ledger) == 0 {
		delete(linearMeta, "relations")
	} else {
		raw, err := json.Marshal(ledger)
		if err != nil {
			return nil, fmt.Errorf("marshaling relation ledger: %w", err)
		}
		linearMeta["relations"] = raw
	}
	if len(linearMeta) == 0 {
		delete(data, "linear")
	} else {
		raw, err := json.Marshal(linearMeta)
		if err != nil {
			return nil, fmt.Errorf("marshaling linear metadata: %w", err)
		}
		data["linear"] = raw
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	return json.RawMessage(raw), nil
}

// writeLinearRelationLedger re-reads the bead right before writing (so keys
// written since the pass started are not clobbered), merges the ledger into
// its metadata and writes it back. A no-op when nothing changed.
func writeLinearRelationLedger(ctx context.Context, st tracker.Store, beadID, ref string, ledger []linear.OwnedRelation) error {
	if ref == "" {
		return fmt.Errorf("bead %s has no external_ref to re-read", beadID)
	}
	fresh, err := st.GetIssueByExternalRef(ctx, ref)
	if err != nil {
		return fmt.Errorf("re-reading bead %s: %w", beadID, err)
	}
	if fresh == nil || fresh.ID != beadID {
		return fmt.Errorf("re-reading bead %s by external_ref %s returned a different bead", beadID, ref)
	}
	merged, err := mergeLinearRelationLedger(fresh.Metadata, ledger)
	if err != nil {
		return err
	}
	if string(merged) == string(fresh.Metadata) {
		return nil
	}
	return st.UpdateIssue(ctx, beadID, map[string]interface{}{"metadata": merged}, actor)
}
