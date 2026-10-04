package controller

import (
	"context"
	"testing"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

// fakeOOMSource is a canned oomwatch.Source for live-OOM tests.
type fakeOOMSource struct {
	records map[string]*oomwatch.OOMRecord
}

func (f *fakeOOMSource) RecentByWorkload(_, _, _ string, _ time.Duration) map[string]*oomwatch.OOMRecord {
	return f.records
}

func recommendOne(t *testing.T, r *PolicyReconciler, it computeItem) identityResult {
	t.Helper()
	return r.recommend(context.Background(), policyForReconcileWorkload(t, "p"), []computeItem{it},
		autoscaler.NewNamespacedSnapshot(r.Client))[0]
}

func histogramCount(t *testing.T, vec *prom.HistogramVec, labels ...string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := vec.WithLabelValues(labels...).(prom.Metric).Write(&m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// A kill the live watcher saw reaches the computation: the killed container's
// memory is floored above its limit at the kill, the floor is counted, and so
// is how long after the kill the recommendation responded.
func TestRecommend_LiveOOMRaisesTheFloorAndIsCounted(t *testing.T) {
	const ns = "liveoom"
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs().Set(identityOf(ns, "Deployment", "web"), &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1, "side": 0.05},
		MemPerPod: promclient.ContainerValues{"app": 100 << 20, "side": 100 << 20},
	}), true)
	r.LiveOOM = LiveOOMConfig{
		Source: &fakeOOMSource{records: map[string]*oomwatch.OOMRecord{
			"app": {Container: "app", ObservedAt: time.Now(), TerminatedAt: time.Now().Add(-10 * time.Second), OOMLimitBytes: 200 << 20},
		}},
		TriggerCh: make(chan event.GenericEvent),
	}
	tgt := deploymentTarget(ns, "web")
	tgt.Containers = []corev1.Container{{Name: "app"}, {Name: "side"}}
	floorBefore := testutilCounterValue(t, oomFloorApplied, ns, "Deployment", "web", "app")
	latencyBefore := histogramCount(t, oomReactionLatencySeconds, ns, "Deployment", "web")

	res := recommendOne(t, r, itemForTarget(tgt))

	if res.outcome != outcomeRecommended {
		t.Fatalf("outcome = %v, want recommended", res.outcome)
	}
	if got := res.recs["app"].MemoryRequest; got == nil || got.Cmp(resource.MustParse("200Mi")) <= 0 {
		t.Errorf("app memory = %v, want a bump above the 200Mi limit it was killed at", got)
	}
	if got := res.recs["side"].MemoryRequest; got == nil || got.String() != "100Mi" {
		t.Errorf("side memory = %v, want its 100Mi percentile", got)
	}
	if after := testutilCounterValue(t, oomFloorApplied, ns, "Deployment", "web", "app"); after-floorBefore != 1 {
		t.Errorf("oom_floor_applied delta = %v, want 1", after-floorBefore)
	}
	if after := histogramCount(t, oomReactionLatencySeconds, ns, "Deployment", "web"); after-latencyBefore != 1 {
		t.Errorf("oom_reaction_latency observations delta = %d, want 1 for a floor driven by a live kill", after-latencyBefore)
	}
}

// A Too young identity has no Recommendation: nothing is handed to persist or
// apply, and the skip is counted.
func TestRecommend_TooYoungIsNotARecommendation(t *testing.T) {
	const ns = "tooyoung"
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true)
	tgt := deploymentTarget(ns, "web")
	tgt.Object.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Minute)))
	before := testutil.ToFloat64(recommendationSkipped.WithLabelValues(ns, "Deployment", "web", "workload_too_young"))

	res := recommendOne(t, r, itemForTarget(tgt))

	if res.outcome != outcomeTooYoung {
		t.Fatalf("outcome = %v, want too young", res.outcome)
	}
	if res.recs != nil {
		t.Errorf("recs = %v, want none for a Too young identity", res.recs)
	}
	if after := testutil.ToFloat64(recommendationSkipped.WithLabelValues(ns, "Deployment", "web", "workload_too_young")); after-before != 1 {
		t.Errorf("recommendation_skipped delta = %v, want 1", after-before)
	}
}
