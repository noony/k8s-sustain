package dashboard

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// invalidOverrides breaks one rule each, written for a resource {r}: the
// rule as the CRD names it (its path, then the keyword or the message it
// fails with) and an override that breaks it.
var invalidOverrides = []struct {
	rule     string
	override string
}{
	{"resourcesConfigs.{r}.window:pattern", `{"window":"7 days"}`},
	{"resourcesConfigs.{r}.requests.percentile:minimum", `{"requests":{"percentile":0}}`},
	{"resourcesConfigs.{r}.requests.percentile:maximum", `{"requests":{"percentile":101}}`},
	{"resourcesConfigs.{r}.requests.headroom:minimum", `{"requests":{"headroom":-1}}`},
	{"resourcesConfigs.{r}.requests.headroom:maximum", `{"requests":{"headroom":101}}`},
	{"resourcesConfigs.{r}.requests.minAllowed:quantity", `{"requests":{"minAllowed":"a lot"}}`},
	{"resourcesConfigs.{r}.requests.maxAllowed:quantity", `{"requests":{"maxAllowed":"a lot"}}`},
	{
		"resourcesConfigs.{r}.requests:minAllowed must be less than or equal to maxAllowed",
		`{"requests":{"minAllowed":"2","maxAllowed":"1"}}`,
	},
	{
		"resourcesConfigs.{r}.requests:keepRequest cannot be combined with headroom, percentile, minAllowed, or maxAllowed (they have no effect when the request is kept)",
		`{"requests":{"keepRequest":true}}`,
	},
	{"resourcesConfigs.{r}.limits.requestsLimitsRatio:minimum", `{"limits":{"keepLimit":null,"requestsLimitsRatio":0.5}}`},
	{
		"resourcesConfigs.{r}.limits:at most one of equalsToRequest, keepLimit, keepLimitRequestRatio, noLimit, requestsLimitsRatio may be set",
		`{"limits":{"noLimit":true}}`,
	},
	{"resourcesConfigs.{r}.downsizeThreshold.percent:minimum", `{"downsizeThreshold":{"percent":-1}}`},
	{"resourcesConfigs.{r}.downsizeThreshold.percent:maximum", `{"downsizeThreshold":{"percent":101}}`},
	{"resourcesConfigs.{r}.downsizeThreshold.minDecrease:quantity", `{"downsizeThreshold":{"minDecrease":"a lot"}}`},
}

// baseSettings is a Policy's settings that pass every rule, with a percentile
// for keepRequest to clash with and a limit strategy for another to clash
// with.
func baseSettings() simulationSettings {
	rc := sustainv1alpha1.ResourceConfig{
		Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](95)},
		Limits:   sustainv1alpha1.ResourceLimitsConfig{KeepLimit: true},
	}
	return simulationSettings{ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{CPU: rc, Memory: *rc.DeepCopy()}}
}

// An override the Policy CRD would reject is rejected once merged, naming the
// field it breaks.
func TestOverrideRejectsWhatTheCRDRejects(t *testing.T) {
	cases := map[string]string{
		"autoscalerCoordination.replicaBudgetAnchor:minimum": `{"autoscalerCoordination":{"replicaBudgetAnchor":-0.1}}`,
		"autoscalerCoordination.replicaBudgetAnchor:maximum": `{"autoscalerCoordination":{"replicaBudgetAnchor":1.5}}`,
	}
	for _, r := range []string{"cpu", "memory"} {
		for _, c := range invalidOverrides {
			cases[strings.ReplaceAll(c.rule, "{r}", r)] = `{"resourcesConfigs":{"` + r + `":` + c.override + `}}`
		}
	}
	for rule, override := range cases {
		t.Run(rule, func(t *testing.T) {
			_, err := baseSettings().override(json.RawMessage(override))

			var invalid *settingsError
			if !errors.As(err, &invalid) {
				t.Fatalf("override %s = %v, want it rejected", override, err)
			}
			if field := strings.SplitN(rule, ":", 2)[0]; invalid.field != field {
				t.Errorf("rejected on %q (%v), want %q", invalid.field, err, field)
			}
		})
	}
}

