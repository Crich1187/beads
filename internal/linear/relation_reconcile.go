package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// RelationTypeBlocks is the only Linear IssueRelationType the relation
// reconcile pass creates or deletes. Every other relation type (duplicate,
// related, similar, ...) is left untouched.
const RelationTypeBlocks = "blocks"

// OwnedRelation is one entry in a bead's relation ledger, stored in bead
// metadata under linear.relations. The ledger records every Linear relation
// this integration created, so the removal half of the pass only ever deletes
// relations it owns — never a relation a human (or another tool) added.
//
// Direction follows Linear's model: the relation's `issue` side is the
// blocker, its `relatedIssue` side is the blocked issue. The ledger lives on
// the blocked bead (the bead holding the blocks dependency).
type OwnedRelation struct {
	ID      string `json:"id"`      // Linear IssueRelation id
	Type    string `json:"type"`    // always RelationTypeBlocks today
	Blocker string `json:"blocker"` // Linear identifier of the issue side, e.g. "TEAM-2"
	Blocked string `json:"blocked"` // Linear identifier of the relatedIssue side, e.g. "TEAM-1"
}

// BlockedBead is the per-bead input to ReconcileRelations: the desired set of
// blockers (from the bead's blocks dependencies) plus the bead's current
// relation ledger.
//
// Beads semantics: bead A depends-on bead B with dependency type "blocks"
// means B blocks A. A is the BlockedBead; B's Linear identifier appears in
// Blockers. On the Linear side that is
// issueRelationCreate(issueId: B, relatedIssueId: A, type: blocks).
type BlockedBead struct {
	// BeadID is opaque to this package; it is handed back to the ledger
	// writer so the caller knows which bead's metadata to update.
	BeadID string
	// Identifier is the blocked bead's Linear identifier (e.g. "TEAM-1").
	Identifier string
	// Blockers are the Linear identifiers of the beads that block this one.
	Blockers []string
	// Ledger is the bead's current linear.relations ledger.
	Ledger []OwnedRelation
}

// RelationMutation describes one create or delete the pass issued (wet run)
// or would issue (dry run).
type RelationMutation struct {
	Action     string // "create" or "delete"
	Blocker    string // Linear identifier (issue side)
	Blocked    string // Linear identifier (relatedIssue side)
	RelationID string
}

// Relation mutation actions reported in RelationMutation.Action.
const (
	RelationActionCreate = "create"
	RelationActionDelete = "delete"
)

// RelationReconcileStats summarizes a ReconcileRelations run.
type RelationReconcileStats struct {
	// Created / Deleted count relations actually created / deleted in Linear
	// (wet run only).
	Created int
	Deleted int
	// WouldCreate / WouldDelete are the dry-run counterparts.
	WouldCreate int
	WouldDelete int
	// Skipped counts desired links already satisfied by an existing Linear
	// blocks relation (owned or not) — no mutation issued.
	Skipped int
	// Released counts ledger entries dropped without any Linear mutation
	// because the relation no longer exists in Linear, or a human changed
	// its type or endpoints (so it is no longer ours to manage).
	Released int
	// Mutations lists every create/delete applied (wet run, appended only
	// after the API call succeeded) or planned (dry run).
	Mutations []RelationMutation
	// NotFound lists Linear identifiers that did not resolve to an issue.
	// Links touching them are left alone (ledger entries kept) and retried
	// on the next run.
	NotFound []string
}

// RelationLedgerWriter persists a bead's updated ledger. ReconcileRelations
// calls it (wet run only) whenever the ledger changes: once before issuing
// creates (so an interrupted run still owns what it created) and again after
// deletes succeed. An error aborts the pass.
type RelationLedgerWriter func(ctx context.Context, beadID string, ledger []OwnedRelation) error

