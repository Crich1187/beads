// Package scopedbundle provides exact, closed-set export and apply primitives
// for a bounded set of Beads rows. It deliberately operates below normal store
// opening so callers can inspect compatible historical schemas without running
// migrations.
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
	// MappingVersion is the only mapping contract understood by this release.
	MappingVersion = 1
)

// IDPair maps one source issue ID to one destination issue ID.
type IDPair struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Mapping is the explicit, versioned issue-ID allowlist for one bundle.
type Mapping struct {
	Version       int      `json:"version"`
	ExpectedCount int      `json:"expected_count"`
	SourcePrefix  string   `json:"source_prefix"`
	TargetPrefix  string   `json:"target_prefix"`
	Pairs         []IDPair `json:"pairs"`
	SHA256        string   `json:"sha256,omitempty"`
}

func (m Mapping) validateShape() error {
	if m.Version != MappingVersion {
		return fmt.Errorf("unsupported mapping version %d (want %d)", m.Version, MappingVersion)
	}
	if m.ExpectedCount <= 0 {
		return fmt.Errorf("mapping expected_count must be positive")
	}
	if len(m.Pairs) != m.ExpectedCount {
		return fmt.Errorf("mapping expected %d pairs but contains %d", m.ExpectedCount, len(m.Pairs))
	}
	if strings.TrimSpace(m.SourcePrefix) == "" || strings.ContainsAny(m.SourcePrefix, "%_") {
		return fmt.Errorf("mapping source_prefix must be nonempty and contain no SQL wildcards")
	}
	if strings.TrimSpace(m.TargetPrefix) == "" || strings.ContainsAny(m.TargetPrefix, "%_") {
		return fmt.Errorf("mapping target_prefix must be nonempty and contain no SQL wildcards")
	}

	sources := make(map[string]struct{}, len(m.Pairs))
	targets := make(map[string]struct{}, len(m.Pairs))
	for i, pair := range m.Pairs {
		if strings.TrimSpace(pair.Source) == "" {
			return fmt.Errorf("mapping pair %d has empty source", i)
		}
		if strings.TrimSpace(pair.Target) == "" {
			return fmt.Errorf("mapping pair %d has empty target", i)
		}
		if !strings.HasPrefix(pair.Source, m.SourcePrefix) {
			return fmt.Errorf("mapping source ID %q is outside source_prefix %q", pair.Source, m.SourcePrefix)
		}
		if !strings.HasPrefix(pair.Target, m.TargetPrefix) {
			return fmt.Errorf("mapping target ID %q is outside target_prefix %q", pair.Target, m.TargetPrefix)
		}
		if _, exists := sources[pair.Source]; exists {
			return fmt.Errorf("duplicate source ID %q", pair.Source)
		}
		if _, exists := targets[pair.Target]; exists {
			return fmt.Errorf("duplicate target ID %q", pair.Target)
		}
		sources[pair.Source] = struct{}{}
		targets[pair.Target] = struct{}{}
	}
	return nil
}

// Validate rejects any set that differs from its explicitly reviewed count or
// digest. The count is data, not a tool constant, so a newly reviewed manifest
// can grow without allowing silent scope expansion.
func (m Mapping) Validate() error {
	if err := m.validateShape(); err != nil {
		return err
	}
	if m.SHA256 == "" {
		return fmt.Errorf("mapping digest is required")
	}
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	if digest != m.SHA256 {
		return fmt.Errorf("mapping digest mismatch: declared %s computed %s", m.SHA256, digest)
	}
	return nil
}

// Canonical returns a deep copy ordered by source then target ID.
func (m Mapping) Canonical() Mapping {
	out := Mapping{
		Version:       m.Version,
		ExpectedCount: m.ExpectedCount,
		SourcePrefix:  m.SourcePrefix,
		TargetPrefix:  m.TargetPrefix,
		Pairs:         append([]IDPair(nil), m.Pairs...),
		SHA256:        m.SHA256,
	}
	sort.Slice(out.Pairs, func(i, j int) bool {
		if out.Pairs[i].Source != out.Pairs[j].Source {
			return out.Pairs[i].Source < out.Pairs[j].Source
		}
		return out.Pairs[i].Target < out.Pairs[j].Target
	})
	return out
}

// Digest returns the lowercase SHA-256 of the canonical JSON mapping.
func (m Mapping) Digest() (string, error) {
	if err := m.validateShape(); err != nil {
		return "", err
	}
	canonical := m.Canonical()
	canonical.SHA256 = ""
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode mapping: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Seal records the digest of the exact reviewed mapping shape.
func (m *Mapping) Seal() error {
	if m == nil {
		return fmt.Errorf("nil mapping")
	}
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	m.SHA256 = digest
	return nil
}

// TargetFor maps an issue reference only when it is in the approved allowlist.
func (m Mapping) TargetFor(sourceID string) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	for _, pair := range m.Pairs {
		if pair.Source == sourceID {
			return pair.Target, nil
		}
	}
	return "", fmt.Errorf("issue reference %q is not in the approved mapping", sourceID)
}

// SourceIDs returns source IDs in canonical mapping order.
func (m Mapping) SourceIDs() ([]string, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	canonical := m.Canonical()
	ids := make([]string, 0, len(canonical.Pairs))
	for _, pair := range canonical.Pairs {
		ids = append(ids, pair.Source)
	}
	return ids, nil
}

// TargetIDs returns target IDs in canonical mapping order.
func (m Mapping) TargetIDs() ([]string, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	canonical := m.Canonical()
	ids := make([]string, 0, len(canonical.Pairs))
	for _, pair := range canonical.Pairs {
		ids = append(ids, pair.Target)
	}
	return ids, nil
}