// Every rule the generated Policy CRD declares on these settings has a case
// above: a rule added to the API types without its copy in violations fails
// here, and so does a kind of rule this test cannot read.
func TestOverrideRulesCoverTheCRD(t *testing.T) {
	raw, err := os.ReadFile("../../charts/k8s-sustain/files/crds/k8s.sustain.io_policies.yaml")
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	rightSizing := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, p := range []string{"spec", "rightSizing"} {
		rightSizing = rightSizing["properties"].(map[string]any)[p].(map[string]any)
	}
	declared := map[string]bool{}
	for _, field := range []string{"resourcesConfigs", "autoscalerCoordination", "excludeInitContainers"} {
		schemaRules(t, field, rightSizing["properties"].(map[string]any)[field].(map[string]any), declared)
	}

	covered := map[string]bool{
		"autoscalerCoordination.replicaBudgetAnchor:minimum": true,
		"autoscalerCoordination.replicaBudgetAnchor:maximum": true,
	}
	for _, r := range []string{"cpu", "memory"} {
		for _, c := range invalidOverrides {
			covered[strings.ReplaceAll(c.rule, "{r}", r)] = true
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(declared)) {
		if !covered[rule] {
			t.Errorf("the CRD declares %q, which no override case breaks", rule)
		}
	}
	if len(declared) < 20 {
		t.Errorf("found %d rules in the CRD, too few: is the schema walk still reading it?", len(declared))
	}
}

// schemaRules adds, under path, every rule an OpenAPI schema node and its
// properties declare to out, as path:keyword or path:message. A pattern on an
// int-or-string is a quantity's; a format is enforced by the Go type a setting
// decodes into.
func schemaRules(t *testing.T, path string, node map[string]any, out map[string]bool) {
	t.Helper()
	for key, v := range node {
		switch key {
		case "type", "description", "format", "anyOf", "x-kubernetes-int-or-string":
		case "minimum", "maximum":
			out[path+":"+key] = true
		case "pattern":
			if node["x-kubernetes-int-or-string"] == true {
				out[path+":quantity"] = true
			} else {
				out[path+":pattern"] = true
			}
		case "x-kubernetes-validations":
			for _, rule := range v.([]any) {
				out[path+":"+rule.(map[string]any)["message"].(string)] = true
			}
		case "properties":
			for name, p := range v.(map[string]any) {
				schemaRules(t, path+"."+name, p.(map[string]any), out)
			}
		default:
			t.Errorf("%s declares %q, which this test does not read: teach it the rule", path, key)
		}
	}
}

// An override is a JSON merge patch onto the base: what it sets replaces the
// base's, false and null clear what the base sets, and the rest is kept.
func TestOverrideMergesOntoTheBase(t *testing.T) {
	base := baseSettings()
	base.ResourcesConfigs.Memory.Requests.MinAllowed = ptr.To(resource.MustParse("64Mi"))
	base.AutoscalerCoordination.Enabled = true

	got, err := base.override(json.RawMessage(`{
		"resourcesConfigs": {
			"cpu": {"requests": {"percentile": 90}, "limits": {"keepLimit": false, "noLimit": true}},
			"memory": {"requests": {"minAllowed": null}}
		},
		"excludeInitContainers": true
	}`))
	if err != nil {
		t.Fatalf("override: %v", err)
	}

	cpu, mem := got.ResourcesConfigs.CPU, got.ResourcesConfigs.Memory
	if *cpu.Requests.Percentile != 90 || !cpu.Limits.NoLimit || cpu.Limits.KeepLimit {
		t.Errorf("cpu = %+v, want the 90th percentile and noLimit in place of keepLimit", cpu)
	}
	if mem.Requests.MinAllowed != nil || *mem.Requests.Percentile != 95 || !mem.Limits.KeepLimit {
		t.Errorf("memory = %+v, want minAllowed removed and the rest of the base kept", mem)
	}
	if !got.ExcludeInitContainers || !got.AutoscalerCoordination.Enabled {
		t.Errorf("got %+v, want excludeInitContainers set and the base's coordination kept", got)
	}
}

// A rule the base already broke, admitted before the CRD gained it, is the
// Policy's: an override that leaves it as it is, changing nothing or
// something else, is accepted, and one that breaks a rule of its own is not.
// A malformed override is rejected as the override's.
func TestOverrideShape(t *testing.T) {
	admitted := baseSettings()
	admitted.ResourcesConfigs.CPU.Requests.MinAllowed = ptr.To(resource.MustParse("2"))
	admitted.ResourcesConfigs.CPU.Requests.MaxAllowed = ptr.To(resource.MustParse("1"))
	for _, accepted := range []string{"", "null", " {} ", `{"resourcesConfigs":{"cpu":{"requests":{"percentile":90}}}}`} {
		if got, err := admitted.override(json.RawMessage(accepted)); err != nil || got.ResourcesConfigs.CPU.Requests.Percentile == nil {
			t.Errorf("override %q = %+v, %v; want it accepted", accepted, got, err)
		}
	}
	var invalid *settingsError
	if _, err := admitted.override(json.RawMessage(`{"resourcesConfigs":{"cpu":{"requests":{"percentile":101}}}}`)); !errors.As(err, &invalid) ||
		invalid.field != "resourcesConfigs.cpu.requests.percentile" {
		t.Errorf("override breaking a rule of its own = %v, want it rejected on the percentile", err)
	}
	for _, bad := range []string{`[1]`, `"x"`, `{"recommendOnly": true}`, `{"resourcesConfigs":{"cpu":{"requests":{"percentil":90}}}}`} {
		var invalid *settingsError
		if _, err := baseSettings().override(json.RawMessage(bad)); !errors.As(err, &invalid) {
			t.Errorf("override %s = %v, want it rejected", bad, err)
		}
	}
}
