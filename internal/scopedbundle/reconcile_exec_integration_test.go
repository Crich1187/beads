//go:build scopedbundle_integration

package scopedbundle

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// These tests run against a real private Dolt server. They cover the executor
// added for root-55fr9.13.21.10 and, critically, prove that adding it does NOT
// weaken `apply`: the same destination history that Reconcile is allowed to
// retain under a sealed manifest is still rejected outright by Apply.

const (
	destOnlyComment = "d0000000-0000-0000-0000-0000000000c1"
	linkedComment   = "d0000000-0000-0000-0000-0000000000c2"
	destOnlyEvent   = "d0000000-0000-0000-0000-0000000000e1"
)

// seedDestinationHistory adds destination-only comment/event rows plus a comment
// that the manifest will declare equivalent to a source comment.
func seedDestinationHistory(t *testing.T, db *sql.DB, issueID string) {
	t.Helper()
	when := "2026-09-05 13:00:00"
	stmts := []struct {
		q string
		a []any
	}{
		{"INSERT INTO comments (id,issue_id,author,text,created_at) VALUES (?,?,?,?,?)",
			[]any{destOnlyComment, issueID, "mac-author", "destination-only history", when}},
		{"INSERT INTO comments (id,issue_id,author,text,created_at) VALUES (?,?,?,?,?)",
			[]any{linkedComment, issueID, "mac-author", "exact comment", when}},
		{"INSERT INTO events (id,issue_id,event_type,actor,old_value,new_value,comment,created_at) VALUES (?,?,?,?,?,?,?,?)",
			[]any{destOnlyEvent, issueID, "updated", "mac-actor", "o", "n", "destination-only event", when}},
	}
	for _, s := range stmts {
		if _, err := db.Exec(s.q, s.a...); err != nil {
			t.Fatalf("seed destination history: %v", err)
		}
	}
}

// reconcileScenario builds a source and a destination that already carries
// history the source does not have — the shape of the real migration.
func reconcileScenario(t *testing.T, name string) (*sql.DB, *sql.DB, Mapping, Bundle) {
	t.Helper()
	ctx := context.Background()
	source := newPrivateDatabase(t, "recon_src_"+name, 53)
	target := newPrivateDatabase(t, "recon_tgt_"+name, 53)
	mapping := syntheticResearchMapping()
	seedSyntheticSource(t, source, mapping)
	seedUnrelatedControl(t, target, "unrelated-newer")

	bundle, err := Export(ctx, source, mapping)
	if err != nil {
		t.Fatal(err)
	}
	// Land the source once so the destination has the mapped issues, then give
	// the destination its own extra history.
	empty, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, target, *bundle, ApplyOptions{ExpectedCurrentSHA256: empty.SHA256}); err != nil {
		t.Fatal(err)
	}
	seedDestinationHistory(t, target, mapping.Pairs[0].Target)
	return source, target, mapping, *bundle
}

func manifestFor(t *testing.T, bundle Bundle, target State, mutate func(*ReconcileManifest)) ReconcileManifest {
	t.Helper()
	m := ReconcileManifest{
		Format:               ReconcileManifestFormat,
		Version:              ReconcileManifestVersion,
		ExpectedSourceSHA256: bundle.SourceStateSHA256,
		ExpectedTargetSHA256: target.SHA256,
	}
	// Enumerate every destination comment/event that must survive.
	comments, _ := findTable(target.Tables, "comments")
	events, _ := findTable(target.Tables, "events")
	for _, row := range comments.Rows {
		id, err := rowID(comments, row)
		if err != nil {
			t.Fatal(err)
		}
		if id == linkedComment {
			m.CommentLinks = append(m.CommentLinks, CommentLink{TargetID: id, SourceID: "00000000-0000-0000-0000-000000000001"})
			continue
		}
		m.RetainTargetCommentIDs = append(m.RetainTargetCommentIDs, id)
	}
	for _, row := range events.Rows {
		id, err := rowID(events, row)
		if err != nil {
			t.Fatal(err)
		}
		m.RetainTargetEventIDs = append(m.RetainTargetEventIDs, id)
	}
	if mutate != nil {
		mutate(&m)
	}
	if err := m.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return m
}

func TestReconcileBuildsUnionPreservingDestinationHistory(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "union")

	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFor(t, bundle, before, nil)

	result, err := Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: before.SHA256,
		Actor:                 "claude:wS:p4",
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !result.Changed {
		t.Error("reconcile reported no change")
	}
	if result.RemovedLinkedComments != 1 {
		t.Errorf("removed linked comments = %d, want 1", result.RemovedLinkedComments)
	}

	after, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	comments, _ := findTable(after.Tables, "comments")
	events, _ := findTable(after.Tables, "events")
	ids := idSet(comments)

	// The destination-only comment survived.
	if _, ok := ids[destOnlyComment]; !ok {
		t.Error("destination-only comment was lost — the union did not preserve destination history")
	}
	// The linked duplicate is gone; the source comment represents it.
	if _, ok := ids[linkedComment]; ok {
		t.Error("linked destination comment survived and duplicates the source comment")
	}
	if _, ok := ids["00000000-0000-0000-0000-000000000001"]; !ok {
		t.Error("source comment is missing after reconcile")
	}
	// Both event sets survived.
	eventIDs := idSet(events)
	for _, want := range []string{destOnlyEvent, "00000000-0000-0000-0000-000000000003"} {
		if _, ok := eventIDs[want]; !ok {
			t.Errorf("event %s missing from the union", want)
		}
	}
	assertUnrelatedControl(t, target, "unrelated-newer")
}

