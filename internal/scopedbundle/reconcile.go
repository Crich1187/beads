package scopedbundle

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Reconciliation exists because `apply` alone cannot produce the required
// destination union. `apply` deliberately rejects every destination-only or
// content-colliding comment/event row before mutating anything, which is the
// correct default: it prevents an unreviewed destination from being silently
// overwritten or silently absorbed. For the root-55fr9.13.21 migration the
// destination legitimately holds history the source does not, so that history
// must be retained rather than rejected — but only under an explicit, reviewed,
// digest-bound manifest.
//
// The rule this file follows is therefore: reconciliation NEVER weakens
// `apply`. `apply` keeps its rejections unchanged. Reconciliation is a separate,
// sealed entry point that still rejects everything the operator did not
// explicitly enumerate and seal. An unlisted destination row is as fatal here as
// it is in `apply`.

// ReconcileManifestFormat and ReconcileManifestVersion identify the sealed
// manifest shape. A manifest whose format/version is not exactly this is refused.
const (
	ReconcileManifestFormat  = "beads.scopedbundle.reconcile"
	ReconcileManifestVersion = 1
)

// SchemaAddition is one explicitly reviewed target column to create before the
// union is written. It exists to satisfy a source-only column that carries real
// scoped values (root-55fr9.13.21: issues.row_lock has four non-default values),
// without granting the tool a general migration capability.
//
// Only ADD COLUMN is expressible. There is deliberately no way to drop, rename,
// retype or reorder a column, and no way to touch a table outside the five-table
// contract or a database other than the one the workspace already binds.
type SchemaAddition struct {
	Table    string `json:"table"`
	Column   string `json:"column"`
	SQLType  string `json:"sql_type"`
	Nullable bool   `json:"nullable"`
	// Default is the literal SQL default. It is required for a NOT NULL column
	// so an existing destination row can never be left in an invalid state.
	Default string `json:"default,omitempty"`
}

// CommentLink records that a destination comment is the same comment as a source
// comment, after deterministic free-text reference rewriting. Linked pairs are
// retained once, under the source identity, instead of being rejected as a
// content collision or duplicated into the union.
type CommentLink struct {
	TargetID string `json:"target_id"`
	SourceID string `json:"source_id"`
}

// ReconcileManifest is the sealed, reviewed description of a union. Every
// destination row that is to survive must be named here; anything unlisted is
// rejected.
type ReconcileManifest struct {
	Format  string `json:"format"`
	Version int    `json:"version"`

	// ExpectedSourceSHA256 and ExpectedTargetSHA256 bind this manifest to the
	// exact two states it was reviewed against. Either mismatching is fatal.
	ExpectedSourceSHA256 string `json:"expected_source_sha256"`
	ExpectedTargetSHA256 string `json:"expected_target_sha256"`

	SchemaAdditions []SchemaAddition `json:"schema_additions,omitempty"`

	// CommentLinks are destination comments that are semantically the same as a
	// source comment. RetainTargetCommentIDs are destination-only comments kept
	// as they stand. RetainTargetEventIDs and RetainSourceEventIDs enumerate the
	// event union explicitly, because event identity is not reconstructable.
	CommentLinks          []CommentLink `json:"comment_links,omitempty"`
	RetainTargetCommentIDs []string     `json:"retain_target_comment_ids,omitempty"`
	RetainTargetEventIDs   []string     `json:"retain_target_event_ids,omitempty"`
	RetainSourceEventIDs   []string     `json:"retain_source_event_ids,omitempty"`

	SHA256 string `json:"sha256,omitempty"`
}

