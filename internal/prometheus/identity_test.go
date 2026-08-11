package prometheus

import (
	"testing"

	"github.com/prometheus/common/model"
)

func TestVectorToIdentityValuesGroupsByFullIdentity(t *testing.T) {
	vec := model.Vector{
		identitySample("prod", "Deployment", "api", "app", 0.42),
		identitySample("prod", "Deployment", "api", "sidecar", 0.03),
		identitySample("prod", "Deployment", "web", "app", 1.2),
		identitySample("staging", "Deployment", "api", "app", 0.1),
	}

	out := vectorToIdentityValues(vec)

	if len(out) != 3 {
		t.Fatalf("got %d identities want 3: %v", len(out), out)
	}
	prodAPI := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	if got := out[prodAPI]["app"]; got != 0.42 {
		t.Fatalf("prod/api app: got %v want 0.42", got)
	}
	if got := out[prodAPI]["sidecar"]; got != 0.03 {
		t.Fatalf("prod/api sidecar: got %v want 0.03", got)
	}
	// The collision case this type exists to prevent: same owner_name and
	// container, different namespace.
	stagingAPI := WorkloadIdentity{Namespace: "staging", OwnerKind: "Deployment", OwnerName: "api"}
	if got := out[stagingAPI]["app"]; got != 0.1 {
		t.Fatalf("staging/api app: got %v want 0.1 (must not collide with prod/api)", got)
	}
}

func TestVectorToIdentityValuesDropsIncompleteSeries(t *testing.T) {
	vec := model.Vector{
		identitySample("prod", "Deployment", "api", "", 1),   // no container
		identitySample("", "Deployment", "api", "app", 1),    // no namespace
		identitySample("prod", "", "api", "app", 1),          // no owner_kind
		identitySample("prod", "Deployment", "", "app", 1),   // no owner_name
		identitySample("prod", "Deployment", "ok", "app", 7), // the only valid one
	}
	out := vectorToIdentityValues(vec)
	if len(out) != 1 {
		t.Fatalf("got %d identities want 1: %v", len(out), out)
	}
	valid := WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "ok"}
	if got := out[valid]["app"]; got != 7 {
		t.Fatalf("valid series: got %v want 7", got)
	}
}

func identitySample(ns, kind, name, container string, v float64) *model.Sample {
	m := model.Metric{}
	if ns != "" {
		m["namespace"] = model.LabelValue(ns)
	}
	if kind != "" {
		m["owner_kind"] = model.LabelValue(kind)
	}
	if name != "" {
		m["owner_name"] = model.LabelValue(name)
	}
	if container != "" {
		m["container"] = model.LabelValue(container)
	}
	return &model.Sample{Metric: m, Value: model.SampleValue(v)}
}