// THE CENTRAL GUARANTEE. The very same destination history that Reconcile
// retains under a sealed manifest must still be rejected by Apply, with nothing
// written. If this test ever passes an Apply, the executor has weakened apply.
func TestApplyStillRejectsDestinationHistoryThatReconcileCanRetain(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "notweakened")

	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	fullBefore := fullFiveTableDigest(t, target)

	_, err = Apply(ctx, target, bundle, ApplyOptions{ExpectedCurrentSHA256: before.SHA256})
	if err == nil {
		t.Fatal("APPLY ACCEPTED UNENUMERATED DESTINATION HISTORY — apply has been weakened")
	}
	if !strings.Contains(err.Error(), "destination-only") && !strings.Contains(err.Error(), "unrepresentable") {
		t.Fatalf("apply error = %v, want a destination-only rejection", err)
	}
	if got := fullFiveTableDigest(t, target); got != fullBefore {
		t.Fatalf("apply rejection changed database rows: %s != %s", got, fullBefore)
	}
}

func TestReconcileRejectsUnenumeratedDestinationRowWithoutWrites(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "unlisted")

	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the destination-only comment from the manifest: it becomes unlisted.
	manifest := manifestFor(t, bundle, before, func(m *ReconcileManifest) {
		kept := m.RetainTargetCommentIDs[:0]
		for _, id := range m.RetainTargetCommentIDs {
			if id != destOnlyComment {
				kept = append(kept, id)
			}
		}
		m.RetainTargetCommentIDs = kept
	})
	fullBefore := fullFiveTableDigest(t, target)

	_, err = Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: before.SHA256, Actor: "claude:wS:p4",
	})
	if err == nil || !strings.Contains(err.Error(), "not listed in the reviewed manifest") {
		t.Fatalf("error = %v, want refusal of the unlisted destination comment", err)
	}
	if got := fullFiveTableDigest(t, target); got != fullBefore {
		t.Fatalf("refusal changed database rows: %s != %s", got, fullBefore)
	}
}

func TestReconcileRejectsStaleExpectedDigestWithoutWrites(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "drift")

	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFor(t, bundle, before, nil)
	fullBefore := fullFiveTableDigest(t, target)

	_, err = Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: strings.Repeat("a", 64), Actor: "claude:wS:p4",
	})
	if err == nil || !strings.Contains(err.Error(), "expected current SHA-256") {
		t.Fatalf("error = %v, want stale-digest refusal", err)
	}
	if got := fullFiveTableDigest(t, target); got != fullBefore {
		t.Fatalf("refusal changed database rows: %s != %s", got, fullBefore)
	}
}

func TestReconcileRejectsTamperedManifestWithoutWrites(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "tamper")

	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFor(t, bundle, before, nil)
	manifest.RetainTargetEventIDs = append(manifest.RetainTargetEventIDs, "smuggled-after-seal")
	fullBefore := fullFiveTableDigest(t, target)

	_, err = Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: before.SHA256, Actor: "claude:wS:p4",
	})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want the seal to reject post-review tampering", err)
	}
	if got := fullFiveTableDigest(t, target); got != fullBefore {
		t.Fatalf("refusal changed database rows: %s != %s", got, fullBefore)
	}
}

func TestReconcileRequiresExpectedCurrentAndActor(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "args")
	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFor(t, bundle, before, nil)

	if _, err := Reconcile(ctx, target, bundle, manifest, ReconcileOptions{}); err == nil ||
		!strings.Contains(err.Error(), "expected current SHA-256 is required") {
		t.Fatalf("error = %v, want missing-digest refusal", err)
	}
	if _, err := Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: before.SHA256, JournalEnabled: true,
	}); err == nil || !strings.Contains(err.Error(), "actor is required") {
		t.Fatalf("error = %v, want missing-actor refusal", err)
	}

}

// A rerun against the already-reconciled state must be refused on the stale
// digest rather than silently double-applying.
func TestReconcileRerunIsRefusedOnStaleDigest(t *testing.T) {
	ctx := context.Background()
	_, target, mapping, bundle := reconcileScenario(t, "rerun")
	before, err := Inspect(ctx, target, mapping, TargetSide)
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFor(t, bundle, before, nil)
	if _, err := Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: before.SHA256, Actor: "claude:wS:p4",
	}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	afterFirst := fullFiveTableDigest(t, target)

	if _, err := Reconcile(ctx, target, bundle, manifest, ReconcileOptions{
		ExpectedCurrentSHA256: before.SHA256, Actor: "claude:wS:p4",
	}); err == nil {
		t.Fatal("rerun with the stale pre-state digest was accepted")
	}
	if got := fullFiveTableDigest(t, target); got != afterFirst {
		t.Fatalf("refused rerun changed rows: %s != %s", got, afterFirst)
	}
}
