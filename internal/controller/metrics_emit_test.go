package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/noony/k8s-sustain/internal/workload"
)

// seriesForWorkload returns all metrics in the named family that match the given
// (namespace, owner_kind, owner_name) triple, regardless of any other label values.
func seriesForWorkload(t *testing.T, name, namespace, kind, workload string) []*dto.Metric {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []*dto.Metric
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] == namespace && labels["owner_kind"] == kind && labels["owner_name"] == workload {
				out = append(out, m)
			}
		}
	}
	return out
}

func TestEmitWorkloadMetricsSetsExpectedValues(t *testing.T) {
	rec := WorkloadMetrics{
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "web",
		Policy:    "p",
		Containers: []ContainerMetric{
			{Name: "app", HasCPU: true, RecommendedCPUCores: 0.25, CurrentCPUCores: 0.5, HasMemory: true, RecommendedMemoryBytes: 200_000_000, CurrentMemoryBytes: 400_000_000},
		},
	}
	EmitWorkloadMetrics(rec)

	cpu := gaugeValue(t, "k8s_sustain_recommended_cpu_cores", map[string]string{
		"namespace": "default", "owner_kind": "Deployment", "owner_name": "web", "container": "app", "policy": "p",
	})
	if cpu != 0.25 {
		t.Errorf("cpu rec: got %v want 0.25", cpu)
	}
	driftCPU := gaugeValue(t, "k8s_sustain_workload_drift_ratio", map[string]string{
		"namespace": "default", "owner_kind": "Deployment", "owner_name": "web", "container": "app", "resource": "cpu",
	})
	if driftCPU != 0.5 {
		t.Errorf("cpu drift: got %v want 0.5 (rec/current)", driftCPU)
	}
}

func gaugeValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			if matchesLabels(m, labels) {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %q with labels %v not found", name, labels)
	return 0
}

func matchesLabels(m *dto.Metric, want map[string]string) bool {
	if len(m.Label) != len(want) {
		return false
	}
	for _, l := range m.Label {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

func TestEmitRetryStateClearsAllReasons(t *testing.T) {
	// Use a unique workload name so this test does not collide with other
	// tests touching the global registry.
	const ns, kind, name = "ns", "Deployment", "clear-test"

	EmitRetryState(ns, kind, name, "prometheus", true)
	EmitRetryState(ns, kind, name, "patch", true)

	// Sanity check: both reason variants should be present at this point.
	if got := len(seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, kind, name)); got != 2 {
		t.Fatalf("expected 2 retry_state series before clear, got %d", got)
	}

	// Clear: should remove every reason variant for this workload.
	EmitRetryState(ns, kind, name, "", false)

	if got := len(seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, kind, name)); got != 0 {
		t.Errorf("expected 0 retry_state series after clear, got %d", got)
	}
}

func TestEmitAutoscalerPresentReplacesPriorKind(t *testing.T) {
	const ns, kind, name = "ns", "Deployment", "kind-test"

	EmitAutoscalerPresent(ns, kind, name, "HPA")
	EmitAutoscalerPresent(ns, kind, name, "KEDA")

	series := seriesForWorkload(t, "k8s_sustain_autoscaler_present", ns, kind, name)
	if len(series) != 1 {
		t.Fatalf("expected exactly one series, got %d", len(series))
	}
}

func TestEmitAutoscalerPresentNoneClearsSeries(t *testing.T) {
	const ns, kind, name = "ns", "Deployment", "none-test"

	EmitAutoscalerPresent(ns, kind, name, "HPA")
	EmitAutoscalerPresent(ns, kind, name, "None")

	series := seriesForWorkload(t, "k8s_sustain_autoscaler_present", ns, kind, name)
	if len(series) != 0 {
		t.Errorf("expected series cleared on None, got %d", len(series))
	}
}

func TestEmitCoordinationFactor(t *testing.T) {
	EmitCoordinationFactor("ns", "Deployment", "w", "cpu", "overhead", 1.57)
	val := testutil.ToFloat64(coordinationFactor.With(prometheus.Labels{
		"namespace": "ns", "owner_kind": "Deployment", "owner_name": "w",
		"resource": "cpu", "kind": "overhead",
	}))
	if got, want := val, 1.57; got < want-1e-6 || got > want+1e-6 {
		t.Errorf("coordination factor: got %v, want %v", got, want)
	}
}

func TestIncrementRetryAttempt(t *testing.T) {
	const ns, kind, name = "ns", "Deployment", "retry-attempt"

	IncrementRetryAttempt(ns, kind, name)
	IncrementRetryAttempt(ns, kind, name)
	IncrementRetryAttempt(ns, kind, name)

	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got float64
	for _, mf := range mfs {
		if mf.GetName() != "k8s_sustain_workload_retry_attempts" {
			continue
		}
		for _, m := range mf.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] == ns && labels["owner_kind"] == kind && labels["owner_name"] == name {
				got = m.GetCounter().GetValue()
			}
		}
	}
	if got != 3 {
		t.Errorf("retry_attempts_total = %v, want 3", got)
	}
}

