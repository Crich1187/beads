package scopedbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	BundleFormat  = "beads.scoped-bundle"
	BundleVersion = 1
)

// Cell is a lossless scalar as returned by the MySQL protocol. Null is kept
// separate from an empty string so defaults and exact row equality are honest.
type Cell struct {
	Null bool   `json:"null,omitempty"`
	Text string `json:"text,omitempty"`
}

// Column describes one source SQL column. Default is nil when no default is
// declared. Generated columns are included for compatibility checks but never
// written.
type Column struct {
	Name      string  `json:"name"`
	SQLType   string  `json:"sql_type"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default,omitempty"`
	Generated bool    `json:"generated,omitempty"`
}

type Row struct {
	Cells []Cell `json:"cells"`
}

type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	Rows    []Row    `json:"rows"`
}

type Bundle struct {
	Format             string  `json:"format"`
	Version            int     `json:"version"`
	SourceSchema       int     `json:"source_schema"`
	Mapping            Mapping `json:"mapping"`
	Tables             []Table `json:"tables"`
	SourceStateSHA256  string  `json:"source_state_sha256"`
	DesiredStateSHA256 string  `json:"desired_state_sha256"`
	SHA256             string  `json:"sha256"`
}

func (b *Bundle) Seal() error {
	if b == nil {
		return fmt.Errorf("nil bundle")
	}
	if err := b.validateShape(); err != nil {
		return err
	}
	source, err := digestTables(b.Tables)
	if err != nil {
		return err
	}
	mapped, err := b.MappedTables()
	if err != nil {
		return err
	}
	desired, err := digestTables(mapped)
	if err != nil {
		return err
	}
	b.SourceStateSHA256 = source
	b.DesiredStateSHA256 = desired
	digest, err := b.digest()
	if err != nil {
		return err
	}
	b.SHA256 = digest
	return nil
}

func (b Bundle) Verify() error {
	if err := b.validateShape(); err != nil {
		return err
	}
	declared := b.SHA256
	digest, err := b.digest()
	if err != nil {
		return err
	}
	if declared == "" || declared != digest {
		return fmt.Errorf("bundle digest mismatch: declared %s computed %s", declared, digest)
	}
	source, err := digestTables(b.Tables)
	if err != nil {
		return err
	}
	if source != b.SourceStateSHA256 {
		return fmt.Errorf("source state digest mismatch")
	}
	mapped, err := b.MappedTables()
	if err != nil {
		return err
	}
	desired, err := digestTables(mapped)
	if err != nil {
		return err
	}
	if desired != b.DesiredStateSHA256 {
		return fmt.Errorf("desired state digest mismatch")
	}
	return nil
}

func (b Bundle) validateShape() error {
	if b.Format != BundleFormat || b.Version != BundleVersion {
		return fmt.Errorf("unsupported bundle format/version %q/%d", b.Format, b.Version)
	}
	if b.SourceSchema < 53 {
		return fmt.Errorf("source schema v%d is below minimum v53", b.SourceSchema)
	}
	if err := b.Mapping.Validate(); err != nil {
		return fmt.Errorf("mapping: %w", err)
	}
	seenTables := make(map[string]struct{}, len(b.Tables))
	allowedTables := make(map[string]struct{}, len(transferredTables))
	for _, name := range transferredTables {
		allowedTables[name] = struct{}{}
	}
	for _, table := range b.Tables {
		if strings.TrimSpace(table.Name) == "" {
			return fmt.Errorf("bundle contains an unnamed table")
		}
		if _, exists := seenTables[table.Name]; exists {
			return fmt.Errorf("bundle contains duplicate table %s", table.Name)
		}
		if _, allowed := allowedTables[table.Name]; !allowed {
			return fmt.Errorf("bundle contains unexpected table %s", table.Name)
		}
		seenTables[table.Name] = struct{}{}
		seenColumns := make(map[string]struct{}, len(table.Columns))
		for _, column := range table.Columns {
			if column.Name == "" {
				return fmt.Errorf("table %s contains an unnamed column", table.Name)
			}
			if _, exists := seenColumns[column.Name]; exists {
				return fmt.Errorf("table %s contains duplicate column %s", table.Name, column.Name)
			}
			seenColumns[column.Name] = struct{}{}
		}
		for i, row := range table.Rows {
			if len(row.Cells) != len(table.Columns) {
				return fmt.Errorf("table %s row %d has %d cells for %d columns", table.Name, i, len(row.Cells), len(table.Columns))
			}
		}
	}
	for _, name := range transferredTables {
		if _, ok := seenTables[name]; !ok {
			return fmt.Errorf("bundle has no %s table", name)
		}
	}
	// The required-table loop above is the single fail-closed presence guard
	// for all five tables, including issues. Avoid a second unreachable issues
	// guard whose removal is an equivalent mutation rather than a bypass.
	issues, _ := findTable(b.Tables, "issues")
	if len(issues.Rows) != b.Mapping.ExpectedCount {
		return fmt.Errorf("issues row count %d does not match reviewed mapping count %d", len(issues.Rows), b.Mapping.ExpectedCount)
	}
	return nil
}

