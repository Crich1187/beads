package main

import (
	"encoding/json"
	"testing"

	"github.com/steveyegge/beads/internal/scopedbundle"
)

// The scoped inspector reports the database schema version so an operator can
// confirm which schema a migration digest was taken against. The generic JSON
// wrapper injects its own output-format version under the key "schema_version"
// (JSONSchemaVersion, currently 1) and overwrites whatever the payload already
// had at that key. Reusing the same key therefore silently replaced a real
// database schema version such as 66 with the constant 1 on the legacy
// (non-envelope) JSON path.
//
// That is what produced the unexplained "schema_version=1" reading in the
// root-55fr9.13.21 pre-migration preflight: the inspector never read schema v1
// from the database. InspectSchema rejects anything below v53 outright, so a
// genuine v1 read could not have returned successfully at all.
//
// These tests pin the fix: the database schema version travels under a
// non-colliding key and survives both JSON paths intact.

func TestScopedStateOutputUsesNonCollidingSchemaKey(t *testing.T) {
	state := scopedbundle.State{
		Schema: scopedbundle.Schema{Version: 66},
		SHA256: "state-digest",
	}
	mapping := scopedbundle.Mapping{SHA256: "map-digest", ExpectedCount: 19}

	out := scopedStateOutput(state, mapping, "source")

	if _, collides := out["schema_version"]; collides {
		t.Fatalf("scoped output must not use the reserved JSON envelope key %q", "schema_version")
	}
	got, ok := out["db_schema_version"]
	if !ok {
		t.Fatalf("scoped output missing db_schema_version; keys = %v", keysOf(out))
	}
	if got != 66 {
		t.Errorf("db_schema_version = %v, want 66", got)
	}
}

// TestScopedSchemaVersionSurvivesLegacyJSONWrapper is the regression that fails
// against the pre-fix code: wrapWithSchemaVersion overwrites m["schema_version"].
func TestScopedSchemaVersionSurvivesLegacyJSONWrapper(t *testing.T) {
	t.Setenv("BD_JSON_ENVELOPE", "") // legacy, non-envelope path

	state := scopedbundle.State{
		Schema: scopedbundle.Schema{Version: 66},
		SHA256: "state-digest",
	}
	mapping := scopedbundle.Mapping{SHA256: "map-digest", ExpectedCount: 19}

	wrapped := wrapWithSchemaVersion(scopedStateOutput(state, mapping, "source"))
	data, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}

	// The wrapper's own format version is expected to be present and to be 1.
	if v, ok := m["schema_version"]; !ok || v != float64(JSONSchemaVersion) {
		t.Errorf("envelope schema_version = %v (present=%v), want %d", v, ok, JSONSchemaVersion)
	}
	// The database schema version must survive unmodified alongside it.
	if v, ok := m["db_schema_version"]; !ok || v != float64(66) {
		t.Errorf("db_schema_version = %v (present=%v), want 66 — the JSON wrapper clobbered the database schema identity", v, ok)
	}
}

// TestScopedSchemaVersionSurvivesEnvelopeJSON covers the opt-in envelope path,
// where the payload is nested under "data" and no collision was possible.
func TestScopedSchemaVersionSurvivesEnvelopeJSON(t *testing.T) {
	t.Setenv("BD_JSON_ENVELOPE", "1")

	state := scopedbundle.State{
		Schema: scopedbundle.Schema{Version: 66},
		SHA256: "state-digest",
	}
	mapping := scopedbundle.Mapping{SHA256: "map-digest", ExpectedCount: 19}

	wrapped := wrapWithSchemaVersion(scopedStateOutput(state, mapping, "source"))
	data, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	inner, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope missing data object; got %v", m)
	}
	if v, ok := inner["db_schema_version"]; !ok || v != float64(66) {
		t.Errorf("data.db_schema_version = %v (present=%v), want 66", v, ok)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