func TestEmitPolicyRollup(t *testing.T) {
	const policyName = "rollup-test"
	EmitPolicyRollup(policyName, 7, 2)

	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]float64{
		"k8s_sustain_policy_workload_count": 7,
		"k8s_sustain_policy_at_risk_count":  2,
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if w, ok := want[mf.GetName()]; ok {
			_ = w
			for _, m := range mf.Metric {
				for _, l := range m.Label {
					if l.GetName() == "policy" && l.GetValue() == policyName {
						got[mf.GetName()] = m.GetGauge().GetValue()
					}
				}
			}
		}
	}
	for n, v := range want {
		if got[n] != v {
			t.Errorf("%s = %v, want %v", n, got[n], v)
		}
	}
}

func TestEmitAutoscalerTargetsConfigured_ClearsAndSets(t *testing.T) {
	const ns, kind, name = "ns", "Deployment", "ats-test"

	EmitAutoscalerTargetsConfigured(ns, kind, name, "HPA", map[string]int32{"cpu": 70, "memory": 80})
	if got := len(seriesForWorkload(t, "k8s_sustain_autoscaler_target_configured", ns, kind, name)); got != 2 {
		t.Fatalf("expected 2 series after first emit, got %d", got)
	}

	// Drop memory trigger — should leave only cpu.
	EmitAutoscalerTargetsConfigured(ns, kind, name, "HPA", map[string]int32{"cpu": 70})
	got := seriesForWorkload(t, "k8s_sustain_autoscaler_target_configured", ns, kind, name)
	if len(got) != 1 {
		t.Fatalf("expected 1 series after dropping memory, got %d", len(got))
	}
	for _, l := range got[0].Label {
		if l.GetName() == "resource" && l.GetValue() != "cpu" {
			t.Errorf("expected only cpu resource left, got %v", l.GetValue())
		}
	}

	// Kind=None clears all.
	EmitAutoscalerTargetsConfigured(ns, kind, name, "None", nil)
	if got := len(seriesForWorkload(t, "k8s_sustain_autoscaler_target_configured", ns, kind, name)); got != 0 {
		t.Errorf("expected 0 series after None, got %d", got)
	}
}

func TestContainerRequestCPUCores(t *testing.T) {
	none := corev1.Container{}
	if got := containerRequestCPUCores(none); got != 0 {
		t.Errorf("no requests should be 0, got %v", got)
	}

	c := corev1.Container{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
	}}
	if got := containerRequestCPUCores(c); got != 0.25 {
		t.Errorf("250m → %v, want 0.25", got)
	}
}

func TestContainerRequestMemoryBytes(t *testing.T) {
	none := corev1.Container{}
	if got := containerRequestMemoryBytes(none); got != 0 {
		t.Errorf("no requests should be 0, got %v", got)
	}

	c := corev1.Container{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}}
	if got, want := containerRequestMemoryBytes(c), float64(128*1024*1024); got != want {
		t.Errorf("128Mi → %v, want %v", got, want)
	}
}

// TestEmitWorkloadFromRecs_HappyPath drives emitWorkloadFromRecs and verifies
// the resulting recommended/template gauges and drift ratio for a workload
// with both CPU and memory recommendations.
func TestEmitWorkloadFromRecs_HappyPath(t *testing.T) {
	const ns, kind, name, container = "ns", "Deployment", "rec-test", "app"

	t1 := &workloadTarget{
		Namespace: ns, Kind: kind, Name: name,
		Containers: []corev1.Container{{
			Name: container,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		}},
	}
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")
	recs := map[string]workload.ContainerRecommendation{
		container: {CPURequest: &cpu, MemoryRequest: &mem},
	}
	emitWorkloadFromRecs(t1, "p", recs)

	// recommended cpu should be 0.25
	if got := gaugeValue(t, "k8s_sustain_recommended_cpu_cores", map[string]string{
		"namespace": ns, "owner_kind": kind, "owner_name": name, "container": container, "policy": "p",
	}); got != 0.25 {
		t.Errorf("recommended cpu = %v, want 0.25", got)
	}
	// drift ratio cpu = 0.25 / 0.5 = 0.5
	if got := gaugeValue(t, "k8s_sustain_workload_drift_ratio", map[string]string{
		"namespace": ns, "owner_kind": kind, "owner_name": name, "container": container, "resource": "cpu",
	}); got != 0.5 {
		t.Errorf("cpu drift = %v, want 0.5", got)
	}
}

// TestEmitWorkloadFromRecs_EmptyRecsIsNoOp verifies that when no container
// has a recommendation, the emitter does nothing — important to avoid
// emitting zero-valued gauges that would corrupt dashboards.
func TestEmitWorkloadFromRecs_EmptyRecsIsNoOp(t *testing.T) {
	t1 := &workloadTarget{
		Namespace: "ns", Kind: "Deployment", Name: "no-recs",
		Containers: []corev1.Container{{Name: "app"}},
	}
	emitWorkloadFromRecs(t1, "p", map[string]workload.ContainerRecommendation{})
	if got := len(seriesForWorkload(t, "k8s_sustain_recommended_cpu_cores", "ns", "Deployment", "no-recs")); got != 0 {
		t.Errorf("expected no series emitted for empty recs, got %d", got)
	}
}
