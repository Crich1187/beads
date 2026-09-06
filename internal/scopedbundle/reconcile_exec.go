package scopedbundle

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage/issueops"
)

// Reconcile executes the sealed union that `apply` deliberately refuses to
// perform.
//
// `apply` rejects every destination-only or content-colliding comment/event row
// before mutating. That default is correct and is left exactly as it is — this
// file adds no path through `apply` and changes none of its checks. Reconcile is
// a separate entry point that performs the union ONLY under a sealed, reviewed
// manifest which enumerates every destination row that is to survive. A
// destination row the operator did not enumerate is as fatal here as in `apply`.
//
// Union semantics, all driven by the sealed manifest:
//
//   - issues, labels, dependencies — replaced from the mapped bundle, exactly as
//     `apply` does.
//   - comments — the mapped source comments, PLUS destination-only comments named
//     by RetainTargetCommentIDs. A destination comment named by a CommentLink is
//     the same comment as a source comment after deterministic reference
//     rewriting, so its destination row is DELETED and the content survives once,
//     under the source identity. That is what stops the union double-counting.
//   - events — the mapped source events PLUS every destination event named by
//     RetainTargetEventIDs. Event identity is not reconstructable, so both sets
//     are kept and nothing is deleted.
//
// Everything happens in one transaction: on any error the whole reconcile rolls
// back, including the schema additions.

// ReconcileOptions mirrors ApplyOptions so the two entry points are operationally
// interchangeable from the caller's point of view.
type ReconcileOptions struct {
	// ExpectedCurrentSHA256 is the exact scoped target digest the operator
	// reviewed. A mismatch aborts before any write, exactly as in Apply.
	ExpectedCurrentSHA256 string
	Actor                 string
	JournalEnabled        bool
}

// ReconcileResult reports what the union did, for the operator's record.
type ReconcileResult struct {
	BeforeSHA256     string
	AfterSHA256      string
	Changed          bool
	SchemaStatements []string
	// SourceComments is the number of mapped source comments written;
	// RetainedDestinationComments counts destination-only comments kept;
	// RemovedLinkedComments counts destination duplicates removed.
	SourceComments              int
	RetainedDestinationComments int
	RemovedLinkedComments       int
	SourceEvents                int
	RetainedDestinationEvents   int
}

