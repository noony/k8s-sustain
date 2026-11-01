package controller

import (
	"strings"
	"testing"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

// fakeOOMSource is a canned oomwatch.Source for live-OOM tests.
type fakeOOMSource struct {
	records map[string]*oomwatch.OOMRecord
}

func (f *fakeOOMSource) ByWorkload(promclient.WorkloadIdentity) map[string]*oomwatch.OOMRecord {
	return f.records
}

func histogramCount(t *testing.T, vec *prom.HistogramVec, labels ...string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := vec.WithLabelValues(labels...).(prom.Metric).Write(&m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// A kill the live watcher saw reaches the computation through the decorated
// inputs: the killed container's memory is floored above its limit at the
// kill, the floor is counted from its trace stage, and so is how long after
// the kill the recommendation responded.
func TestReconcile_LiveOOMRaisesTheFloorAndIsCounted(t *testing.T) {
	const ns = "liveoom"
	dep := establishedDeployment(ns, "web", "p")
	dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, corev1.Container{Name: "side"})
	static := recommendertest.NewStaticInputs().Set(identityOf(ns, "Deployment", "web"),
		recommender.WithCPUUsage(promclient.ContainerValues{"app": 0.1, "side": 0.05}),
		recommender.WithMemoryUsage(promclient.ContainerValues{"app": 100 << 20, "side": 100 << 20}))
	r := reconcilerWithInputs(t, recommender.WithLiveOOMs(static, &fakeOOMSource{records: map[string]*oomwatch.OOMRecord{
		"app": {Container: "app", ObservedAt: time.Now(), TerminatedAt: time.Now().Add(-10 * time.Second), OOMLimitBytes: 200 << 20},
	}}), true, ongoingDeployments("p"), dep)
	floorBefore := testutilCounterValue(t, oomFloorApplied, ns, "Deployment", "web", "app")
	latencyBefore := histogramCount(t, oomReactionLatencySeconds, ns, "Deployment", "web")

	reconcileOnce(t, r, "p")

	stored := getWLRFor(t, r, ns, "Deployment", "web").Status.Containers
	if got := stored["app"].MemoryRequest; got == nil || got.Cmp(resource.MustParse("200Mi")) <= 0 {
		t.Errorf("app memory = %v, want a bump above the 200Mi limit it was killed at", got)
	}
	if got := stored["side"].MemoryRequest; got == nil || got.String() != "100Mi" {
		t.Errorf("side memory = %v, want its 100Mi percentile", got)
	}
	if after := testutilCounterValue(t, oomFloorApplied, ns, "Deployment", "web", "app"); after-floorBefore != 1 {
		t.Errorf("oom_floor_applied delta = %v, want 1", after-floorBefore)
	}
	if after := histogramCount(t, oomReactionLatencySeconds, ns, "Deployment", "web"); after-latencyBefore != 1 {
		t.Errorf("oom_reaction_latency observations delta = %d, want 1 for a floor driven by a live kill", after-latencyBefore)
	}
}

// cpuHPA scales Deployment ns/target on CPU utilization at pct.
func cpuHPA(ns, target string, pct int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: target + "-hpa"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: target},
			MaxReplicas:    4,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To(pct)},
				},
			}},
		},
	}
}

// An owner-name group is coordinated against the autoscaler of the member it
// targets, never looked up under the identity's own name: the stored trace
// records the coordination, and only that member reports the autoscaler.
func TestReconcile_OwnerNameGroupIsCoordinatedByItsMembersAutoscaler(t *testing.T) {
	const ns = "coordinated"
	policy := ongoingDeployments("p")
	policy.Spec.RightSizing.AutoscalerCoordination.Enabled = true
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "api"), true, policy,
		groupedDeployment(ns, "api-blue", "api", "p"), groupedDeployment(ns, "api-green", "api", "p"),
		cpuHPA(ns, "api-green", 50))

	reconcileOnce(t, r, "p")

	tr := getWLRFor(t, r, ns, "Deployment", "api").Status.Trace["app"]
	if c, ok := recommender.TracedCoordination(tr.CPU); !ok || c.Overhead != 2.2 {
		t.Errorf("coordination = %+v (traced %v), want the 110/50 overhead of api-green's HPA", c, ok)
	}
	var detected []string
	for _, e := range drainEvents(r.recorder.(*events.FakeRecorder)) {
		if strings.Contains(e, "AutoscalerDetected") {
			detected = append(detected, e)
		}
	}
	if len(detected) != 1 || !strings.Contains(detected[0], "Deployment/api-green") {
		t.Errorf("AutoscalerDetected events = %v, want one, for api-green", detected)
	}
}

// A Too young identity has no Recommendation: its outcome is recorded with
// nothing to inject, and the skip is counted.
func TestReconcile_TooYoungIsNotARecommendation(t *testing.T) {
	const ns = "tooyoung"
	dep := annotatedDeployment(ns, "web", "p")
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true, ongoingDeployments("p"), dep)
	before := testutil.ToFloat64(recommendationSkipped.WithLabelValues(ns, "Deployment", "web", "workload_too_young"))

	reconcileOnce(t, r, "p")

	got := getWLRFor(t, r, ns, "Deployment", "web")
	if got.Status.Outcome != sustainv1alpha1.OutcomeTooYoung || len(got.Status.Containers) != 0 {
		t.Errorf("outcome = %q with %v, want TooYoung and no Recommendation", got.Status.Outcome, got.Status.Containers)
	}
	if after := testutil.ToFloat64(recommendationSkipped.WithLabelValues(ns, "Deployment", "web", "workload_too_young")); after-before != 1 {
		t.Errorf("recommendation_skipped delta = %v, want 1", after-before)
	}
}
