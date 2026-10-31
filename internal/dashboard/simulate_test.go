package dashboard

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

// newCoordinatedSimulateServer builds the owner-name identity Deployment/web
// of members web-blue and web-green, governed by policy "p" with autoscaler
// coordination enabled, reporting one core of usage. An HPA at 50% CPU
// targets web-green, never the identity's own name, the way the controller
// reads it. Coordinated, that is 1 * 110/50 = 2.2 cores.
func newCoordinatedSimulateServer(t *testing.T) *Server {
	t.Helper()
	mode := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &mode
	policy.Spec.RightSizing.AutoscalerCoordination.Enabled = true

	objs := []client.Object{policy}
	for _, name := range []string{"web-blue", "web-green"} {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		d.Spec.Template.Annotations = map[string]string{
			sustainv1alpha1.PolicyAnnotation:    "p",
			sustainv1alpha1.OwnerNameAnnotation: "web",
		}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}
		objs = append(objs, d)
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-green"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web-green"},
			MaxReplicas:    5,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](50)},
				},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(append(objs, hpa)...).Build()
	inputs := recommendertest.NewStaticInputs().Set(
		promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"},
		recommender.WithCPUUsage(promclient.ContainerValues{"main": 1}))
	return &Server{K8sClient: c, PromClient: &fakePromClient{}, Inputs: inputs, Logger: testLogger(t)}
}

// Each chart point is computed as the point value is: with headroom, and with
// the OOM floor of the container that was killed only.
func TestRecommendationSeries_ComputesEachPointWithTheIdentitysOOMs(t *testing.T) {
	const mib = 1 << 20
	key := promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"}
	spec := sustainv1alpha1.RightSizingSpec{ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](50)}},
	}}
	inputs := recommendertest.NewStaticInputs().Set(key,
		recommender.WithCPUUsage(promclient.ContainerValues{"app": 0.2, "side": 0.2}),
		recommender.WithMemoryUsage(promclient.ContainerValues{"app": 150 * mib, "side": 150 * mib}),
		recommender.WithOOMKills(map[string]recommender.OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true}}))
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	req := recommender.Assemble(context.Background(), inputs, autoscaler.NewNamespacedSnapshot(c), time.Now, spec, []*inventory.Identity{{Key: key}})[0].Request
	at := time.Unix(1_700_000_000, 0)
	points := func(v float64) []promclient.TimeValue { return []promclient.TimeValue{{Timestamp: at, Value: v}} }

	mem := recommendationSeries(promclient.ContainerTimeSeries{"app": points(100 * mib), "side": points(100 * mib)}, req, corev1.ResourceMemory)
	cpu := recommendationSeries(promclient.ContainerTimeSeries{"app": points(0.1)}, req, corev1.ResourceCPU)

	if got := mem["app"][0].Value; got != 200*mib {
		t.Errorf("app memory = %v, want the 200Mi OOM floor", got)
	}
	if got := mem["side"][0].Value; got != 100*mib {
		t.Errorf("side memory = %v, want its 100Mi point: it was not killed", got)
	}
	if got := cpu["app"][0]; got.Value != 0.15 || !got.Timestamp.Equal(at) {
		t.Errorf("app cpu = %+v, want 0.15 cores (0.1 + 50%%) at the point's time", got)
	}
}

// The override merges onto the governing Policy: what it leaves alone is the
// Policy's, and the effective settings it returns have every default
// explicit.
func TestRunSimulation_OverrideMergesOntoTheGoverningPolicy(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	res, err := srv.runSimulation(context.Background(), simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
		Override: json.RawMessage(`{"resourcesConfigs":{"cpu":{"requests":{"headroom":10}}}}`),
	})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; got != "2420m" {
		t.Errorf("cpu = %q, want 2420m: 1 core with 10%% headroom, coordinated as the Policy says", got)
	}
	cpu := res.Effective.ResourcesConfigs.CPU
	if res.Policy != "p" || !res.Effective.AutoscalerCoordination.Enabled || *cpu.Requests.Headroom != 10 ||
		*cpu.Requests.Percentile != 95 || cpu.Window != "168h" || !cpu.Limits.KeepLimit {
		t.Errorf("policy %q, effective %+v; want p's coordination, the override's headroom and the defaults", res.Policy, res.Effective)
	}
	if _, ok := recommender.TracedCoordination(res.Trace["main"].CPU); !ok {
		t.Errorf("trace = %+v, want the Simulation's own, with its coordination stage", res.Trace["main"])
	}
}

// A Simulation can start from any Policy instead of the governing one.
func TestRunSimulation_StartsFromTheChosenPolicy(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	uncoordinated := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "flat"}}
	if err := srv.K8sClient.Create(context.Background(), uncoordinated); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := srv.runSimulation(context.Background(), simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web", Policy: "flat",
	})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; res.Policy != "flat" || got != "1" {
		t.Errorf("policy %q, cpu %q; want flat's uncoordinated 1 core", res.Policy, got)
	}
}

// A kept request is recommended nothing, so a Simulation draws no
// recommendation series for it. That a Simulation without an override equals
// the controller's Recommendation is held by the controller's
// TestSimulationRecommendsWhatTheControllerStores.
func TestRunSimulation_AKeptRequestDrawsNoSeries(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	ctx := context.Background()
	var policy sustainv1alpha1.Policy
	if err := srv.K8sClient.Get(ctx, client.ObjectKey{Name: "p"}, &policy); err != nil {
		t.Fatalf("get: %v", err)
	}
	policy.Spec.RightSizing.ResourcesConfigs.Memory.Requests.KeepRequest = true
	if err := srv.K8sClient.Update(ctx, &policy); err != nil {
		t.Fatalf("update: %v", err)
	}
	key := promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"}
	srv.Inputs = recommendertest.NewStaticInputs().Set(key,
		recommender.WithCPUUsage(promclient.ContainerValues{"main": 1}),
		recommender.WithMemoryUsage(promclient.ContainerValues{"main": 64 << 20}))

	res, err := srv.runSimulation(ctx, simulateRequest{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}

	if c := res.Containers["main"]; c.CPURequest != "2200m" || c.MemoryRequest != "" {
		t.Errorf("main = %+v, want its CPU coordinated and its memory request kept", c)
	}
	if res.CPURecommendations == nil || res.MemRecommendations != nil {
		t.Errorf("recommendation series cpu=%v memory=%v, want none for the kept memory request", res.CPURecommendations, res.MemRecommendations)
	}
}