// Reconcile applies the sealed union. It writes nothing unless every guard passes.
func Reconcile(ctx context.Context, db *sql.DB, bundle Bundle, manifest ReconcileManifest, options ReconcileOptions) (result ReconcileResult, err error) {
	if strings.TrimSpace(options.ExpectedCurrentSHA256) == "" {
		return ReconcileResult{}, fmt.Errorf("expected current SHA-256 is required")
	}
	if options.JournalEnabled && strings.TrimSpace(options.Actor) == "" {
		return ReconcileResult{}, fmt.Errorf("actor is required when the events journal is enabled")
	}
	if err := bundle.Verify(); err != nil {
		return ReconcileResult{}, fmt.Errorf("verify bundle: %w", err)
	}
	if err := manifest.Verify(); err != nil {
		return ReconcileResult{}, fmt.Errorf("verify reconcile manifest: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("begin scoped reconcile: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	schemaBefore, err := InspectSchema(ctx, tx)
	if err != nil {
		return ReconcileResult{}, err
	}
	current, err := inspectWithSchema(ctx, tx, bundle.Mapping, TargetSide, schemaBefore)
	if err != nil {
		return ReconcileResult{}, err
	}
	result.BeforeSHA256 = current.SHA256

	// Same drift guard as Apply: the target must be exactly what was reviewed.
	if current.SHA256 != options.ExpectedCurrentSHA256 {
		return ReconcileResult{}, fmt.Errorf("expected current SHA-256 %s but found %s", options.ExpectedCurrentSHA256, current.SHA256)
	}

	// PlanReconcile re-checks both sealed digests and refuses any destination
	// comment/event row the manifest did not enumerate, plus any enumerated
	// identity that is not actually present.
	plan, err := PlanReconcile(bundle, current, schemaBefore, manifest)
	if err != nil {
		return ReconcileResult{}, err
	}
	result.SchemaStatements = plan.SchemaStatements

	if err := ApplySchemaAdditions(ctx, tx, plan.SchemaStatements); err != nil {
		return ReconcileResult{}, err
	}

	// The schema changed inside this transaction, so re-read it before shaping
	// the desired rows; otherwise the new columns would be invisible.
	schemaAfter, err := InspectSchema(ctx, tx)
	if err != nil {
		return ReconcileResult{}, err
	}
	desired, err := materializeDesired(bundle, schemaAfter)
	if err != nil {
		return ReconcileResult{}, err
	}

	if err := validateGlobalIDCollisions(ctx, tx, desired); err != nil {
		return ReconcileResult{}, err
	}

	// Remove destination comment rows that the manifest declares are the same
	// comment as a source comment. Their content survives under the source
	// identity; leaving them would duplicate the comment in the union.
	linked := make([]string, 0, len(plan.LinkedComments))
	for _, link := range plan.LinkedComments {
		linked = append(linked, link.TargetID)
	}
	sort.Strings(linked)
	if len(linked) > 0 {
		holes := strings.TrimSuffix(strings.Repeat("?,", len(linked)), ",")
		//nolint:gosec // G202: the table/column SQL is constant; only the count of bound placeholders varies, and every value is a bound parameter.
		if _, err := tx.ExecContext(ctx, "DELETE FROM `comments` WHERE `id` IN ("+holes+")", stringsToAny(linked)...); err != nil {
			return ReconcileResult{}, fmt.Errorf("remove linked destination comments: %w", err)
		}
	}
	result.RemovedLinkedComments = len(linked)

	// reconcileTables replaces issues/labels/dependencies and inserts
	// comments/events that are not already present. It never deletes a
	// destination comment or event, which is precisely why the retained
	// destination history survives the union.
	delta := computeDelta(current.Tables, desired)
	cleanupJournal := issueops.ScopeEventsJournalTransaction(tx, options.JournalEnabled)
	defer cleanupJournal()
	if err := reconcileTables(ctx, tx, bundle.Mapping, current.Tables, desired); err != nil {
		return ReconcileResult{}, err
	}
	if err := recordJournalDelta(ctx, tx, delta, desired, ApplyOptions{
		ExpectedCurrentSHA256: options.ExpectedCurrentSHA256,
		Actor:                 options.Actor,
		JournalEnabled:        options.JournalEnabled,
	}); err != nil {
		return ReconcileResult{}, err
	}

	// Postcondition: verify the union is exactly what the manifest described.
	post, err := inspectWithSchema(ctx, tx, bundle.Mapping, TargetSide, schemaAfter)
	if err != nil {
		return ReconcileResult{}, err
	}
	counts, err := verifyUnion(current, desired, post, manifest)
	if err != nil {
		return ReconcileResult{}, err
	}
	result.SourceComments = counts.sourceComments
	result.RetainedDestinationComments = counts.retainedComments
	result.SourceEvents = counts.sourceEvents
	result.RetainedDestinationEvents = counts.retainedEvents
	result.AfterSHA256 = post.SHA256
	result.Changed = post.SHA256 != current.SHA256

	if err := tx.Commit(); err != nil {
		return ReconcileResult{}, fmt.Errorf("commit scoped reconcile: %w", err)
	}
	return result, nil
}

type unionCounts struct {
	sourceComments   int
	retainedComments int
	sourceEvents     int
	retainedEvents   int
}

// verifyUnion is the postcondition. It proves, against the freshly re-read state,
// that every source row landed, every enumerated destination row survived, and
// every linked destination duplicate is gone — and that nothing else appeared.
func verifyUnion(current State, desired []Table, post State, manifest ReconcileManifest) (unionCounts, error) {
	var counts unionCounts

	desiredComments, _ := findTable(desired, "comments")
	desiredEvents, _ := findTable(desired, "events")
	postComments, _ := findTable(post.Tables, "comments")
	postEvents, _ := findTable(post.Tables, "events")

	counts.sourceComments = len(desiredComments.Rows)
	counts.sourceEvents = len(desiredEvents.Rows)
	counts.retainedComments = len(manifest.RetainTargetCommentIDs)
	counts.retainedEvents = len(manifest.RetainTargetEventIDs)

	postCommentIDs := idSet(postComments)
	postEventIDs := idSet(postEvents)

	for _, row := range desiredComments.Rows {
		id, err := rowID(desiredComments, row)
		if err != nil {
			return counts, err
		}
		if _, ok := postCommentIDs[id]; !ok {
			return counts, fmt.Errorf("postcondition: source comment %q is missing after reconcile", id)
		}
	}
	for _, row := range desiredEvents.Rows {
		id, err := rowID(desiredEvents, row)
		if err != nil {
			return counts, err
		}
		if _, ok := postEventIDs[id]; !ok {
			return counts, fmt.Errorf("postcondition: source event %q is missing after reconcile", id)
		}
	}
	for _, id := range manifest.RetainTargetCommentIDs {
		if _, ok := postCommentIDs[id]; !ok {
			return counts, fmt.Errorf("postcondition: retained destination comment %q was lost", id)
		}
	}
	for _, id := range manifest.RetainTargetEventIDs {
		if _, ok := postEventIDs[id]; !ok {
			return counts, fmt.Errorf("postcondition: retained destination event %q was lost", id)
		}
	}
	for _, link := range manifest.CommentLinks {
		if _, ok := postCommentIDs[link.TargetID]; ok {
			return counts, fmt.Errorf("postcondition: linked destination comment %q survived and would duplicate source %q", link.TargetID, link.SourceID)
		}
	}

	// Exact cardinality: nothing may appear that the manifest did not describe.
	// This is a SET union, not a sum: a destination row the manifest retains may
	// legitimately be a row the source also supplies under the same identity
	// (it was landed by an earlier apply), and counting it twice would make a
	// correct union look wrong.
	wantComments := unionSize(desiredComments, manifest.RetainTargetCommentIDs)
	if len(postComments.Rows) != wantComments {
		return counts, fmt.Errorf("postcondition: comment count %d does not match the reviewed union %d", len(postComments.Rows), wantComments)
	}
	wantEvents := unionSize(desiredEvents, manifest.RetainTargetEventIDs)
	if len(postEvents.Rows) != wantEvents {
		return counts, fmt.Errorf("postcondition: event count %d does not match the reviewed union %d", len(postEvents.Rows), wantEvents)
	}
	return counts, nil
}

func idSet(table Table) map[string]struct{} {
	out := make(map[string]struct{}, len(table.Rows))
	for _, row := range table.Rows {
		if id, err := rowID(table, row); err == nil {
			out[id] = struct{}{}
		}
	}
	return out
}

// unionSize is the cardinality of {desired ids} united with {retained ids}.
func unionSize(desired Table, retained []string) int {
	all := idSet(desired)
	for _, id := range retained {
		all[id] = struct{}{}
	}
	return len(all)
}
