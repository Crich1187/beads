package scopedbundle

import (
	"strings"
	"testing"
)

// targetStateWith builds a destination State holding the given comment and event
// identities, so the manifest's enumeration guarantee can be exercised directly.
// Every synthetic destination row lives on issue "target-001", matching the
// fixture bundle's linked source comment on "source-001".
func targetStateWith(digest string, commentIDs, eventIDs []string) State {
	mk := func(name string, ids []string) Table {
		t := Table{Name: name, Columns: []Column{
			{Name: "id", SQLType: "varchar(64)"},
			{Name: "issue_id", SQLType: "varchar(64)"},
		}}
		for _, id := range ids {
			t.Rows = append(t.Rows, Row{Cells: []Cell{{Text: id}, {Text: "target-001"}}})
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
	// Give the bundle the source rows the link-integrity checks resolve against:
	// comment "s-shared" and event "e-src-1", both on issue "source-001" (which
	// the fixture mapping sends to "target-001").
	for i := range b.Tables {
		switch b.Tables[i].Name {
		case "comments":
			b.Tables[i].Rows = []Row{{Cells: []Cell{{Text: "s-shared"}, {Text: "source-001"}}}}
		case "events":
			b.Tables[i].Rows = []Row{{Cells: []Cell{{Text: "e-src-1"}, {Text: "source-001"}}}}
		}
	}
	if err := b.Seal(); err != nil {
		t.Fatalf("seal bundle: %v", err)
	}
	target := Schema{Version: 66, Tables: map[string][]Column{}}
	for _, table := range b.Tables {
		target.Tables[table.Name] = table.Columns
	}
	return b, target, b.SourceStateSHA256, strings.Repeat("d", 64)
}

// reconcileFixtureWithEvents is reconcileFixture with a caller-chosen bundle
// event identity set, for exercising the RetainSourceEventIDs enumeration rule.
func reconcileFixtureWithEvents(t *testing.T, eventIDs []string) (Bundle, Schema, string, string) {
	t.Helper()
	b := minimalBundle(t)
	for i := range b.Tables {
		if b.Tables[i].Name != "events" {
			continue
		}
		b.Tables[i].Rows = nil
		for _, id := range eventIDs {
			b.Tables[i].Rows = append(b.Tables[i].Rows, Row{Cells: []Cell{{Text: id}, {Text: "source-001"}}})
		}
	}
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

// Gate 4 r1 Finding 1: a link whose SourceID names no bundle comment used to be
// accepted, and execution then deleted the destination comment while writing
// nothing in its place. The plan must refuse it before any write.
func TestPlanReconcileRejectsPhantomLinkSource(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)
	state := targetStateWith(tgtDigest, []string{"c-shared"}, nil)

	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.ExpectedSourceSHA256 = srcDigest
		m.ExpectedTargetSHA256 = tgtDigest
		m.CommentLinks = []CommentLink{{TargetID: "c-shared", SourceID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}}
	})

	_, err := PlanReconcile(b, state, schema, m)
	if err == nil || !strings.Contains(err.Error(), `comment link source "ffffffff-ffff-ffff-ffff-ffffffffffff" does not exist in the bundle`) {
		t.Fatalf("error = %v, want phantom link source refusal", err)
	}
}

// A link target that the bundle itself also supplies as a comment identity
// would be deleted and then resurrected by the union — refuse at plan time.
func TestPlanReconcileRejectsLinkTargetSuppliedByBundle(t *testing.T) {
	b, schema, _, tgtDigest := reconcileFixture(t)
	// A second, real source comment so the collision check (not the phantom
	// check) is what fires.
	for i := range b.Tables {
		if b.Tables[i].Name == "comments" {
			b.Tables[i].Rows = append(b.Tables[i].Rows, Row{Cells: []Cell{{Text: "s-other"}, {Text: "source-001"}}})
		}
	}
	if err := b.Seal(); err != nil {
		t.Fatalf("reseal bundle: %v", err)
	}
	srcDigest := b.SourceStateSHA256
	// Destination already carries the source identity "s-shared".
	state := targetStateWith(tgtDigest, []string{"s-shared"}, nil)

	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.ExpectedSourceSHA256 = srcDigest
		m.ExpectedTargetSHA256 = tgtDigest
		// Not a self-link (validateShape refuses those); the target identity
		// collides with a bundle comment id via a different source.
		m.CommentLinks = []CommentLink{{TargetID: "s-shared", SourceID: "s-other"}}
	})

	_, err := PlanReconcile(b, state, schema, m)
	if err == nil || !strings.Contains(err.Error(), "also a bundle comment identity") {
		t.Fatalf("error = %v, want link target collision refusal", err)
	}
}