// relationIssue is an issue with its outgoing relation connection. Only the
// fields the relation pass needs are fetched.
type relationIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Relations  struct {
		Nodes    []Relation `json:"nodes"`
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"relations"`
}

// findRelation returns the outgoing relation with the given id, or nil.
func (ri *relationIssue) findRelation(id string) *Relation {
	for i := range ri.Relations.Nodes {
		if ri.Relations.Nodes[i].ID == id {
			return &ri.Relations.Nodes[i]
		}
	}
	return nil
}

// blocks reports whether ri has an outgoing blocks relation to relatedID.
func (ri *relationIssue) blocks(relatedID string) bool {
	for _, rel := range ri.Relations.Nodes {
		if rel.Type == RelationTypeBlocks && rel.RelatedIssue.ID == relatedID {
			return true
		}
	}
	return false
}

// blockedBy reports whether ri carries a legacy "blockedBy" relation to
// blockerID. The Linear API enum has no blockedBy (a UI "blocked by" is
// stored as a blocks relation on the blocker), but the pull mapping already
// honours it defensively, so the push side treats it as satisfying the link
// too rather than creating a second, duplicate relation.
func (ri *relationIssue) blockedBy(blockerID string) bool {
	for _, rel := range ri.Relations.Nodes {
		if rel.Type == "blockedBy" && rel.RelatedIssue.ID == blockerID {
			return true
		}
	}
	return false
}

const issueRelationsByIdentifierQuery = `
	query IssueRelationsByIdentifier($filter: IssueFilter!) {
		issues(filter: $filter, first: 1) {
			nodes {
				id
				identifier
				relations {
					nodes {
						id
						type
						relatedIssue {
							id
							identifier
						}
					}
					pageInfo {
						hasNextPage
					}
				}
			}
		}
	}
`

// fetchIssueRelations returns the issue with the given identifier in this
// client's team, including its outgoing relations. Returns (nil, nil) when
// the issue does not exist in the team. Fails closed when the relation
// connection has more than one page: a truncated view would make an existing
// relation look missing and the pass would create a duplicate.
func (c *Client) fetchIssueRelations(ctx context.Context, identifier string) (*relationIssue, error) {
	filter := map[string]interface{}{
		"team": map[string]interface{}{
			"id": map[string]interface{}{"eq": c.TeamID},
		},
	}
	parts := strings.Split(identifier, "-")
	if len(parts) >= 2 {
		if number, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			filter["number"] = map[string]interface{}{"eq": number}
		}
	}
	data, err := c.Execute(ctx, &GraphQLRequest{
		Query:     issueRelationsByIdentifierQuery,
		Variables: map[string]interface{}{"filter": filter},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch relations for %s: %w", identifier, err)
	}
	var resp struct {
		Issues struct {
			Nodes []relationIssue `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse relations response for %s: %w", identifier, err)
	}
	for i := range resp.Issues.Nodes {
		issue := resp.Issues.Nodes[i]
		if issue.Identifier != identifier {
			continue
		}
		if issue.Relations.PageInfo.HasNextPage {
			return nil, fmt.Errorf("issue %s has more relations than one page; refusing to reconcile a truncated view", identifier)
		}
		return &issue, nil
	}
	return nil, nil
}

