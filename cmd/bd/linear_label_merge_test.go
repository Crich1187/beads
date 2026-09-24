package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/tracker"
)

// bd linear sync must run the engine with the three-way label merge (finding
// F3); the merge itself is tested in internal/tracker and internal/linear.
func TestLinearEngineEnablesThreeWayLabelMerge(t *testing.T) {
	engine := tracker.NewEngine(nil, nil, "test")
	configureLinearEngine(engine)
	if !engine.ThreeWayLabelMerge {
		t.Fatal("bd linear sync must run with ThreeWayLabelMerge enabled")
	}
}
