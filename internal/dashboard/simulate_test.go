package dashboard

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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
	prom := &usageBatchPromClient{cpuByOwner: map[string]promclient.ContainerValues{"web": {"main": 1}}}
	return &Server{K8sClient: c, PromClient: prom, Logger: testLogger(t)}
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

// The recommendations endpoint and the simulator must agree for the same
// workload, which is the whole point of the shared pipeline.
func TestRunSimulationWithEntry_MatchesPolicySpec(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	entry, err := srv.getWorkloadEntry(context.Background(), "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("getWorkloadEntry: %v", err)
	}
	policy := &sustainv1alpha1.Policy{}
	if err := srv.K8sClient.Get(context.Background(), client.ObjectKey{Name: "p"}, policy); err != nil {
		t.Fatal(err)
	}
	res, err := srv.runSimulationWithEntry(context.Background(), policySpec(policy, "default", "Deployment", "web"), entry)
	if err != nil {
		t.Fatalf("runSimulationWithEntry: %v", err)
	}
	if got := res.Containers["main"].CPURequest; got != "2200m" {
		t.Errorf("cpu = %q, want 2200m", got)
	}
}