// createBlocksRelation issues issueRelationCreate for "blockerID blocks
// blockedID" with a client-supplied relation id, and returns the id Linear
// reports for the new relation.
func (c *Client) createBlocksRelation(ctx context.Context, relationID, blockerID, blockedID string) (string, error) {
	query := `
		mutation IssueRelationCreate($input: IssueRelationCreateInput!) {
			issueRelationCreate(input: $input) {
				success
				issueRelation {
					id
					type
				}
			}
		}
	`
	data, err := c.Execute(ctx, &GraphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"input": map[string]interface{}{
				"id":             relationID,
				"issueId":        blockerID,
				"relatedIssueId": blockedID,
				"type":           RelationTypeBlocks,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create relation: %w", err)
	}
	var resp struct {
		IssueRelationCreate struct {
			Success       bool `json:"success"`
			IssueRelation struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"issueRelation"`
		} `json:"issueRelationCreate"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("failed to parse relation create response: %w", err)
	}
	if !resp.IssueRelationCreate.Success {
		return "", errors.New("relation create reported as unsuccessful")
	}
	if resp.IssueRelationCreate.IssueRelation.ID == "" {
		return "", errors.New("relation create returned no relation id")
	}
	return resp.IssueRelationCreate.IssueRelation.ID, nil
}

// deleteRelation issues issueRelationDelete for the given relation id.
func (c *Client) deleteRelation(ctx context.Context, relationID string) error {
	query := `
		mutation IssueRelationDelete($id: String!) {
			issueRelationDelete(id: $id) {
				success
			}
		}
	`
	data, err := c.Execute(ctx, &GraphQLRequest{
		Query:     query,
		Variables: map[string]interface{}{"id": relationID},
	})
	if err != nil {
		return fmt.Errorf("failed to delete relation: %w", err)
	}
	var resp struct {
		IssueRelationDelete struct {
			Success bool `json:"success"`
		} `json:"issueRelationDelete"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("failed to parse relation delete response: %w", err)
	}
	if !resp.IssueRelationDelete.Success {
		return errors.New("relation delete reported as unsuccessful")
	}
	return nil
}

// fetchIssueRelationsAcrossTeams mirrors fetchIssueAcrossTeams for the
// relation query, but fails closed: in a multi-team setup a probe error on
// any team is returned when no team produced the issue, because treating an
// errored probe as "not found" could hide an existing relation.
func (t *Tracker) fetchIssueRelationsAcrossTeams(ctx context.Context, identifier string) (*relationIssue, *Client, error) {
	if identifier == "" {
		return nil, nil, nil
	}
	if len(t.teamIDs) <= 1 {
		client := t.primaryClient()
		if client == nil {
			return nil, nil, errors.New("no Linear client available")
		}
		ri, err := client.fetchIssueRelations(ctx, identifier)
		if err != nil {
			return nil, nil, err
		}
		if ri == nil {
			return nil, nil, nil
		}
		return ri, client, nil
	}
	var probeErrs []error
	for _, teamID := range t.teamIDs {
		client := t.clients[teamID]
		if client == nil {
			continue
		}
		ri, err := client.fetchIssueRelations(ctx, identifier)
		if err != nil {
			if isRateLimitExhausted(err) {
				return nil, nil, err
			}
			probeErrs = append(probeErrs, err)
			continue
		}
		if ri != nil {
			return ri, client, nil
		}
	}
	if len(probeErrs) > 0 {
		return nil, nil, errors.Join(probeErrs...)
	}
	return nil, nil, nil
}

// ReconcileRelations mirrors bead blocks dependencies into Linear blocks
// relations. It is the relation counterpart of ReconcileParents and runs in
// the same post-push slot.
//
// For each blocked bead it compares the desired blockers with the blockers'
// current outgoing Linear relations (fetch-before-create) and:
//
//   - creates a blocks relation (issueId = blocker, relatedIssueId = blocked)
//     for every desired link with no existing blocks relation, recording the
//     new relation id in the bead's ledger;
//   - deletes every ledgered relation whose blocker is no longer desired;
//   - never creates a second relation when one already satisfies the link
//     (owned or not), and never deletes a relation that is not in the ledger,
//     has a type other than blocks, or points somewhere else.
//
// Idempotent: a rerun against unchanged beads and Linear issues makes zero
// mutations and zero ledger writes.
//
// Fail closed: the first fetch, mutation or ledger-write error aborts the
// pass and is returned. Ledger entries for relations created before the
// failure are already persisted (the ledger is written before creates), so
// a rerun neither duplicates nor orphans them.
//
// In dry-run mode the read-only fetches still run; creates and deletes are
// only counted (WouldCreate / WouldDelete) and the ledger is never written.
func (t *Tracker) ReconcileRelations(ctx context.Context, beads []BlockedBead, dryRun bool, persist RelationLedgerWriter) (*RelationReconcileStats, error) {
	stats := &RelationReconcileStats{}
	if len(beads) == 0 {
		return stats, nil
	}
	if t.primaryClient() == nil {
		return nil, errors.New("no Linear client available")
	}
	if !dryRun && persist == nil {
		return nil, errors.New("relation reconcile: no ledger writer")
	}

	f := &relationFetcher{t: t, ctx: ctx, cache: make(map[string]relationEntry), stats: stats}
	for _, bead := range beads {
		if err := t.reconcileBeadRelations(ctx, bead, dryRun, persist, stats, f); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (t *Tracker) reconcileBeadRelations(
	ctx context.Context,
	bead BlockedBead,
	dryRun bool,
	persist RelationLedgerWriter,
	stats *RelationReconcileStats,
	f *relationFetcher,
) error {
	fetch := f.get
	notFound := f.notFound
	blockedIdent := strings.TrimSpace(bead.Identifier)
	if blockedIdent == "" {
		return nil
	}

	desired := make(map[string]bool, len(bead.Blockers))
	for _, b := range bead.Blockers {
		b = strings.TrimSpace(b)
		if b == "" || b == blockedIdent {
			continue
		}
		desired[b] = true
	}
	if len(desired) == 0 && len(bead.Ledger) == 0 {
		return nil
	}

	blockedE, err := fetch(blockedIdent, false)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", blockedIdent, err)
	}
	if blockedE.issue == nil {
		notFound(blockedIdent)
		return nil
	}
	blocked := blockedE.issue

	// Group ledger entries by blocker. Malformed entries (no id or blocker)
	// can never be acted on; they are dropped.
	owned := make(map[string][]OwnedRelation)
	ledgerChanged := false
	for _, e := range bead.Ledger {
		if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Blocker) == "" {
			ledgerChanged = true
			stats.Released++
			continue
		}
		owned[e.Blocker] = append(owned[e.Blocker], e)
	}

	blockers := make([]string, 0, len(desired)+len(owned))
	for b := range desired {
		blockers = append(blockers, b)
	}
	for b := range owned {
		if !desired[b] {
			blockers = append(blockers, b)
		}
	}
	sort.Strings(blockers)

	type pendingCreate struct {
		blocker    string
		blockerID  string
		client     *Client
		relationID string
	}
	type pendingDelete struct {
		blocker string
		entry   OwnedRelation
		client  *Client
	}
	var (
		ledger  []OwnedRelation
		creates []pendingCreate
		deletes []pendingDelete
	)

	for _, blockerIdent := range blockers {
		entries := owned[blockerIdent]
		be, err := fetch(blockerIdent, false)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", blockerIdent, err)
		}
		if be.issue == nil {
			// Can't see the blocker: keep its ledger entries untouched and
			// retry next run. Nothing is created or deleted for it.
			notFound(blockerIdent)
			ledger = append(ledger, entries...)
			continue
		}
		blocker := be.issue

		// Classify ledger entries against what Linear currently holds.
		var live []OwnedRelation
		for _, e := range entries {
			rel := blocker.findRelation(e.ID)
			switch {
			case rel == nil:
				// Gone from Linear (deleted by hand, or a create that never
				// landed). Nothing to delete; drop the entry.
				ledgerChanged = true
				stats.Released++
			case rel.Type != RelationTypeBlocks || rel.RelatedIssue.ID != blocked.ID:
				// A human changed its type or endpoints: no longer ours.
				ledgerChanged = true
				stats.Released++
			default:
				live = append(live, e)
			}
		}

		if desired[blockerIdent] {
			ledger = append(ledger, live...)
			if blocker.blocks(blocked.ID) || blocked.blockedBy(blocker.ID) {
				stats.Skipped++
				continue
			}
			if dryRun {
				stats.WouldCreate++
				stats.Mutations = append(stats.Mutations, RelationMutation{
					Action: RelationActionCreate, Blocker: blockerIdent, Blocked: blockedIdent,
				})
				continue
			}
			relationID := uuid.NewString()
			ledger = append(ledger, OwnedRelation{
				ID: relationID, Type: RelationTypeBlocks, Blocker: blockerIdent, Blocked: blockedIdent,
			})
			ledgerChanged = true
			creates = append(creates, pendingCreate{
				blocker: blockerIdent, blockerID: blocker.ID, client: be.client, relationID: relationID,
			})
			continue
		}

		// Blocker no longer desired: delete every live relation we own.
		for _, e := range live {
			if dryRun {
				stats.WouldDelete++
				stats.Mutations = append(stats.Mutations, RelationMutation{
					Action: RelationActionDelete, Blocker: blockerIdent, Blocked: blockedIdent, RelationID: e.ID,
				})
				continue
			}
			// Stays in the ledger until the delete succeeds.
			ledger = append(ledger, e)
			deletes = append(deletes, pendingDelete{blocker: blockerIdent, entry: e, client: be.client})
		}
	}

	if dryRun {
		return nil
	}

	// Write the ledger (including ids for relations about to be created)
	// before any create, so a crash or error mid-pass never leaves a
	// relation we created without an ownership record.
	if ledgerChanged {
		if err := persist(ctx, bead.BeadID, append([]OwnedRelation(nil), ledger...)); err != nil {
			return fmt.Errorf("write relation ledger for %s: %w", bead.BeadID, err)
		}
		ledgerChanged = false
	}

	for _, c := range creates {
		gotID, err := c.client.createBlocksRelation(ctx, c.relationID, c.blockerID, blocked.ID)
		if err != nil {
			// Ambiguous failure: the create may have landed (e.g. a transport
			// error after Linear committed). Look for our exact id before
			// giving up.
			if recovered, rerr := fetch(c.blocker, true); rerr == nil && recovered.issue != nil {
				if rel := recovered.issue.findRelation(c.relationID); rel != nil && rel.Type == RelationTypeBlocks && rel.RelatedIssue.ID == blocked.ID {
					err = nil
					gotID = c.relationID
				}
			}
			if err != nil {
				return fmt.Errorf("create relation %s blocks %s: %w", c.blocker, blockedIdent, err)
			}
		}
		if gotID != c.relationID {
			// Linear assigned its own id; re-point the ledger entry.
			for i := range ledger {
				if ledger[i].ID == c.relationID {
					ledger[i].ID = gotID
				}
			}
			ledgerChanged = true
		}
		// Keep the cache honest for later beads sharing this blocker.
		if cached, ferr := fetch(c.blocker, false); ferr == nil && cached.issue != nil && cached.issue.findRelation(gotID) == nil {
			rel := Relation{ID: gotID, Type: RelationTypeBlocks}
			rel.RelatedIssue.ID = blocked.ID
			rel.RelatedIssue.Identifier = blockedIdent
			cached.issue.Relations.Nodes = append(cached.issue.Relations.Nodes, rel)
		}
		stats.Created++
		stats.Mutations = append(stats.Mutations, RelationMutation{
			Action: RelationActionCreate, Blocker: c.blocker, Blocked: blockedIdent, RelationID: gotID,
		})
	}

	var deleteErr error
	for _, d := range deletes {
		if err := d.client.deleteRelation(ctx, d.entry.ID); err != nil {
			deleteErr = fmt.Errorf("delete relation %s (%s blocks %s): %w", d.entry.ID, d.blocker, blockedIdent, err)
			break
		}
		ledger = removeOwnedRelation(ledger, d.entry.ID)
		ledgerChanged = true
		if cached, ferr := fetch(d.blocker, false); ferr == nil && cached.issue != nil {
			nodes := cached.issue.Relations.Nodes[:0]
			for _, rel := range cached.issue.Relations.Nodes {
				if rel.ID != d.entry.ID {
					nodes = append(nodes, rel)
				}
			}
			cached.issue.Relations.Nodes = nodes
		}
		stats.Deleted++
		stats.Mutations = append(stats.Mutations, RelationMutation{
			Action: RelationActionDelete, Blocker: d.blocker, Blocked: blockedIdent, RelationID: d.entry.ID,
		})
	}

	if ledgerChanged {
		if err := persist(ctx, bead.BeadID, append([]OwnedRelation(nil), ledger...)); err != nil {
			if deleteErr != nil {
				return errors.Join(deleteErr, fmt.Errorf("write relation ledger for %s: %w", bead.BeadID, err))
			}
			return fmt.Errorf("write relation ledger for %s: %w", bead.BeadID, err)
		}
	}
	return deleteErr
}

// relationEntry is a cached fetch result: the issue with its outgoing
// relations, plus the team client that owns it (reused for mutations so they
// go to the same team the issue was found in, as in ReconcileParents).
type relationEntry struct {
	issue  *relationIssue
	client *Client
}

// relationFetcher caches per-identifier fetches for one ReconcileRelations
// run. A blocker shared by many beads is fetched once.
type relationFetcher struct {
	t     *Tracker
	ctx   context.Context
	cache map[string]relationEntry
	stats *RelationReconcileStats
}

func (f *relationFetcher) get(identifier string, refresh bool) (relationEntry, error) {
	if cached, ok := f.cache[identifier]; ok && !refresh {
		return cached, nil
	}
	issue, client, err := f.t.fetchIssueRelationsAcrossTeams(f.ctx, identifier)
	if err != nil {
		return relationEntry{}, err
	}
	e := relationEntry{issue: issue, client: client}
	f.cache[identifier] = e
	return e, nil
}

func (f *relationFetcher) notFound(identifier string) {
	for _, seen := range f.stats.NotFound {
		if seen == identifier {
			return
		}
	}
	f.stats.NotFound = append(f.stats.NotFound, identifier)
}

func removeOwnedRelation(ledger []OwnedRelation, id string) []OwnedRelation {
	out := ledger[:0]
	for _, e := range ledger {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return out
}