// Canonical returns a deterministic copy: every list sorted, so an identical
// reviewed intent always seals to an identical digest regardless of input order.
func (m ReconcileManifest) Canonical() ReconcileManifest {
	out := m

	out.SchemaAdditions = append([]SchemaAddition(nil), m.SchemaAdditions...)
	sort.Slice(out.SchemaAdditions, func(i, j int) bool {
		if out.SchemaAdditions[i].Table != out.SchemaAdditions[j].Table {
			return out.SchemaAdditions[i].Table < out.SchemaAdditions[j].Table
		}
		return out.SchemaAdditions[i].Column < out.SchemaAdditions[j].Column
	})

	out.CommentLinks = append([]CommentLink(nil), m.CommentLinks...)
	sort.Slice(out.CommentLinks, func(i, j int) bool {
		if out.CommentLinks[i].SourceID != out.CommentLinks[j].SourceID {
			return out.CommentLinks[i].SourceID < out.CommentLinks[j].SourceID
		}
		return out.CommentLinks[i].TargetID < out.CommentLinks[j].TargetID
	})

	out.RetainTargetCommentIDs = sortedCopy(m.RetainTargetCommentIDs)
	out.RetainTargetEventIDs = sortedCopy(m.RetainTargetEventIDs)
	out.RetainSourceEventIDs = sortedCopy(m.RetainSourceEventIDs)
	return out
}

func sortedCopy(in []string) []string {
	if in == nil {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (m ReconcileManifest) validateShape() error {
	if m.Format != ReconcileManifestFormat || m.Version != ReconcileManifestVersion {
		return fmt.Errorf("unsupported reconcile manifest format/version %q/%d", m.Format, m.Version)
	}
	if !isHexDigest(m.ExpectedSourceSHA256) {
		return fmt.Errorf("expected_source_sha256 must be a 64-character hex digest")
	}
	if !isHexDigest(m.ExpectedTargetSHA256) {
		return fmt.Errorf("expected_target_sha256 must be a 64-character hex digest")
	}

	seenColumn := make(map[string]struct{}, len(m.SchemaAdditions))
	for _, addition := range m.SchemaAdditions {
		if !isTransferredTable(addition.Table) {
			return fmt.Errorf("schema addition targets table %q outside the five-table contract", addition.Table)
		}
		if strings.TrimSpace(addition.Column) == "" || strings.TrimSpace(addition.SQLType) == "" {
			return fmt.Errorf("schema addition for %s has an empty column or type", addition.Table)
		}
		if !addition.Nullable && strings.TrimSpace(addition.Default) == "" {
			return fmt.Errorf("schema addition %s.%s is NOT NULL and needs an explicit default", addition.Table, addition.Column)
		}
		key := addition.Table + "." + addition.Column
		if _, dup := seenColumn[key]; dup {
			return fmt.Errorf("duplicate schema addition %s", key)
		}
		seenColumn[key] = struct{}{}
	}

	seenTarget := make(map[string]struct{}, len(m.CommentLinks))
	seenSource := make(map[string]struct{}, len(m.CommentLinks))
	for _, link := range m.CommentLinks {
		if strings.TrimSpace(link.TargetID) == "" || strings.TrimSpace(link.SourceID) == "" {
			return fmt.Errorf("comment link has an empty identity")
		}
		if _, dup := seenTarget[link.TargetID]; dup {
			return fmt.Errorf("destination comment %q is linked more than once", link.TargetID)
		}
		if _, dup := seenSource[link.SourceID]; dup {
			return fmt.Errorf("source comment %q is linked more than once", link.SourceID)
		}
		seenTarget[link.TargetID] = struct{}{}
		seenSource[link.SourceID] = struct{}{}
	}

	for name, ids := range map[string][]string{
		"retain_target_comment_ids": m.RetainTargetCommentIDs,
		"retain_target_event_ids":   m.RetainTargetEventIDs,
		"retain_source_event_ids":   m.RetainSourceEventIDs,
	} {
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("%s contains an empty identity", name)
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%s contains duplicate %q", name, id)
			}
			seen[id] = struct{}{}
		}
	}

	// A destination comment cannot be both "the same as a source comment" and
	// "destination-only". That would double-count it in the union.
	for _, id := range m.RetainTargetCommentIDs {
		if _, linked := seenTarget[id]; linked {
			return fmt.Errorf("destination comment %q is both linked and retained as destination-only", id)
		}
	}
	return nil
}

