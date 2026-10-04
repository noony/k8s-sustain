package dashboard

import (
	"context"
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
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

// newCoordinatedSimulateServer builds a Deployment managed by policy "p" with
// autoscaler coordination enabled, targeted by an HPA at 50% CPU, reporting
// one core of usage. Coordinated, that is 1 * 110/50 = 2.2 cores.
func newCoordinatedSimulateServer(t *testing.T) *Server {
	t.Helper()
	mode := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &mode
	policy.Spec.RightSizing.AutoscalerCoordination.Enabled = true

	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
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
	objs := []client.Object{policy, d, hpa}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	inputs := recommendertest.NewStaticInputs().Set(
		promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"},
		&recommender.WorkloadInputs{CPUPerPod: promclient.ContainerValues{"main": 1}})
	return &Server{K8sClient: c, PromClient: &fakePromClient{}, Inputs: inputs, Logger: testLogger(t)}
}

func TestRunSimulation_InheritsManagingPolicyCoordination(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	res, err := srv.runSimulation(context.Background(), simulateRequest{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; got != "2200m" {
		t.Errorf("cpu = %q, want 2200m (managing policy's coordination applied by default)", got)
	}
}

// Each chart point is computed as the point value is: with headroom, and with
// the OOM floor of the container that was killed only.
func TestRecommendationSeries_ComputesEachPointWithTheIdentitysOOMs(t *testing.T) {
	const mib = 1 << 20
	spec := simulationSpec{resources: sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](50)}},
	}}
	inputs := &recommender.WorkloadInputs{OOM: map[string]recommender.OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true}}}
	at := time.Unix(1_700_000_000, 0)
	points := func(v float64) []promclient.TimeValue { return []promclient.TimeValue{{Timestamp: at, Value: v}} }

	mem := recommendationSeries(promclient.ContainerTimeSeries{"app": points(100 * mib), "side": points(100 * mib)},
		spec, autoscaler.Info{Kind: autoscaler.KindNone}, inputs, true)
	cpu := recommendationSeries(promclient.ContainerTimeSeries{"app": points(0.1)},
		spec, autoscaler.Info{Kind: autoscaler.KindNone}, inputs, false)

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

func TestRunSimulation_RequestOverridesCoordination(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	res, err := srv.runSimulation(context.Background(), simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
		AutoscalerCoordination: &sustainv1alpha1.AutoscalerCoordination{Enabled: false},
	})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; got != "1" {
		t.Errorf("cpu = %q, want 1 (request disabled coordination)", got)
	}
}
