package signalcontract

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

var (
	annotationSelectors = regexp.MustCompile(`kube_pod_annotations\s*\{((?:[^}"]|"(?:[^"\\]|\\.)*")*)\}`)
	ownerNameMatcher    = regexp.MustCompile(`^annotation_k8s_sustain_io_owner_name=~"([^"\\]*)"$`)
	ownerKindLiterals   = regexp.MustCompile(`"owner_kind"\s*,\s*"([^"]*)"\s*,\s*""\s*,\s*""`)
)

// ownerlessKinds lists the literal owner kinds k8s_sustain:pod_workload
// writes, which only a pod without a controller gets: every other pod keeps
// the kind its controller resolves to.
func ownerlessKinds(c chart) []string {
	var out []string
	for _, d := range c.rules[promclient.MetricPodWorkload] {
		for _, m := range ownerKindLiterals.FindAllStringSubmatch(d.Expr, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// ownerNamePatterns lists, sorted and deduplicated, the regex every rule
// selects kube_pod_annotations with, reporting any other selection.
func ownerNamePatterns(t *testing.T, c chart) []string {
	t.Helper()
	var out []string
	for _, name := range slices.Sorted(maps.Keys(c.rules)) {
		for _, d := range c.rules[name] {
			for _, m := range annotationSelectors.FindAllStringSubmatch(d.Expr, -1) {
				sub := ownerNameMatcher.FindStringSubmatch(strings.TrimSpace(m[1]))
				if sub == nil {
					t.Errorf("%s selects kube_pod_annotations{%s}, want exactly annotation_k8s_sustain_io_owner_name=~\"...\"", name, m[1])
					continue
				}
				out = append(out, sub[1])
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ownerNameValues are annotation values on both sides of every rule a label
// value follows: every string of up to three characters from an alphabet of
// valid and invalid ones, and the values around the 63-character limit.
func ownerNameValues() []string {
	alphabet := []string{"a", "Z", "0", "-", "_", ".", "/", " ", "é"}
	values := []string{""}
	level := []string{""}
	for range 3 {
		var next []string
		for _, prefix := range level {
			for _, c := range alphabet {
				next = append(next, prefix+c)
			}
		}
		values = append(values, next...)
		level = next
	}
	for _, n := range []int{61, 62, 63, 64} {
		values = append(values,
			strings.Repeat("a", n),
			"a"+strings.Repeat("-", n-2)+"a",
			"a"+strings.Repeat("-", n-1),
		)
	}
	return append(values, "etl-daily", "Invalid/Value", "a\n", "\na")
}

// The recording rules apply the owner-name override to exactly the
// annotation values the controller does, so a pod's usage reaches the
// identity the controller groups it under: the bundled kube-state-metrics
// exports the annotation, every rule that excludes or maps an override
// selects it with one regex, which Prometheus anchors at both ends, and it
// matches a value exactly when workload.ApplyOwnerNameOverride accepts it. A
// pod without a controller gets the one kind the controller gives it; every
// other pod keeps its controller's kind on both sides, which the promtool
// fixture pins for the rules.
func TestRecordingRulesApplyTheOwnerNameOverrideWhereTheControllerDoes(t *testing.T) {
	c := loadChart(t)
	if !slices.Contains(c.podAnnotations, sustainv1alpha1.OwnerNameAnnotation) {
		t.Errorf("prometheus.kube-state-metrics.metricAnnotationsAllowList exports pod annotations %q, want %s among them",
			c.podAnnotations, sustainv1alpha1.OwnerNameAnnotation)
	}
	for i, d := range c.rules[promclient.MetricPodWorkload] {
		if !annotationSelectors.MatchString(d.Expr) {
			t.Errorf("definition %d of %s does not read the owner-name override from kube_pod_annotations", i, promclient.MetricPodWorkload)
		}
	}
	patterns := ownerNamePatterns(t, c)
	if len(patterns) != 1 {
		t.Fatalf("owner-name regexes = %q, want exactly one", patterns)
	}
	re := regexp.MustCompile(`^(?s:` + patterns[0] + `)$`)
	for _, v := range ownerNameValues() {
		kind, _ := workload.ApplyOwnerNameOverride("", "", map[string]string{sustainv1alpha1.OwnerNameAnnotation: v})
		if controller, rules := kind != "", re.MatchString(v); controller != rules {
			t.Errorf("owner-name %q: controller applies the override = %v, recording rules = %v", v, controller, rules)
		}
	}
	kind, _ := workload.ApplyOwnerNameOverride("", "", map[string]string{sustainv1alpha1.OwnerNameAnnotation: "etl-daily"})
	if written := ownerlessKinds(c); !slices.Equal(written, []string{kind}) {
		t.Errorf("%s gives an overridden pod without a controller owner_kind %q, want exactly %q as the controller does",
			promclient.MetricPodWorkload, written, kind)
	}
}
