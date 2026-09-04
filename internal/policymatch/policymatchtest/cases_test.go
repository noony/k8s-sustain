package policymatchtest

import (
	"testing"

	"github.com/noony/k8s-sustain/internal/policymatch"
)

// The table is only worth anything if it agrees with the resolver itself:
// without this, fixture drift would silently weaken all three component tests
// that consume it rather than failing anything.
func TestAnnotationCasesAgreeWithResolvePolicy(t *testing.T) {
	cases := AnnotationCases()
	if len(cases) < 6 {
		t.Fatalf("expected the shared contract table to carry at least 6 cases, got %d", len(cases))
	}
	for _, c := range cases {
		gotName, gotLevel := policymatch.ResolvePolicy(c.Template, c.Workload, c.Namespace)
		if gotName != c.WantPolicy || gotLevel != c.WantLevel {
			t.Errorf("case %q: ResolvePolicy() = (%q, %q), want (%q, %q)",
				c.Name, gotName, gotLevel, c.WantPolicy, c.WantLevel)
		}
	}
}
