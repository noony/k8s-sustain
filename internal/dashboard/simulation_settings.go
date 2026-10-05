package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// simulationSettings are the settings of a Policy's right-sizing spec that
// shape a Recommendation, as a Simulation runs with them and overrides them;
// how a Recommendation is applied is no part of them.
type simulationSettings struct {
	ResourcesConfigs       sustainv1alpha1.ResourcesConfigs       `json:"resourcesConfigs"`
	AutoscalerCoordination sustainv1alpha1.AutoscalerCoordination `json:"autoscalerCoordination"`
	ExcludeInitContainers  bool                                   `json:"excludeInitContainers"`
}

func settingsOf(spec sustainv1alpha1.RightSizingSpec) simulationSettings {
	return simulationSettings{
		ResourcesConfigs:       spec.ResourcesConfigs,
		AutoscalerCoordination: spec.AutoscalerCoordination,
		ExcludeInitContainers:  spec.ExcludeInitContainers,
	}
}

func (s simulationSettings) rightSizing() sustainv1alpha1.RightSizingSpec {
	return sustainv1alpha1.RightSizingSpec{
		ResourcesConfigs:       s.ResourcesConfigs,
		AutoscalerCoordination: s.AutoscalerCoordination,
		ExcludeInitContainers:  s.ExcludeInitContainers,
	}
}

// settingsError is an override the Policy CRD would reject, attributed to the
// field it is about.
type settingsError struct {
	field, msg string
}

func (e *settingsError) Error() string { return e.field + ": " + e.msg }

// quantityFields are the paths, under a resource, of the settings a Kubernetes
// quantity decodes: a malformed quantity fails its decode without naming the
// field.
var quantityFields = [][]string{
	{"requests", "minAllowed"},
	{"requests", "maxAllowed"},
	{"downsizeThreshold", "minDecrease"},
}

// override merges patch, a JSON merge patch (RFC 7386) of simulationSettings,
// onto s: a key it sets replaces the base's, and a null removes it. Settings
// it changes are validated as the Policy CRD validates them; a patch that
// changes nothing leaves s as the apiserver admitted it, which an older
// object can have been under rules the CRD has since gained.
func (s simulationSettings) override(patch json.RawMessage) (simulationSettings, error) {
	if len(bytes.TrimSpace(patch)) == 0 || bytes.Equal(bytes.TrimSpace(patch), []byte("null")) {
		return s, nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(patch), []byte("{")) {
		return s, &settingsError{field: "override", msg: "must be a JSON object"}
	}
	base, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	merged, err := jsonpatch.MergePatch(base, patch)
	if err != nil {
		return s, &settingsError{field: "override", msg: err.Error()}
	}
	if err := checkQuantities(merged); err != nil {
		return s, err
	}
	var out simulationSettings
	dec := json.NewDecoder(bytes.NewReader(merged))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return s, &settingsError{field: "override", msg: err.Error()}
	}
	if equality.Semantic.DeepEqual(out, s) {
		return s, nil
	}
	return out, out.validate()
}

// checkQuantities rejects a malformed quantity in doc, naming its field.
func checkQuantities(doc []byte) error {
	var settings struct {
		ResourcesConfigs map[string]map[string]any `json:"resourcesConfigs"`
	}
	if err := json.Unmarshal(doc, &settings); err != nil {
		return &settingsError{field: "override.resourcesConfigs", msg: err.Error()}
	}
	for _, res := range []string{"cpu", "memory"} {
		for _, path := range quantityFields {
			parent, _ := settings.ResourcesConfigs[res][path[0]].(map[string]any)
			v, ok := parent[path[1]].(string)
			if !ok {
				continue
			}
			if _, err := resource.ParseQuantity(v); err != nil {
				field := strings.Join(append([]string{"resourcesConfigs", res}, path...), ".")
				return &settingsError{field: field, msg: fmt.Sprintf("invalid quantity %q", v)}
			}
		}
	}
	return nil
}

// validate checks s against the Policy CRD's rules on these settings. They
// are copied from api/v1alpha1's markers; a test holds the copy to every rule
// the generated CRD declares.
func (s simulationSettings) validate() error {
	for _, r := range []struct {
		name string
		cfg  sustainv1alpha1.ResourceConfig
	}{{"cpu", s.ResourcesConfigs.CPU}, {"memory", s.ResourcesConfigs.Memory}} {
		if err := validateResource("resourcesConfigs."+r.name, r.cfg); err != nil {
			return err
		}
	}
	if a := s.AutoscalerCoordination.ReplicaBudgetAnchor; a != nil && (*a < 0 || *a > 1) {
		return &settingsError{field: "autoscalerCoordination.replicaBudgetAnchor", msg: fmt.Sprintf("%v must be between 0 and 1", *a)}
	}
	return nil
}

func validateResource(path string, c sustainv1alpha1.ResourceConfig) error {
	if c.Window != "" && !windowPattern.MatchString(c.Window) {
		return &settingsError{field: path + ".window", msg: fmt.Sprintf("%q must be a duration like 168h or 14d, with no sub-minute unit", c.Window)}
	}
	req := c.Requests
	if p := req.Percentile; p != nil && (*p < 1 || *p > 100) {
		return &settingsError{field: path + ".requests.percentile", msg: fmt.Sprintf("%d must be between 1 and 100", *p)}
	}
	if h := req.Headroom; h != nil && (*h < 0 || *h > 100) {
		return &settingsError{field: path + ".requests.headroom", msg: fmt.Sprintf("%d must be between 0 and 100", *h)}
	}
	if req.MinAllowed != nil && req.MaxAllowed != nil && req.MinAllowed.Cmp(*req.MaxAllowed) > 0 {
		return &settingsError{field: path + ".requests", msg: "minAllowed must be less than or equal to maxAllowed"}
	}
	if req.KeepRequest && (req.Headroom != nil || req.Percentile != nil || req.MinAllowed != nil || req.MaxAllowed != nil) {
		return &settingsError{field: path + ".requests", msg: "keepRequest cannot be combined with headroom, percentile, minAllowed, or maxAllowed (they have no effect when the request is kept)"}
	}
	l := c.Limits
	if r := l.RequestsLimitsRatio; r != nil && *r < 1 {
		return &settingsError{field: path + ".limits.requestsLimitsRatio", msg: fmt.Sprintf("%v must be at least 1", *r)}
	}
	strategies := 0
	for _, set := range []bool{l.EqualsToRequest, l.KeepLimit, l.KeepLimitRequestRatio, l.NoLimit, l.RequestsLimitsRatio != nil} {
		if set {
			strategies++
		}
	}
	if strategies > 1 {
		return &settingsError{field: path + ".limits", msg: "at most one of equalsToRequest, keepLimit, keepLimitRequestRatio, noLimit, requestsLimitsRatio may be set"}
	}
	if d := c.DownsizeThreshold; d != nil && d.Percent != nil && (*d.Percent < 0 || *d.Percent > 100) {
		return &settingsError{field: path + ".downsizeThreshold.percent", msg: fmt.Sprintf("%d must be between 0 and 100", *d.Percent)}
	}
	return nil
}
