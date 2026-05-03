package controller

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestNewMetricsRegistered(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
	}{
		{"k8s_sustain_recommended_cpu_cores", []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"}},
		{"k8s_sustain_recommended_memory_bytes", []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"}},
		{"k8s_sustain_workload_drift_ratio", []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "resource"}},
		{"k8s_sustain_workload_retry_state", []string{"namespace", "owner_kind", "owner_name", "reason"}},
		{"k8s_sustain_workload_retry_attempts", []string{"namespace", "owner_kind", "owner_name"}},
		{"k8s_sustain_policy_workload_count", []string{"policy"}},
		{"k8s_sustain_policy_at_risk_count", []string{"policy"}},
		{"k8s_sustain_autoscaler_present", []string{"namespace", "owner_kind", "owner_name", "kind"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := findMetric(t, tc.name)
			if m == nil {
				t.Fatalf("metric %q not registered", tc.name)
			}
			gotLabels := labelNames(m)
			if !equalSet(gotLabels, tc.labels) {
				t.Fatalf("labels for %q: got %v want %v", tc.name, gotLabels, tc.labels)
			}
		})
	}
}

func findMetric(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := metricsRegistry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

func labelNames(mf *dto.MetricFamily) []string {
	if len(mf.Metric) == 0 {
		return nil
	}
	out := make([]string, 0, len(mf.Metric[0].Label))
	for _, l := range mf.Metric[0].Label {
		out = append(out, l.GetName())
	}
	return out
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func metricsRegistry() interface {
	Gather() ([]*dto.MetricFamily, error)
} {
	return registryForTest
}

func init() {
	recommendedCPUCores.WithLabelValues("ns", "Deployment", "n", "c", "regular", "p").Set(0)
	recommendedMemoryBytes.WithLabelValues("ns", "Deployment", "n", "c", "regular", "p").Set(0)
	workloadDriftRatio.WithLabelValues("ns", "Deployment", "n", "c", "regular", "cpu").Set(1)
	workloadRetryState.WithLabelValues("ns", "Deployment", "n", "test").Set(0)
	workloadRetryAttempts.WithLabelValues("ns", "Deployment", "n").Add(0)
	policyWorkloadCount.WithLabelValues("p").Set(0)
	policyAtRiskCount.WithLabelValues("p").Set(0)
	autoscalerPresent.WithLabelValues("ns", "Deployment", "n", "HPA").Set(0)
	_ = strings.Builder{}
}
