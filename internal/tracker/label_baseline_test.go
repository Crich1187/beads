package tracker

import (
	"context"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

func TestThreeWayMergeLabels(t *testing.T) {
	base := []string{"a", "b", "c"}
	for _, tc := range []struct {
		name          string
		local, remote []string
		want          []string
	}{
		{name: "no change", local: base, remote: base, want: base},
		{name: "both sides added different labels: both survive", local: []string{"a", "b", "c", "agent"}, remote: []string{"a", "b", "c", "carl"}, want: []string{"a", "agent", "b", "c", "carl"}},
		{name: "both sides added the same label", local: []string{"a", "b", "c", "x"}, remote: []string{"a", "b", "c", "x"}, want: []string{"a", "b", "c", "x"}},
		{name: "remote-only add and remove win", local: base, remote: []string{"a", "c", "carl"}, want: []string{"a", "c", "carl"}},
		{name: "local-only add and remove survive", local: []string{"a", "c", "agent"}, remote: base, want: []string{"a", "agent", "c"}},
		{name: "removal on each side honored", local: []string{"a", "b"}, remote: []string{"a", "c"}, want: []string{"a"}},
		{name: "both removed the same label", local: []string{"a", "b"}, remote: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "remote removed everything", local: base, remote: []string{}, want: []string{}},
		{name: "whitespace and duplicates normalized", local: []string{" a", "b", "b", "c "}, remote: base, want: base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := threeWayMergeLabels(base, tc.local, tc.remote)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("merge(base=%v, local=%v, remote=%v) = %v, want %v", base, tc.local, tc.remote, got, tc.want)
			}
		})
	}
}

// Nil tracker labels mean "unknown", not "every label removed": no merge, no
// baseline write.
func TestMergePulledLabelsIgnoresUnknownRemoteLabels(t *testing.T) {
	ctx := context.Background()
	store := newPureTestStore()
	e := NewEngine(newMockTracker("linear"), store, "sync")
	e.ThreeWayLabelMerge = true
	e.recordLabelBaseline(ctx, "bd-1", "EXT-1", []string{"a", "b"})
	existing := &types.Issue{ID: "bd-1", Labels: []string{"a", "b", "agent"}}

	if got, ok := e.mergePulledLabels(ctx, existing, nil, "EXT-1"); ok || got != nil {
		t.Fatalf("mergePulledLabels(nil remote) = %v, %v; want no merge", got, ok)
	}
	before := store.localMetadata["linear.labelbase.bd-1"]
	e.recordLabelBaseline(ctx, "bd-1", "EXT-1", nil)
	if after := store.localMetadata["linear.labelbase.bd-1"]; after != before {
		t.Fatalf("nil labels overwrote the baseline: %q -> %q", before, after)
	}
	if got, ok := e.mergePulledLabels(ctx, existing, []string{"a", "b"}, "EXT-1"); !ok || !reflect.DeepEqual(got, []string{"a", "agent", "b"}) {
		t.Fatalf("mergePulledLabels = %v, %v; want the local addition kept", got, ok)
	}
}

// The baseline is written only when it changes.
func TestRecordLabelBaselineWritesOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	store := &countingMetadataStore{pureTestStore: newPureTestStore()}
	e := NewEngine(newMockTracker("linear"), store, "sync")
	e.ThreeWayLabelMerge = true
	for i := 0; i < 3; i++ {
		e.recordLabelBaseline(ctx, "bd-1", "EXT-1", []string{"b", "a"})
	}
	e.recordLabelBaseline(ctx, "bd-1", "EXT-1", []string{"a", "b", "c"})
	if store.writes != 2 {
		t.Fatalf("baseline writes = %d, want 2 (first record, then the change)", store.writes)
	}
}

// Disabled, nothing is recorded or merged.
func TestLabelBaselineDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	store := newPureTestStore()
	e := NewEngine(newMockTracker("linear"), store, "sync")
	e.recordLabelBaseline(ctx, "bd-1", "EXT-1", []string{"a"})
	if len(store.localMetadata) != 0 {
		t.Fatalf("baseline recorded with ThreeWayLabelMerge off: %v", store.localMetadata)
	}
	if _, ok := e.mergePulledLabels(ctx, &types.Issue{ID: "bd-1"}, []string{"a"}, "EXT-1"); ok {
		t.Fatal("merge ran with ThreeWayLabelMerge off")
	}
}

type countingMetadataStore struct {
	*pureTestStore
	writes int
}

func (s *countingMetadataStore) SetLocalMetadata(ctx context.Context, key, value string) error {
	s.writes++
	return s.pureTestStore.SetLocalMetadata(ctx, key, value)
}
