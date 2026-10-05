package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		&recommender.WorkloadInputs{CPUPerPod: promclient.ContainerValues{"main": 1}})
	return &Server{K8sClient: c, PromClient: &fakePromClient{}, Inputs: inputs, Logger: testLogger(t)}
}

// A Simulation coordinates against the autoscaler of the member it targets,
// with the governing Policy's coordination, as the controller does.
func TestRunSimulation_CoordinatesAgainstItsMembersAutoscaler(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	res, err := srv.runSimulation(context.Background(), simulateRequest{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; got != "2200m" {
		t.Errorf("cpu = %q, want 2200m: web-green's HPA under the governing Policy's coordination", got)
	}
}

// Batch simulate coordinates each identity as its Simulation does.
func TestBatchSimulate_CoordinatesAgainstItsMembersAutoscaler(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)

	rec := httptest.NewRecorder()
	srv.handlePolicyBatchSimulate(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), "p")

	var resp batchSimulateResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Workloads) != 1 || resp.Workloads[0].Containers["main"].RecommendedCPU != "2200m" {
		t.Errorf("workloads = %+v, want web recommended 2200m: web-green's HPA", resp.Workloads)
	}
}

// Each chart point is computed as the point value is: with headroom, and with
// the OOM floor of the container that was killed only.
func TestRecommendationSeries_ComputesEachPointWithTheIdentitysOOMs(t *testing.T) {
	const mib = 1 << 20
	key := promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"}
	spec := sustainv1alpha1.RightSizingSpec{ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](50)}},
	}}
	inputs := recommendertest.NewStaticInputs().Set(key, &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.2, "side": 0.2},
		MemPerPod: promclient.ContainerValues{"app": 150 * mib, "side": 150 * mib},
		OOM:       map[string]recommender.OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true}},
	})
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	req := recommender.Assemble(context.Background(), inputs, autoscaler.NewNamespacedSnapshot(c), spec, []*inventory.Identity{{Key: key}})[0].Request
	at := time.Unix(1_700_000_000, 0)
	points := func(v float64) []promclient.TimeValue { return []promclient.TimeValue{{Timestamp: at, Value: v}} }

	mem := recommendationSeries(promclient.ContainerTimeSeries{"app": points(100 * mib), "side": points(100 * mib)}, req, true)
	cpu := recommendationSeries(promclient.ContainerTimeSeries{"app": points(0.1)}, req, false)

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
