package scopedbundle

import (
	"strings"
	"testing"
)

// targetStateWith builds a destination State holding the given comment and event
// identities, so the manifest's enumeration guarantee can be exercised directly.
func targetStateWith(digest string, commentIDs, eventIDs []string) State {
	mk := func(name string, ids []string) Table {
		t := Table{Name: name, Columns: []Column{{Name: "id", SQLType: "varchar(64)"}}}
		for _, id := range ids {
			t.Rows = append(t.Rows, Row{Cells: []Cell{{Text: id}}})
		}
		return t
	}
	return State{
		Schema: Schema{Version: 66},
		Tables: []Table{mk("comments", commentIDs), mk("events", eventIDs)},
		SHA256: digest,
	}
}

// reconcileFixture returns a bundle whose columns are all present in the target,
// so these tests exercise the comment/event union rather than schema additions
// (schema additions have their own tests in reconcile_test.go).
func reconcileFixture(t *testing.T) (Bundle, Schema, string, string) {
	t.Helper()
	b := minimalBundle(t)
	if err := b.Seal(); err != nil {
		t.Fatalf("seal bundle: %v", err)
	}
	target := Schema{Version: 66, Tables: map[string][]Column{}}
	for _, table := range b.Tables {
		target.Tables[table.Name] = table.Columns
	}
	return b, target, b.SourceStateSHA256, strings.Repeat("d", 64)
}

func TestPlanReconcileBuildsTheReviewedUnion(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)
	state := targetStateWith(tgtDigest, []string{"c-shared", "c-dest-only"}, []string{"e-dest-1", "e-dest-2"})

	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.ExpectedSourceSHA256 = srcDigest
		m.ExpectedTargetSHA256 = tgtDigest
		m.CommentLinks = []CommentLink{{TargetID: "c-shared", SourceID: "s-shared"}}
		m.RetainTargetCommentIDs = []string{"c-dest-only"}
		m.RetainTargetEventIDs = []string{"e-dest-1", "e-dest-2"}
		m.RetainSourceEventIDs = []string{"e-src-1"}
	})

	plan, err := PlanReconcile(b, state, schema, m)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.LinkedComments) != 1 || plan.LinkedComments[0].SourceID != "s-shared" {
		t.Errorf("linked comments = %v", plan.LinkedComments)
	}
	if len(plan.RetainedTargetOnly) != 1 || plan.RetainedTargetOnly[0] != "c-dest-only" {
		t.Errorf("destination-only comments = %v", plan.RetainedTargetOnly)
	}
	// Both sides of the event union survive.
	if len(plan.RetainedTargetEvent) != 2 || len(plan.RetainedSourceEvent) != 1 {
		t.Errorf("event union = target %v source %v", plan.RetainedTargetEvent, plan.RetainedSourceEvent)
	}
}

// The central guarantee: reconciliation does not weaken apply. A destination row
// the operator did not enumerate is still fatal.
func TestPlanReconcileRejectsUnlistedDestinationRows(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)

	t.Run("unlisted comment", func(t *testing.T) {
		state := targetStateWith(tgtDigest, []string{"c-listed", "c-SMUGGLED"}, nil)
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = srcDigest
			m.ExpectedTargetSHA256 = tgtDigest
			m.RetainTargetCommentIDs = []string{"c-listed"}
		})
		_, err := PlanReconcile(b, state, schema, m)
		if err == nil || !strings.Contains(err.Error(), `comments row "c-SMUGGLED" is not listed`) {
			t.Fatalf("error = %v, want rejection of the unlisted comment", err)
		}
	})

	t.Run("unlisted event", func(t *testing.T) {
		state := targetStateWith(tgtDigest, nil, []string{"e-SMUGGLED"})
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = srcDigest
			m.ExpectedTargetSHA256 = tgtDigest
		})
		_, err := PlanReconcile(b, state, schema, m)
		if err == nil || !strings.Contains(err.Error(), `events row "e-SMUGGLED" is not listed`) {
			t.Fatalf("error = %v, want rejection of the unlisted event", err)
		}
	})

	t.Run("manifest names a row that is absent", func(t *testing.T) {
		state := targetStateWith(tgtDigest, nil, nil)
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = srcDigest
			m.ExpectedTargetSHA256 = tgtDigest
			m.RetainTargetCommentIDs = []string{"c-ghost"}
		})
		_, err := PlanReconcile(b, state, schema, m)
		if err == nil || !strings.Contains(err.Error(), "absent from the destination") {
			t.Fatalf("error = %v, want rejection of a phantom identity", err)
		}
	})
}

func TestPlanReconcileRejectsDigestDrift(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)
	state := targetStateWith(tgtDigest, nil, nil)

	t.Run("source drift", func(t *testing.T) {
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = strings.Repeat("f", 64) // not the bundle's
			m.ExpectedTargetSHA256 = tgtDigest
		})
		_, err := PlanReconcile(b, state, schema, m)
		if err == nil || !strings.Contains(err.Error(), "source state digest mismatch") {
			t.Fatalf("error = %v, want source digest refusal", err)
		}
	})

	t.Run("target drift", func(t *testing.T) {
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = srcDigest
			m.ExpectedTargetSHA256 = strings.Repeat("e", 64) // not the live target
		})
		_, err := PlanReconcile(b, state, schema, m)
		if err == nil || !strings.Contains(err.Error(), "target state digest mismatch") {
			t.Fatalf("error = %v, want target digest refusal", err)
		}
	})
}

func TestPlanReconcileRejectsTamperedManifest(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)
	state := targetStateWith(tgtDigest, []string{"c1"}, nil)
	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.ExpectedSourceSHA256 = srcDigest
		m.ExpectedTargetSHA256 = tgtDigest
		m.RetainTargetCommentIDs = []string{"c1"}
	})
	// Smuggle an extra retained identity in after sealing.
	m.RetainTargetCommentIDs = append(m.RetainTargetCommentIDs, "c-smuggled")
	if _, err := PlanReconcile(b, state, schema, m); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want the seal to reject post-review tampering", err)
	}
}
