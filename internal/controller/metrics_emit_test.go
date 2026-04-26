package controller

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
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

func TestEmitHPAPresentReplacesPriorMode(t *testing.T) {
	const ns, kind, name = "ns", "Deployment", "mode-test"

	EmitHPAPresent(ns, kind, name, "HpaAware", true)
	EmitHPAPresent(ns, kind, name, "Ignore", true)

	series := seriesForWorkload(t, "k8s_sustain_hpa_present", ns, kind, name)
	if len(series) != 1 {
		t.Fatalf("expected exactly 1 hpa_present series, got %d", len(series))
	}
	var mode string
	for _, l := range series[0].Label {
		if l.GetName() == "mode" {
			mode = l.GetValue()
		}
	}
	if mode != "Ignore" {
		t.Errorf("expected mode label %q, got %q", "Ignore", mode)
	}
}
