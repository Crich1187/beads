package tracker

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// Three-way label merge on pull.
//
// Why: a pull used to replace a bead's labels with the tracker's set whenever
// the bead was not locally edited since last_sync. bd's label writes never
// touch issues.updated_at, so a label an agent added after the last sync is
// invisible to that guard, and the pull silently removed it when the tracker
// issue had changed too (acceptance run 1, finding F3: the agent's
// stage:build was dropped while Carl's Governor survived).
//
// Baseline: per issue, the label set bd last knew the tracker to hold, stored
// in local_metadata under "<prefix>.labelbase.<issue-id>" as
// {"v":1,"target":"<tracker identifier>","labels":[...sorted...]}. It is
// recorded
//   - on pull, for every existing or created issue whose labels the tracker
//     returned: the tracker's labels as pulled;
//   - on push, for every issue created, updated, or skipped because the
//     tracker already matched: the bead's labels as pushed.
// local_metadata is Dolt-ignored (like the push hash and last_sync), so the
// baseline is per clone or per sql-server; a fresh clone simply has none.
//
// Merge (base B, local L, tracker R): a label ends up on the bead when both
// sides have it, or when one side added it since B. So additions and removals
// on either side since the last sync are honored, and when only the tracker
// changed, the tracker wins. Without a baseline (first sync after upgrade, a
// fresh clone, or a relinked bead whose baseline names another target) the
// pull keeps the previous behavior: the tracker's labels win.
//
// When the tracker returned no label information (nil labels) nothing is
// merged or recorded: nil means "unknown", not "Linear removed every label".
//
// Title, description and assignee are deliberately not merged this way. Edits
// to them go through UpdateIssue and move updated_at, so the pull guard
// already sees them: a bead edited since the last sync keeps its fields and
// the push sends them (local wins), and a field changed only in the tracker
// is applied by the ordinary pull (tracker wins). The remaining gap is both
// sides changing different fields in the same window, where the push
// overwrites the tracker-side field. A per-field baseline would not close it
// safely for descriptions: after a push the tracker's text differs from the
// bead's (structured sections, idempotency marker, Linear's markdown
// re-serialization), so "only the tracker changed" would read true after
// every push and a guarded pull would rewrite the agent's description.
// Assignee keeps the protected-claim rule (SyncOptions.ProtectedAssigneePatterns).
//
// Known window: push sends the bead's full label set, so a tracker-side label
// change made after this cycle's pull and before its push is still
// overwritten by that push.

const labelBaselineVersion = 1

type labelBaseline struct {
	Version int      `json:"v"`
	Target  string   `json:"target"`
	Labels  []string `json:"labels"`
}

func (e *Engine) labelBaselineKey(issueID string) string {
	return e.Tracker.ConfigPrefix() + ".labelbase." + issueID
}

// labelBaselineTarget is the tracker identifier a baseline is bound to, so a
// relinked bead never merges against another issue's labels.
func (e *Engine) labelBaselineTarget(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || !e.Tracker.IsExternalRef(ref) {
		return ""
	}
	return strings.TrimSpace(e.Tracker.ExtractIdentifier(ref))
}

// loadLabelBaseline returns the recorded baseline for issueID when it is
// bound to target.
func (e *Engine) loadLabelBaseline(ctx context.Context, issueID, target string) ([]string, bool) {
	if !e.ThreeWayLabelMerge || issueID == "" || target == "" {
		return nil, false
	}
	raw, err := e.Store.GetLocalMetadata(ctx, e.labelBaselineKey(issueID))
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var base labelBaseline
	if err := json.Unmarshal([]byte(raw), &base); err != nil || base.Version != labelBaselineVersion || base.Target != target {
		return nil, false
	}
	return base.Labels, true
}

// recordLabelBaseline stores labels as issueID's baseline for target. It
// reads first and writes only on change, so a no-op cycle over thousands of
// linked issues does not rewrite thousands of rows.
func (e *Engine) recordLabelBaseline(ctx context.Context, issueID, target string, labels []string) {
	if !e.ThreeWayLabelMerge || issueID == "" || target == "" || labels == nil {
		return
	}
	value, err := json.Marshal(labelBaseline{Version: labelBaselineVersion, Target: target, Labels: normalizedStringSlice(labels)})
	if err != nil {
		return
	}
	key := e.labelBaselineKey(issueID)
	if current, err := e.Store.GetLocalMetadata(ctx, key); err == nil && current == string(value) {
		return
	}
	if err := e.Store.SetLocalMetadata(ctx, key, string(value)); err != nil {
		e.warn("Failed to record label baseline for %s: %v", issueID, err)
	}
}

// recordPushedLabelBaseline records the bead's labels as the baseline after a
// push left the tracker holding them.
func (e *Engine) recordPushedLabelBaseline(ctx context.Context, issue *types.Issue, ref string) {
	if issue == nil {
		return
	}
	labels := issue.Labels
	if labels == nil {
		labels = []string{}
	}
	e.recordLabelBaseline(ctx, issue.ID, e.labelBaselineTarget(ref), labels)
}

// threeWayMergeLabels merges local and remote label sets against base: a
// label is kept when both sides have it, or when either side added it since
// base (it is absent from base). A label in base that either side removed is
// dropped. The result is normalized (trimmed, deduplicated, sorted).
func threeWayMergeLabels(base, local, remote []string) []string {
	baseSet := normalizedStringSet(base)
	localSet := normalizedStringSet(local)
	remoteSet := normalizedStringSet(remote)
	merged := make([]string, 0, len(localSet)+len(remoteSet))
	seen := make(map[string]struct{}, len(localSet)+len(remoteSet))
	consider := func(label string) {
		if _, done := seen[label]; done {
			return
		}
		seen[label] = struct{}{}
		_, inBase := baseSet[label]
		_, inLocal := localSet[label]
		_, inRemote := remoteSet[label]
		if (inLocal && inRemote) || (!inBase && (inLocal || inRemote)) {
			merged = append(merged, label)
		}
	}
	for label := range localSet {
		consider(label)
	}
	for label := range remoteSet {
		consider(label)
	}
	sort.Strings(merged)
	return merged
}

// mergePulledLabels returns the label set a pull should give an existing
// issue: the three-way merge when a baseline exists, otherwise the tracker's
// labels unchanged (the pre-merge behavior).
func (e *Engine) mergePulledLabels(ctx context.Context, existing *types.Issue, remoteLabels []string, target string) ([]string, bool) {
	if !e.ThreeWayLabelMerge || existing == nil || remoteLabels == nil {
		return remoteLabels, false
	}
	base, ok := e.loadLabelBaseline(ctx, existing.ID, target)
	if !ok {
		return remoteLabels, false
	}
	return threeWayMergeLabels(base, existing.Labels, remoteLabels), true
}
