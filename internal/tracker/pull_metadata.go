package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// mergePulledMetadata folds tracker metadata from a pull (TrackerIssue.Metadata)
// into a bead's existing metadata column and reports whether the result differs
// from what is stored.
//
// Pull must not treat the tracker's metadata as the whole column: beads keep
// sibling keys there (e.g. linear.relations, linear.comment_watermark,
// linear.milestone_assigned, and non-tracker keys) that the tracker never
// reports. The merge is deliberately depth-limited rather than a full RFC 7396
// recursive merge patch:
//
//   - Top level: a JSON null deletes the key. When both the incoming and the
//     existing value are objects (a tracker namespace such as "linear"), they
//     are merged one level down. Any other incoming value replaces the key.
//   - Inside a namespace: each incoming key replaces the existing value
//     wholesale (so a new linear.project_milestone object never inherits stale
//     fields from the previous milestone), and a JSON null deletes the key.
//   - A namespace object left empty by the merge is removed rather than kept
//     as {}, so "no milestone" on an issue without Linear metadata is a no-op.
//
// incoming == nil means the tracker reported no metadata: the result is
// (existing, false, nil) and nothing is written.
//
// Existing metadata that is empty, whitespace or JSON null is treated as {}.
// Existing metadata that is not a JSON object (a string, array, number, or
// invalid JSON) cannot be merged into without losing it, so it is returned
// with an error; callers must leave the column untouched and warn.
//
// Changed is decided on decoded values, not bytes, because the store
// normalizes the metadata column and key order/whitespace may differ.
func mergePulledMetadata(existing json.RawMessage, incoming map[string]interface{}) (json.RawMessage, bool, error) {
	if incoming == nil {
		return existing, false, nil
	}

	// Normalize incoming through JSON so typed nils (e.g. a nil
	// *ProjectMilestone stored in an interface{}) are seen as JSON null.
	incomingRaw, err := json.Marshal(incoming)
	if err != nil {
		return existing, false, fmt.Errorf("marshal tracker metadata: %w", err)
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(incomingRaw, &patch); err != nil {
		return existing, false, fmt.Errorf("decode tracker metadata: %w", err)
	}

	base, err := decodeMetadataObject(existing)
	if err != nil {
		return existing, false, err
	}
	original, err := decodeMetadataObject(existing)
	if err != nil {
		return existing, false, err
	}

	for key, value := range patch {
		if isJSONNull(value) {
			delete(base, key)
			continue
		}
		patchNS, patchIsObject := decodeObject(value)
		baseNS, baseIsObject := decodeObject(base[key])
		if !patchIsObject || !baseIsObject {
			if patchIsObject {
				// No existing object to merge into: start from empty so null
				// deletes inside the namespace are dropped, not stored.
				baseNS = map[string]json.RawMessage{}
			} else {
				base[key] = value
				continue
			}
		}
		for subKey, subValue := range patchNS {
			if isJSONNull(subValue) {
				delete(baseNS, subKey)
				continue
			}
			baseNS[subKey] = subValue
		}
		if len(baseNS) == 0 {
			delete(base, key)
			continue
		}
		nsRaw, err := json.Marshal(baseNS)
		if err != nil {
			return existing, false, fmt.Errorf("marshal metadata namespace %q: %w", key, err)
		}
		base[key] = nsRaw
	}

	merged, err := json.Marshal(base)
	if err != nil {
		return existing, false, fmt.Errorf("marshal merged metadata: %w", err)
	}
	changed, err := metadataObjectsDiffer(original, base)
	if err != nil {
		return existing, false, err
	}
	return json.RawMessage(merged), changed, nil
}

// decodeMetadataObject decodes a metadata column into a raw-value map. Empty,
// whitespace-only and JSON null yield an empty map; anything that is not a
// JSON object is an error.
func decodeMetadataObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		return map[string]json.RawMessage{}, nil
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("existing metadata is not a JSON object")
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, fmt.Errorf("existing metadata is not a JSON object: %w", err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, false
	}
	return m, true
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func metadataObjectsDiffer(a, b map[string]json.RawMessage) (bool, error) {
	av, err := decodeJSONValue(a)
	if err != nil {
		return false, err
	}
	bv, err := decodeJSONValue(b)
	if err != nil {
		return false, err
	}
	return !reflect.DeepEqual(av, bv), nil
}

// decodeJSONValue decodes m into plain Go values for a semantic comparison.
// Numbers decode to float64 on purpose: the store may re-render a number
// (60.61 vs 6.061e1, 60 vs 60.0), and comparing number text would make every
// pull rewrite the metadata of every milestone child.
func decodeJSONValue(m map[string]json.RawMessage) (interface{}, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata for comparison: %w", err)
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode metadata for comparison: %w", err)
	}
	return v, nil
}

// pulledMetadataUpdate returns the metadata column value a pull should write
// for issueID, and whether it should be written at all. existing is the
// bead's current metadata (nil for a create). A merge failure is reported as
// a warning and the column is left untouched; the rest of the pull proceeds.
func (e *Engine) pulledMetadataUpdate(issueID string, existing json.RawMessage, incoming map[string]interface{}) (json.RawMessage, bool) {
	merged, changed, err := mergePulledMetadata(existing, incoming)
	if err != nil {
		e.warn("Keeping local metadata on %s unchanged: cannot merge %s metadata: %v", issueID, e.Tracker.DisplayName(), err)
		return nil, false
	}
	return merged, changed
}

// pulledIssueMetadata is pulledMetadataUpdate for doPull: existing is the
// matched local bead (nil for a create) and extIssue the pulled issue.
func (e *Engine) pulledIssueMetadata(existing *types.Issue, extIssue *TrackerIssue) (json.RawMessage, bool) {
	if existing == nil {
		return e.pulledMetadataUpdate(extIssue.Identifier, nil, extIssue.Metadata)
	}
	return e.pulledMetadataUpdate(existing.ID, existing.Metadata, extIssue.Metadata)
}

// localIssueMetadata loads the current metadata column for a local issue by
// ID. The tracker Store has no GetIssue, so it searches by ID and selects the
// exact match (stores that ignore the filter still return the right row).
func (e *Engine) localIssueMetadata(ctx context.Context, issueID string) (json.RawMessage, error) {
	issues, err := e.Store.SearchIssues(ctx, "", types.IssueFilter{IDs: []string{issueID}})
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if issue != nil && strings.TrimSpace(issue.ID) == issueID {
			return issue.Metadata, nil
		}
	}
	return nil, fmt.Errorf("local issue %s not found", issueID)
}
