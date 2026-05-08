package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestPolicyCRD_HasExpectedXValidationRules asserts the generated CRD
// continues to carry the CEL validation rules we depend on. It does NOT
// execute the CEL (that would require envtest) — it just locks in the
// rules' presence so a stray controller-gen run, a removed marker, or a
// mistaken `make manifests` doesn't quietly delete them.
//
// Each entry is a substring expected to appear in the CRD YAML at least once.
// They were added in policy_types.go via // +kubebuilder:validation:XValidation.
func TestPolicyCRD_HasExpectedXValidationRules(t *testing.T) {
	// YAML formatter wraps long messages over multiple lines, so match short
	// unique fragments rather than the full message.
	rules := []string{
		// ResourceRequestsConfig: minAllowed <= maxAllowed
		`minAllowed must be less than or equal to maxAllowed`,
		// ResourceRequestsConfig: keepRequest exclusivity (message prefix +
		// the CEL rule itself)
		`keepRequest cannot be combined with headroom`,
		`!(has(self.keepRequest) && self.keepRequest)`,
		// ResourceLimitsConfig: at most one limit strategy
		`at most one of equalsToRequest, keepLimit`,
	}

	crdPath := findCRD(t, "k8s.sustain.io_policies.yaml")
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read crd: %v", err)
	}
	body := string(raw)
	for _, rule := range rules {
		if !strings.Contains(body, rule) {
			t.Errorf("CRD %s missing expected validation rule: %q\nIf you intentionally removed this rule, update this test.", crdPath, rule)
		}
	}

	// Also sanity-check the generated YAML is parseable — guards against a
	// merge accident producing invalid YAML in the bases directory.
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("CRD YAML is not parseable: %v", err)
	}
}

// TestPolicyCRD_PercentileBounds asserts the percentile field accepts the
// full 1–100 range. Tightening this without a deliberate change would
// silently reject existing Policies, so lock it in.
func TestPolicyCRD_PercentileBounds(t *testing.T) {
	crdPath := findCRD(t, "k8s.sustain.io_policies.yaml")
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read crd: %v", err)
	}
	body := string(raw)
	// The percentile field lives under requests; expect "minimum: 1" and
	// "maximum: 100" to appear adjacent to the percentile description.
	if !strings.Contains(body, "maximum: 100") {
		t.Error("percentile maximum: 100 missing from CRD; did you cap percentile below 100?")
	}
	if !strings.Contains(body, "minimum: 1") {
		t.Error("percentile minimum: 1 missing from CRD")
	}
}

// findCRD walks up from this file's directory looking for config/crd/bases.
// Robust to running tests from any working dir.
func findCRD(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 6 {
		candidate := filepath.Join(dir, "config", "crd", "bases", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate config/crd/bases/%s relative to %s", name, dir)
	return ""
}
