package linear

import (
	"context"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// Labels from acceptance test T09 (TEST-17 / Test-Sandbox-o8l).
var t09Labels = []string{"Barbados", "High", "Lymar Inc", "gate:review", "human", "owner:qa-swarm", "runtime:kimi", "task", "stage:build", "Governor"}

type labelMergeHarness struct {
	t        *testing.T
	fake     *fakeLinear
	store    *memTrackerStore
	lt       *Tracker
	threeWay bool
	beadID   string
	lastPull *tracker.SyncResult
}

func newLabelMergeHarness(t *testing.T, threeWay bool, linearLabels ...string) *labelMergeHarness {
	t.Helper()
	fake := newFakeLinear(t, t09Labels...)
	fake.addIssue("TEST-17", "label merge", "", linearLabels...)
	return &labelMergeHarness{t: t, fake: fake, store: newMemTrackerStore(), lt: fake.tracker(), threeWay: threeWay}
}

func (h *labelMergeHarness) engine() *tracker.Engine {
	e := tracker.NewEngine(h.lt, h.store, "linear-sync-runner")
	e.ThreeWayLabelMerge = h.threeWay
	return e
}

// cycle runs the runner's two invocations: `bd linear sync --pull`, then
// `bd linear sync --push`.
func (h *labelMergeHarness) cycle() {
	h.t.Helper()
	var err error
	if h.lastPull, err = h.engine().Sync(context.Background(), tracker.SyncOptions{Pull: true}); err != nil {
		h.t.Fatalf("pull: %v", err)
	}
	if _, err = h.engine().Sync(context.Background(), tracker.SyncOptions{Push: true}); err != nil {
		h.t.Fatalf("push: %v", err)
	}
	if h.beadID == "" {
		h.beadID = h.store.only(h.t).ID
	}
}

func (h *labelMergeHarness) beadLabels() []string {
	return sortedLabels(h.store.issue(h.beadID).Labels)
}

func (h *labelMergeHarness) agentLabels(labels ...string) {
	h.store.localEdit(h.beadID, func(i *types.Issue) { i.Labels = labels }, false)
}

func (h *labelMergeHarness) wantBoth(want ...string) {
	h.t.Helper()
	want = sortedLabels(want)
	if got := h.beadLabels(); !reflect.DeepEqual(got, want) {
		h.t.Fatalf("bead labels = %v, want %v", got, want)
	}
	if got := h.fake.labelNames("TEST-17"); !reflect.DeepEqual(got, want) {
		h.t.Fatalf("Linear labels = %v, want %v", got, want)
	}
}

var t09Base = []string{"Barbados", "High", "Lymar Inc", "gate:review", "human", "owner:qa-swarm", "runtime:kimi", "task"}

func with(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

func without(base []string, drop string) []string {
	var out []string
	for _, l := range base {
		if l != drop {
			out = append(out, l)
		}
	}
	return out
}

// T09b exactly: the agent adds stage:build (a bd label write, which does not
// move updated_at) and Carl adds Governor in Linear in the same window. Both
// labels must end up on the bead and on Linear.
func TestLabelMerge_T09bBothSidesAddedSurvive(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	h.wantBoth(t09Base...)

	h.agentLabels(with(t09Base, "stage:build")...)
	h.fake.setLabels("TEST-17", with(t09Base, "Governor")...)
	h.cycle()
	h.wantBoth(with(t09Base, "stage:build", "Governor")...)

	// Settled: another cycle changes nothing.
	before := h.fake.updateCount()
	h.cycle()
	if h.fake.updateCount() != before || h.lastPull.PullStats.Updated != 0 {
		t.Fatalf("settled cycle wrote: linear updates %d -> %d, pull updated %d", before, h.fake.updateCount(), h.lastPull.PullStats.Updated)
	}
}

// Same window, but the agent also edited a field (updated_at moved), which
// takes the pull's locally-edited branch: the description stays local and is
// pushed, and Carl's label is still merged in rather than overwritten.
func TestLabelMerge_LocallyEditedIssueStillMergesLinearLabels(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()

	h.store.localEdit(h.beadID, func(i *types.Issue) {
		i.Labels = with(t09Base, "stage:build")
		i.Description = "agent notes"
	}, true)
	h.fake.setLabels("TEST-17", with(t09Base, "Governor")...)
	h.cycle()
	h.wantBoth(with(t09Base, "stage:build", "Governor")...)
	if got := h.store.issue(h.beadID).Description; got != "agent notes" {
		t.Fatalf("bead description = %q, want the local edit kept", got)
	}
}

func TestLabelMerge_LinearOnlyChangeWins(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	// Carl adds one label and removes another; the bead is untouched.
	h.fake.setLabels("TEST-17", with(without(t09Base, "High"), "Governor")...)
	h.cycle()
	h.wantBoth(with(without(t09Base, "High"), "Governor")...)
}

func TestLabelMerge_LocalOnlyChangeSurvives(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	// The agent swaps its gate label; Linear is untouched. The pull must not
	// revert it, and the push delivers it.
	h.agentLabels(with(without(t09Base, "gate:review"), "stage:build")...)
	h.cycle()
	h.wantBoth(with(without(t09Base, "gate:review"), "stage:build")...)
}

func TestLabelMerge_RemovalOnEitherSideHonored(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	// Same window: the agent removes gate:review, Carl removes High.
	h.agentLabels(without(t09Base, "gate:review")...)
	h.fake.setLabels("TEST-17", without(t09Base, "High")...)
	h.cycle()
	h.wantBoth(without(without(t09Base, "gate:review"), "High")...)
}

// Carl removes a label the agent added in an earlier, already-pushed cycle:
// the push baseline makes that a Linear-side removal, not a local addition.
func TestLabelMerge_LinearRemovalOfPreviouslyPushedLocalLabel(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	h.agentLabels(with(t09Base, "stage:build")...)
	h.cycle()
	h.wantBoth(with(t09Base, "stage:build")...)
	h.fake.setLabels("TEST-17", t09Base...)
	h.cycle()
	h.wantBoth(t09Base...)
}

// Without a baseline (first sync after upgrade, fresh clone) the pull keeps
// the old behavior: Linear's labels win. Pinned so the merge never guesses.
func TestLabelMerge_NoBaselineKeepsLinearWins(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	for key := range h.store.localMetadata {
		if key != "linear.last_sync" {
			delete(h.store.localMetadata, key)
		}
	}
	h.agentLabels(with(t09Base, "stage:build")...)
	h.fake.setLabels("TEST-17", with(t09Base, "Governor")...)
	h.cycle()
	h.wantBoth(with(t09Base, "Governor")...)
}

// With the merge disabled the old defect is reproduced: the agent's label is
// dropped. This pins what ThreeWayLabelMerge changes.
func TestLabelMerge_DisabledReproducesF3(t *testing.T) {
	h := newLabelMergeHarness(t, false, t09Base...)
	h.cycle()
	h.agentLabels(with(t09Base, "stage:build")...)
	h.fake.setLabels("TEST-17", with(t09Base, "Governor")...)
	h.cycle()
	h.wantBoth(with(t09Base, "Governor")...)
}

// A baseline recorded for another Linear issue (the bead was relinked) is
// ignored.
func TestLabelMerge_BaselineBoundToTarget(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	key := "linear.labelbase." + h.beadID
	h.store.localMetadata[key] = `{"v":1,"target":"TEST-99","labels":["Barbados"]}`
	h.agentLabels(with(t09Base, "stage:build")...)
	h.fake.setLabels("TEST-17", with(t09Base, "Governor")...)
	h.cycle()
	// No usable baseline: Linear wins, exactly as with none recorded.
	h.wantBoth(with(t09Base, "Governor")...)
}

// A bidirectional sync (pull and push in one run) must not suppress the push
// of a bead whose merged labels still differ from Linear.
func TestLabelMerge_BidirectionalSyncPushesMergedLabels(t *testing.T) {
	h := newLabelMergeHarness(t, true, t09Base...)
	h.cycle()
	h.agentLabels(with(t09Base, "stage:build")...)
	h.fake.setLabels("TEST-17", with(t09Base, "Governor")...)
	if _, err := h.engine().Sync(context.Background(), tracker.SyncOptions{Pull: true, Push: true}); err != nil {
		t.Fatalf("bidirectional sync: %v", err)
	}
	h.wantBoth(with(t09Base, "stage:build", "Governor")...)
}
