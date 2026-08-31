package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestNewMetricsRegistered(t *testing.T) {
	seedMetricsForRegistrationCheck()

	cases := []struct {
		name   string
		labels []string
	}{
		{"k8s_sustain_recommended_cpu_cores", []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"}},
		{"k8s_sustain_recommended_memory_bytes", []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"}},
		{"k8s_sustain_workload_drift_ratio", []string{"namespace", "owner_kind", "owner_name", "resource"}},
		{"k8s_sustain_workload_retry_state", []string{"namespace", "owner_kind", "owner_name", "reason"}},
		{"k8s_sustain_workload_retry_attempts", []string{"namespace", "owner_kind", "owner_name"}},
		{"k8s_sustain_policy_workload_count", []string{"policy"}},
		{"k8s_sustain_policy_at_risk_count", []string{"policy"}},
		{"k8s_sustain_policy_batch_requested_count", []string{"policy"}},
		{"k8s_sustain_policy_batch_resolved_count", []string{"policy"}},
		{"k8s_sustain_policy_batch_failures_total", []string{"policy"}},
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

// seedMetricsForRegistrationCheck creates one child series per vector asserted
// by TestNewMetricsRegistered.
//
// This is mandatory, not cosmetic: a *Vec with no children is absent from
// Gather() output, so an unseeded vector is indistinguishable from an
// unregistered one. It also has to run inside the test rather than in a
// package init(), because the cleanup paths under test delete series --
// DeletePolicyMetrics wipes every series carrying a given policy label, and
// EmitWorkloadMetrics / EmitAutoscalerPresent drop stale per-workload series.
// Seeding once at init() left the assertions at the mercy of -shuffle: any
// test reconciling a deleted Policy named "p" removed the seeds first and the
// gather below reported the metrics as unregistered.
//
// The label values are deliberately unique to this test so no other test's
// cleanup can match and remove them.
func seedMetricsForRegistrationCheck() {
	const (
		ns     = "metrics-registration-probe-ns"
		name   = "metrics-registration-probe-workload"
		policy = "metrics-registration-probe-policy"
	)
	recommendedCPUCores.WithLabelValues(ns, "Deployment", name, "c", ContainerKindRegular, policy).Set(0)
	recommendedMemoryBytes.WithLabelValues(ns, "Deployment", name, "c", ContainerKindRegular, policy).Set(0)
	workloadDriftRatio.WithLabelValues(ns, "Deployment", name, "cpu").Set(1)
	workloadRetryState.WithLabelValues(ns, "Deployment", name, "test").Set(0)
	workloadRetryAttempts.WithLabelValues(ns, "Deployment", name).Add(0)
	policyWorkloadCount.WithLabelValues(policy).Set(0)
	policyAtRiskCount.WithLabelValues(policy).Set(0)
	policyBatchRequested.WithLabelValues(policy).Set(0)
	policyBatchResolved.WithLabelValues(policy).Set(0)
	policyBatchFailuresTotal.WithLabelValues(policy).Add(0)
	autoscalerPresent.WithLabelValues(ns, "Deployment", name, "HPA").Set(0)
}

// Asserts the DELTA this call contributes, not an absolute counter value.
// wlrRefreshTotal is a counter on the process-global registry, so it is
// monotonic for the lifetime of the test binary: under `go test -count=N` —
// the shape of a stress run hunting a flake — the same process runs this test
// N times against the same counter, and an absolute assertion holds only on
// the first iteration. Reading a delta rather than resetting the child is
// deliberate: seedMetricsForRegistrationCheck above documents that removing
// series from this registry mid-run is what broke the registration probe once
// already.
func TestEmitWLRRefreshRecordsOutcome(t *testing.T) {
	c := wlrRefreshTotal.WithLabelValues("ns", "Pod", WLRRefreshRetainedEmpty)
	before := testutil.ToFloat64(c)
	EmitWLRRefresh("ns", "Pod", WLRRefreshRetainedEmpty)
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Errorf("counter delta = %v, want 1", got)
	}
}