// Digest computes the sealed digest of the reviewed manifest shape.
func (m ReconcileManifest) Digest() (string, error) {
	if err := m.validateShape(); err != nil {
		return "", err
	}
	canonical := m.Canonical()
	canonical.SHA256 = ""
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode reconcile manifest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Seal records the digest of the exact reviewed manifest.
func (m *ReconcileManifest) Seal() error {
	if m == nil {
		return fmt.Errorf("nil reconcile manifest")
	}
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	m.SHA256 = digest
	return nil
}

// Verify refuses a manifest whose contents do not match its declared seal.
func (m ReconcileManifest) Verify() error {
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	if m.SHA256 == "" || m.SHA256 != digest {
		return fmt.Errorf("reconcile manifest digest mismatch: declared %s computed %s", m.SHA256, digest)
	}
	return nil
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func isTransferredTable(name string) bool {
	for _, t := range transferredTables {
		if t == name {
			return true
		}
	}
	return false
}

// PlanSchemaAdditions checks the reviewed additions against the bundle and the
// live target schema and returns the DDL that would be executed, in canonical
// order. It is fail-closed in both directions:
//
//   - an addition that does not correspond to a real source-only column is
//     refused, so the manifest cannot invent columns;
//   - a source-only column that carries non-default scoped values and is NOT
//     covered by an addition is refused, which is exactly the guarantee
//     CheckCompatibility already provides and which must not be relaxed.
func PlanSchemaAdditions(bundle Bundle, target Schema, manifest ReconcileManifest) ([]string, error) {
	if err := manifest.Verify(); err != nil {
		return nil, err
	}
	if err := bundle.Verify(); err != nil {
		return nil, fmt.Errorf("verify bundle: %w", err)
	}

	covered := make(map[string]SchemaAddition, len(manifest.SchemaAdditions))
	for _, addition := range manifest.SchemaAdditions {
		sourceTable, ok := findTable(bundle.Tables, addition.Table)
		if !ok {
			return nil, fmt.Errorf("schema addition %s.%s names a table absent from the bundle", addition.Table, addition.Column)
		}
		sourceColumn, exists := columnsByName(sourceTable.Columns)[addition.Column]
		if !exists {
			return nil, fmt.Errorf("schema addition %s.%s does not exist in the source", addition.Table, addition.Column)
		}
		if typeFamily(sourceColumn.SQLType) != typeFamily(addition.SQLType) {
			return nil, fmt.Errorf("schema addition %s.%s type %s does not match source type %s",
				addition.Table, addition.Column, addition.SQLType, sourceColumn.SQLType)
		}
		if _, present := columnsByName(target.Tables[addition.Table])[addition.Column]; present {
			return nil, fmt.Errorf("schema addition %s.%s already exists in the target", addition.Table, addition.Column)
		}
		covered[addition.Table+"."+addition.Column] = addition
	}

	// Do not relax the existing guarantee: any source-only column still holding
	// real scoped values must be covered by a reviewed addition.
	for _, sourceTable := range bundle.Tables {
		targetByName := columnsByName(target.Tables[sourceTable.Name])
		for index, sourceColumn := range sourceTable.Columns {
			if _, exists := targetByName[sourceColumn.Name]; exists {
				continue
			}
			if !columnHasNonDefaultValue(sourceTable, index, sourceColumn) {
				continue
			}
			if _, ok := covered[sourceTable.Name+"."+sourceColumn.Name]; !ok {
				return nil, fmt.Errorf("source-only column %s.%s has a non-default scoped value and is not covered by a reviewed schema addition",
					sourceTable.Name, sourceColumn.Name)
			}
		}
	}

	statements := make([]string, 0, len(manifest.SchemaAdditions))
	for _, addition := range manifest.Canonical().SchemaAdditions {
		stmt := "ALTER TABLE " + quoteIdentifier(addition.Table) +
			" ADD COLUMN " + quoteIdentifier(addition.Column) + " " + addition.SQLType
		if addition.Nullable {
			stmt += " NULL"
		} else {
			stmt += " NOT NULL DEFAULT " + addition.Default
		}
		statements = append(statements, stmt)
	}
	return statements, nil
}

// ReconcilePlan is the fully resolved, reviewed union. Producing a plan performs
// no writes; it is the auditable artifact an operator inspects before execution.
type ReconcilePlan struct {
	SchemaStatements    []string
	LinkedComments      []CommentLink
	RetainedTargetOnly  []string
	RetainedTargetEvent []string
	RetainedSourceEvent []string
}

// PlanReconcile resolves the sealed manifest against the exact source and target
// states. It writes nothing. Every destination comment and event must be
// enumerated by the manifest; an unlisted destination row is rejected exactly as
// `apply` would reject it.
func PlanReconcile(bundle Bundle, target State, targetSchema Schema, manifest ReconcileManifest) (ReconcilePlan, error) {
	if err := manifest.Verify(); err != nil {
		return ReconcilePlan{}, err
	}
	if bundle.SourceStateSHA256 != manifest.ExpectedSourceSHA256 {
		return ReconcilePlan{}, fmt.Errorf("source state digest mismatch: manifest %s bundle %s",
			manifest.ExpectedSourceSHA256, bundle.SourceStateSHA256)
	}
	if target.SHA256 != manifest.ExpectedTargetSHA256 {
		return ReconcilePlan{}, fmt.Errorf("target state digest mismatch: manifest %s target %s",
			manifest.ExpectedTargetSHA256, target.SHA256)
	}

	statements, err := PlanSchemaAdditions(bundle, targetSchema, manifest)
	if err != nil {
		return ReconcilePlan{}, err
	}

	listed := make(map[string]struct{})
	for _, link := range manifest.CommentLinks {
		listed[link.TargetID] = struct{}{}
	}
	for _, id := range manifest.RetainTargetCommentIDs {
		listed[id] = struct{}{}
	}
	if err := requireEveryRowListed(target, "comments", listed); err != nil {
		return ReconcilePlan{}, err
	}

	listedEvents := make(map[string]struct{}, len(manifest.RetainTargetEventIDs))
	for _, id := range manifest.RetainTargetEventIDs {
		listedEvents[id] = struct{}{}
	}
	if err := requireEveryRowListed(target, "events", listedEvents); err != nil {
		return ReconcilePlan{}, err
	}

	canonical := manifest.Canonical()
	return ReconcilePlan{
		SchemaStatements:    statements,
		LinkedComments:      canonical.CommentLinks,
		RetainedTargetOnly:  canonical.RetainTargetCommentIDs,
		RetainedTargetEvent: canonical.RetainTargetEventIDs,
		RetainedSourceEvent: canonical.RetainSourceEventIDs,
	}, nil
}

// requireEveryRowListed rejects any destination row the manifest did not
// enumerate, and any enumerated identity that is not actually present.
func requireEveryRowListed(target State, table string, listed map[string]struct{}) error {
	live, _ := findTable(target.Tables, table)
	present := make(map[string]struct{}, len(live.Rows))
	for _, row := range live.Rows {
		id, err := rowID(live, row)
		if err != nil {
			return err
		}
		present[id] = struct{}{}
		if _, ok := listed[id]; !ok {
			return fmt.Errorf("destination %s row %q is not listed in the reviewed manifest", table, id)
		}
	}
	for id := range listed {
		if _, ok := present[id]; !ok {
			return fmt.Errorf("manifest lists %s row %q which is absent from the destination", table, id)
		}
	}
	return nil
}

// ApplySchemaAdditions executes the reviewed ADD COLUMN statements on the bound
// transaction. It runs only statements PlanSchemaAdditions produced, so it can
// never widen beyond the sealed manifest, and it touches only the currently
// bound database — no other database is opened, named or migrated.
func ApplySchemaAdditions(ctx context.Context, tx *sql.Tx, statements []string) error {
	for _, stmt := range statements {
		if !strings.HasPrefix(stmt, "ALTER TABLE ") || !strings.Contains(stmt, " ADD COLUMN ") {
			return fmt.Errorf("refusing non-additive schema statement")
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema addition: %w", err)
		}
	}
	return nil
}