func (b Bundle) digest() (string, error) {
	canonical := b
	canonical.Tables = canonicalTables(b.Tables)
	canonical.Mapping = b.Mapping.Canonical()
	canonical.SHA256 = ""
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode bundle: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func digestTables(tables []Table) (string, error) {
	encoded, err := json.Marshal(canonicalTables(tables))
	if err != nil {
		return "", fmt.Errorf("encode table state: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalTables(tables []Table) []Table {
	out := make([]Table, len(tables))
	for i, table := range tables {
		out[i] = table
		out[i].Columns = append([]Column(nil), table.Columns...)
		out[i].Rows = make([]Row, len(table.Rows))
		for j, row := range table.Rows {
			out[i].Rows[j] = Row{Cells: append([]Cell(nil), row.Cells...)}
		}
		sort.Slice(out[i].Rows, func(a, b int) bool {
			left, _ := json.Marshal(out[i].Rows[a])
			right, _ := json.Marshal(out[i].Rows[b])
			return string(left) < string(right)
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func findTable(tables []Table, name string) (Table, bool) {
	for _, table := range tables {
		if table.Name == name {
			return table, true
		}
	}
	return Table{}, false
}

func tableColumnIndex(table Table, name string) (int, bool) {
	for i, column := range table.Columns {
		if column.Name == name {
			return i, true
		}
	}
	return -1, false
}

func (b Bundle) MappedTables() ([]Table, error) {
	if err := b.Mapping.Validate(); err != nil {
		return nil, err
	}
	tables := canonicalTables(b.Tables)
	for ti := range tables {
		table := &tables[ti]
		var refs []string
		switch table.Name {
		case "issues":
			refs = []string{"id"}
		case "labels", "comments", "events":
			refs = []string{"issue_id"}
		case "dependencies":
			refs = []string{"issue_id", "depends_on_issue_id"}
			for _, forbidden := range []string{"depends_on_wisp_id", "depends_on_external"} {
				idx, ok := tableColumnIndex(*table, forbidden)
				if !ok {
					continue
				}
				for _, row := range table.Rows {
					if !row.Cells[idx].Null && row.Cells[idx].Text != "" {
						return nil, fmt.Errorf("closed-set violation: dependency %s target %q is not an approved issue reference", forbidden, row.Cells[idx].Text)
					}
				}
			}
		}
		for _, ref := range refs {
			idx, ok := tableColumnIndex(*table, ref)
			if !ok {
				return nil, fmt.Errorf("table %s is missing reference column %s", table.Name, ref)
			}
			for ri := range table.Rows {
				cell := &table.Rows[ri].Cells[idx]
				if cell.Null || cell.Text == "" {
					return nil, fmt.Errorf("closed-set violation: %s.%s is empty", table.Name, ref)
				}
				mapped, err := b.Mapping.TargetFor(cell.Text)
				if err != nil {
					return nil, fmt.Errorf("closed-set violation in %s.%s: %w", table.Name, ref, err)
				}
				cell.Text = mapped
			}
		}
	}
	return canonicalTables(tables), nil
}