func TestManifestRejectsCommentOnBothSidesOfLinkSet(t *testing.T) {
	m := ReconcileManifest{
		Format:               ReconcileManifestFormat,
		Version:              ReconcileManifestVersion,
		ExpectedSourceSHA256: strings.Repeat("a", 64),
		ExpectedTargetSHA256: strings.Repeat("b", 64),
		CommentLinks: []CommentLink{
			{TargetID: "c-1", SourceID: "shared-id"},
			{TargetID: "shared-id", SourceID: "s-2"},
		},
	}
	if err := m.Seal(); err == nil || !strings.Contains(err.Error(), "both a link source and a link target") {
		t.Fatalf("error = %v, want cross-side identity refusal", err)
	}

	self := ReconcileManifest{
		Format:               ReconcileManifestFormat,
		Version:              ReconcileManifestVersion,
		ExpectedSourceSHA256: strings.Repeat("a", 64),
		ExpectedTargetSHA256: strings.Repeat("b", 64),
		CommentLinks:         []CommentLink{{TargetID: "same-id", SourceID: "same-id"}},
	}
	if err := self.Seal(); err == nil || !strings.Contains(err.Error(), "both a link source and a link target") {
		t.Fatalf("error = %v, want self-link refusal", err)
	}
}

// A link may only equate comments on corresponding issues; otherwise history
// silently migrates to a different issue.
func TestPlanReconcileRejectsCrossIssueLink(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)
	// Destination comment lives on target-Referee-030, but the source comment
	// "s-shared" maps to target-001.
	state := targetStateWith(tgtDigest, nil, nil)
	for i := range state.Tables {
		if state.Tables[i].Name == "comments" {
			state.Tables[i].Rows = []Row{{Cells: []Cell{{Text: "c-elsewhere"}, {Text: "target-Referee-030"}}}}
		}
	}

	m := sealedManifest(t, func(m *ReconcileManifest) {
		m.ExpectedSourceSHA256 = srcDigest
		m.ExpectedTargetSHA256 = tgtDigest
		m.CommentLinks = []CommentLink{{TargetID: "c-elsewhere", SourceID: "s-shared"}}
	})

	_, err := PlanReconcile(b, state, schema, m)
	if err == nil || !strings.Contains(err.Error(), "crosses issues") {
		t.Fatalf("error = %v, want cross-issue link refusal", err)
	}
}

// RetainSourceEventIDs is a reviewed enumeration, not a filter: the executor
// retains every mapped source event. A non-empty list must therefore name every
// bundle event and nothing else, so a reviewer can never believe an omission
// excluded an event.
func TestPlanReconcileRejectsInconsistentRetainSourceEventIDs(t *testing.T) {
	b, schema, srcDigest, tgtDigest := reconcileFixture(t)
	state := targetStateWith(tgtDigest, nil, nil)

	t.Run("unknown event id", func(t *testing.T) {
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = srcDigest
			m.ExpectedTargetSHA256 = tgtDigest
			m.RetainSourceEventIDs = []string{"e-src-1", "e-GHOST"}
		})
		_, err := PlanReconcile(b, state, schema, m)
		if err == nil || !strings.Contains(err.Error(), `names "e-GHOST" which is not a bundle event`) {
			t.Fatalf("error = %v, want unknown source event refusal", err)
		}
	})

	t.Run("partial list", func(t *testing.T) {
		bPartial, schemaPartial, srcPartial, tgtPartial := reconcileFixtureWithEvents(t, []string{"e-src-1", "e-src-2"})
		m := sealedManifest(t, func(m *ReconcileManifest) {
			m.ExpectedSourceSHA256 = srcPartial
			m.ExpectedTargetSHA256 = tgtPartial
			m.RetainSourceEventIDs = []string{"e-src-1"}
		})
		_, err := PlanReconcile(bPartial, targetStateWith(tgtPartial, nil, nil), schemaPartial, m)
		if err == nil || !strings.Contains(err.Error(), "lists 1 of 2 bundle events") {
			t.Fatalf("error = %v, want partial enumeration refusal", err)
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

// The executor postcondition mirrors the plan-time phantom-source guard: if a
// link's surviving source identity is absent after the union, the transaction
// must fail rather than report success over deleted destination history.
func TestVerifyUnionFailsWhenLinkedSourceCommentMissing(t *testing.T) {
	manifest := ReconcileManifest{
		CommentLinks: []CommentLink{{TargetID: "c-gone", SourceID: "s-phantom"}},
	}
	desired := targetStateWith("", nil, nil).Tables
	post := targetStateWith("", nil, nil) // neither c-gone nor s-phantom present

	_, err := verifyUnion(State{}, desired, post, manifest)
	if err == nil || !strings.Contains(err.Error(), `linked source comment "s-phantom" is missing after reconcile`) {
		t.Fatalf("error = %v, want missing linked source postcondition failure", err)
	}
}

// Retained counts report observed destination-only survivors, not the manifest
// list length: an identity the source also supplies is a source row.
func TestVerifyUnionCountsOnlyDestinationOnlySurvivors(t *testing.T) {
	manifest := ReconcileManifest{
		RetainTargetCommentIDs: []string{"c-dest-only", "c-also-in-source"},
		RetainTargetEventIDs:   []string{"e-dest-only", "e-also-in-source"},
	}
	desired := targetStateWith("", []string{"c-also-in-source"}, []string{"e-also-in-source"}).Tables
	post := targetStateWith("",
		[]string{"c-dest-only", "c-also-in-source"},
		[]string{"e-dest-only", "e-also-in-source"})

	counts, err := verifyUnion(State{}, desired, post, manifest)
	if err != nil {
		t.Fatalf("verifyUnion: %v", err)
	}
	if counts.retainedComments != 1 || counts.retainedEvents != 1 {
		t.Fatalf("retained = %d comments %d events, want 1 and 1", counts.retainedComments, counts.retainedEvents)
	}
}
