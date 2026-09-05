package scopedbundle

import (
	"strings"
	"testing"
)

var researchSuffixes = []string{
	"001",
	"API-120",
	"Adjudication-100",
	"Campaigns-040",
	"Campaigns-045",
	"Constitution-020",
	"Eval-090",
	"Explorer-070",
	"Infrastructure-130",
	"Ledger-050",
	"Platform-010",
	"Playbook-110",
	"Quality-140",
	"Referee-030",
	"ResearchGraph-060",
	"Validation-150",
	"Workers-080",
}

func syntheticResearchMapping() Mapping {
	pairs := make([]IDPair, 0, len(researchSuffixes))
	for _, suffix := range researchSuffixes {
		pairs = append(pairs, IDPair{
			Source: "source-" + suffix,
			Target: "target-" + suffix,
		})
	}
	m := Mapping{
		Version:       MappingVersion,
		ExpectedCount: len(pairs),
		SourcePrefix:  "source-",
		TargetPrefix:  "target-",
		Pairs:         pairs,
	}
	if err := m.Seal(); err != nil {
		panic(err)
	}
	return m
}

func TestMappingRequiresExactReviewedCardinalityAndUniquePairs(t *testing.T) {
	t.Parallel()

	valid := syntheticResearchMapping()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid mapping: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Mapping)
		want string
	}{
		{
			name: "stale expected count",
			edit: func(m *Mapping) { m.Pairs = m.Pairs[:16] },
			want: "expected 17",
		},
		{
			name: "duplicate source",
			edit: func(m *Mapping) { m.Pairs[1].Source = m.Pairs[0].Source },
			want: "duplicate source",
		},
		{
			name: "duplicate target",
			edit: func(m *Mapping) { m.Pairs[1].Target = m.Pairs[0].Target },
			want: "duplicate target",
		},
		{
			name: "empty source",
			edit: func(m *Mapping) { m.Pairs[0].Source = " " },
			want: "empty source",
		},
		{
			name: "unsupported version",
			edit: func(m *Mapping) { m.Version++ },
			want: "mapping version",
		},
		{
			name: "wildcard source prefix",
			edit: func(m *Mapping) { m.SourcePrefix = "source-%" },
			want: "SQL wildcards",
		},
		{
			name: "source outside prefix",
			edit: func(m *Mapping) { m.Pairs[0].Source = "outside-001" },
			want: "outside source_prefix",
		},
		{
			name: "digest mismatch",
			edit: func(m *Mapping) { m.Pairs[0].Target += "-changed" },
			want: "mapping digest mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := syntheticResearchMapping()
			tc.edit(&m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestMappingAcceptsReviewedSeventeenEighteenAndNineteenButRejectsStaleCounts(t *testing.T) {
	t.Parallel()

	seventeen := syntheticResearchMapping()
	if err := seventeen.Validate(); err != nil {
		t.Fatalf("17-pair manifest: %v", err)
	}

	eighteen := syntheticResearchMapping()
	eighteen.Pairs = append(eighteen.Pairs, IDPair{
		Source: "source-Campaigns-046",
		Target: "target-Campaigns-046",
	})
	eighteen.ExpectedCount = 18
	if err := eighteen.Seal(); err != nil {
		t.Fatalf("seal 18-pair manifest: %v", err)
	}
	if err := eighteen.Validate(); err != nil {
		t.Fatalf("18-pair manifest: %v", err)
	}
	nineteen := eighteen
	nineteen.Pairs = append(nineteen.Pairs, IDPair{
		Source: "source-Campaigns-047",
		Target: "target-Campaigns-047",
	})
	nineteen.ExpectedCount = 19
	if err := nineteen.Seal(); err != nil {
		t.Fatalf("seal 19-pair manifest: %v", err)
	}
	if err := nineteen.Validate(); err != nil {
		t.Fatalf("19-pair manifest: %v", err)
	}

	stale := eighteen
	stale.ExpectedCount = 17
	if err := stale.Validate(); err == nil || !strings.Contains(err.Error(), "expected 17") {
		t.Fatalf("stale 17-vs-18 error = %v", err)
	}
}

func TestMappingResolvesOnlyApprovedReferences(t *testing.T) {
	t.Parallel()

	m := syntheticResearchMapping()
	got, err := m.TargetFor("source-Campaigns-045")
	if err != nil {
		t.Fatalf("TargetFor(mapped): %v", err)
	}
	if got != "target-Campaigns-045" {
		t.Fatalf("TargetFor(mapped) = %q", got)
	}

	if _, err := m.TargetFor("source-outside"); err == nil || !strings.Contains(err.Error(), "not in the approved mapping") {
		t.Fatalf("TargetFor(unmapped) error = %v", err)
	}
}

func TestMappingCanonicalOrderAndDigestIgnoreInputOrder(t *testing.T) {
	t.Parallel()

	a := syntheticResearchMapping()
	b := syntheticResearchMapping()
	for left, right := 0, len(b.Pairs)-1; left < right; left, right = left+1, right-1 {
		b.Pairs[left], b.Pairs[right] = b.Pairs[right], b.Pairs[left]
	}

	aDigest, err := a.Digest()
	if err != nil {
		t.Fatalf("first digest: %v", err)
	}
	bDigest, err := b.Digest()
	if err != nil {
		t.Fatalf("second digest: %v", err)
	}
	if aDigest != bDigest {
		t.Fatalf("mapping digest depends on input order: %s != %s", aDigest, bDigest)
	}

	canonical := b.Canonical()
	for i := 1; i < len(canonical.Pairs); i++ {
		if canonical.Pairs[i-1].Source >= canonical.Pairs[i].Source {
			t.Fatalf("Canonical() is not source-sorted at %d", i)
		}
	}
}
