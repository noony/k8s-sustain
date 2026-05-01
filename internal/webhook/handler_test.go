package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// mockPromVector builds a Prometheus instant-query JSON response with per-container
// values, keyed by container name.
func mockPromVector(values map[string]float64) string {
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"vector","result":[`)
	first := true
	for name, v := range values {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, `{"metric":{"container":%q},"value":[0,"%g"]}`, name, v)
	}
	b.WriteString(`]}}`)
	return b.String()
}

func qtyp(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func TestModeForKind(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	onCreate := sustainv1alpha1.UpdateModeOnCreate

	ut := sustainv1alpha1.UpdateTypes{
		Deployment:  &ongoing,
		StatefulSet: &onCreate,
		CronJob:     &ongoing,
		ArgoRollout: &onCreate,
	}

	tests := []struct {
		kind string
		want *sustainv1alpha1.UpdateMode
	}{
		{"Deployment", &ongoing},
		{"StatefulSet", &onCreate},
		{"CronJob", &ongoing},
		{"Rollout", &onCreate},
		{"DaemonSet", nil},
		{"Unknown", nil},
	}

	for _, tt := range tests {
		got := modeForKind(ut, tt.kind)
		if tt.want == nil {
			if got != nil {
				t.Errorf("modeForKind(%q) = %v, want nil", tt.kind, *got)
			}
			continue
		}
		if got == nil || *got != *tt.want {
			t.Errorf("modeForKind(%q) = %v, want %v", tt.kind, got, *tt.want)
		}
	}
}

// TestResolveOwner_RolloutChain verifies the webhook walks
// Pod → ReplicaSet → Rollout (Argo Rollouts) and reports the Rollout as the
// top-level workload kind. This is what makes OnCreate injection work for
// Rollout-owned pods.
func TestResolveOwner_RolloutChain(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	ctrl := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-rollout-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "argoproj.io/v1alpha1",
				Kind:       "Rollout",
				Name:       "my-rollout",
				Controller: &ctrl,
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	h := &Handler{Client: fakeClient}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-rollout-abc123-xyz",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "my-rollout-abc123",
				Controller: &ctrl,
			}},
		},
	}

	kind, name, err := h.resolveOwner(context.Background(), pod)
	if err != nil {
		t.Fatalf("resolveOwner: %v", err)
	}
	if kind != "Rollout" {
		t.Errorf("kind = %q, want %q", kind, "Rollout")
	}
	if name != "my-rollout" {
		t.Errorf("name = %q, want %q", name, "my-rollout")
	}
}

// TestResolveOwner_DeploymentChain verifies the existing
// Pod → ReplicaSet → Deployment walk still works alongside the Rollout case.
func TestResolveOwner_DeploymentChain(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	ctrl := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-deploy-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "my-deploy",
				Controller: &ctrl,
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	h := &Handler{Client: fakeClient}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-deploy-abc123-xyz",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "my-deploy-abc123",
				Controller: &ctrl,
			}},
		},
	}

	kind, name, err := h.resolveOwner(context.Background(), pod)
	if err != nil {
		t.Fatalf("resolveOwner: %v", err)
	}
	if kind != "Deployment" {
		t.Errorf("kind = %q, want %q", kind, "Deployment")
	}
	if name != "my-deploy" {
		t.Errorf("name = %q, want %q", name, "my-deploy")
	}
}

func TestBuildPatches_EmptyRecs(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	recs := map[string]workload.ContainerRecommendation{}
	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil patches for empty recs")
	}
}

func TestBuildPatches_SetsResources(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {
			CPURequest:    qtyp("100m"),
			MemoryRequest: qtyp("64Mi"),
		},
	}

	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected patches, got nil")
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Op != "add" {
		t.Errorf("expected op 'add', got %q", patches[0].Op)
	}
	if patches[0].Path != "/spec/containers/0/resources" {
		t.Errorf("expected path '/spec/containers/0/resources', got %q", patches[0].Path)
	}
}

func TestBuildPatches_MultipleContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app":     {CPURequest: qtyp("100m")},
		"sidecar": {CPURequest: qtyp("50m")},
	}

	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}
}

func TestBuildPatches_SkipsUnmatchedContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qtyp("100m")},
	}

	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
}

// TestBuildRecommendations_WorkloadLevelSignal verifies that buildRecommendations
// uses workload-level totals divided by replica count to derive per-pod values.
// With replicas=3, total CPU=1.2 cores → per-pod=0.4 cores → request=400m
// (no headroom configured), total memory=300MiB → per-pod=100MiB.
func TestBuildRecommendations_WorkloadLevelSignal(t *testing.T) {
	replicas := 3

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_cpu_usage"):
			// workload total = 0.4 * replicas cores
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 0.4 * float64(replicas)})))
		case strings.Contains(q, "workload_memory_usage"):
			// workload total = 100 MiB * replicas bytes
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 100 * 1024 * 1024 * float64(replicas)})))
		case strings.Contains(q, "workload_replicas"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"` + strconv.Itoa(replicas) + `"]}]}}`))
		case strings.Contains(q, "container_cpu_usage_by_workload"):
			// per-pod floor (same as per-pod value, so no floor bump)
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 0.4})))
		case strings.Contains(q, "container_memory_by_workload"):
			// per-pod floor
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 100 * 1024 * 1024})))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	// Use a fake k8s client (no HPAs/ScaledObjects → autoscaler.Info{Kind:None}).
	fakeClient := fake.NewClientBuilder().Build()

	h := &Handler{
		Client:           fakeClient,
		PrometheusClient: pc,
	}

	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU: sustainv1alpha1.ResourceConfig{
						Window:   "168h",
						Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95},
					},
					Memory: sustainv1alpha1.ResourceConfig{
						Window:   "168h",
						Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95},
					},
				},
			},
		},
	}

	containers := []corev1.Container{{Name: "app"}}
	recs, err := h.buildRecommendations(context.Background(), policy, "default", "Deployment", "my-app", containers)
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}

	rec, ok := recs["app"]
	if !ok {
		t.Fatal("expected recommendation for container 'app'")
	}

	// Per-pod CPU = 1.2 / 3 ≈ 0.4 cores. Due to float64 rounding the recommender
	// sees ~400.000...005 millicores and math.Ceil rounds up to 401m.
	wantCPU := resource.MustParse("401m")
	if rec.CPURequest == nil {
		t.Fatal("CPURequest is nil")
	}
	if rec.CPURequest.Cmp(wantCPU) != 0 {
		t.Errorf("CPURequest = %s, want %s", rec.CPURequest.String(), wantCPU.String())
	}

	// Per-pod memory = 300MiB / 3 = 100MiB
	wantMem := resource.MustParse("100Mi")
	if rec.MemoryRequest == nil {
		t.Fatal("MemoryRequest is nil")
	}
	if rec.MemoryRequest.Cmp(wantMem) != 0 {
		t.Errorf("MemoryRequest = %s, want %s", rec.MemoryRequest.String(), wantMem.String())
	}
}

// TestBuildRecommendations_NoPrometheusData verifies that when Prometheus returns
// no samples for a container, no recommendation is emitted for it.
func TestBuildRecommendations_NoPrometheusData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		// workload_replicas returns 1 so we don't fail the replica query
		if strings.Contains(q, "workload_replicas") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"1"]}]}}`))
			return
		}
		// All other queries return empty
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	fakeClient := fake.NewClientBuilder().Build()
	h := &Handler{
		Client:           fakeClient,
		PrometheusClient: pc,
	}

	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
			},
		},
	}

	containers := []corev1.Container{{Name: "app"}}
	recs, err := h.buildRecommendations(context.Background(), policy, "default", "Deployment", "my-app", containers)
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected no recommendations when Prometheus has no data, got %v", recs)
	}
}

// TestBuildRecommendations_AppliesAutoscalerCoordination verifies that the
// webhook applies overhead from autoscaler coordination at admission time. With
// a baseline of 100m CPU and an HPA targeting CPU at 70%, the request should
// be bumped to ceil(100 * 110 / 70) = 158m.
func TestBuildRecommendations_AppliesAutoscalerCoordination(t *testing.T) {
	const replicas = 1
	const baselineCores = 0.1 // 100m baseline

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_cpu_usage"):
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": baselineCores * float64(replicas)})))
		case strings.Contains(q, "workload_memory_usage"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case strings.Contains(q, "workload_replicas"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"` + strconv.Itoa(replicas) + `"]}]}}`))
		case strings.Contains(q, "container_cpu_usage_by_workload"):
			// Floor matches per-pod baseline so floor doesn't bump it.
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": baselineCores})))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	// Register autoscaling/v2 in the scheme so the fake client can serve HPAs.
	scheme := runtime.NewScheme()
	if err := autoscalingv2.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	cpuTarget := int32(70)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "my-app"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "my-app"},
			MaxReplicas:    5,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &cpuTarget,
					},
				},
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	h := &Handler{
		Client:           fakeClient,
		PrometheusClient: pc,
	}

	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU: sustainv1alpha1.ResourceConfig{
						Window:   "168h",
						Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95},
					},
					Memory: sustainv1alpha1.ResourceConfig{
						Window:   "168h",
						Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95},
					},
				},
				AutoscalerCoordination: sustainv1alpha1.AutoscalerCoordination{
					Enabled: true,
				},
			},
		},
	}

	containers := []corev1.Container{{Name: "app"}}
	recs, err := h.buildRecommendations(context.Background(), policy, "default", "Deployment", "my-app", containers)
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}

	rec, ok := recs["app"]
	if !ok {
		t.Fatal("expected recommendation for container 'app'")
	}
	if rec.CPURequest == nil {
		t.Fatal("CPURequest is nil")
	}
	// Baseline 100m, overhead = ceil(100 * 110 / 70) = 158m.
	if rec.CPURequest.MilliValue() != 158 {
		t.Errorf("CPURequest = %dm, want 158m", rec.CPURequest.MilliValue())
	}
}

// admitTestEnv bundles the boilerplate for end-to-end admit() tests:
// scheme, fake client, mock Prometheus, and a constructed Handler.
type admitTestEnv struct {
	handler *Handler
	server  *httptest.Server
}

// newAdmitEnv builds a Handler whose Prometheus mock returns one CPU and one
// memory sample for container "app" (yielding ~100m CPU, ~64Mi memory after
// per-pod division by replicas=1).
func newAdmitEnv(t *testing.T, objs ...runtime.Object) *admitTestEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_cpu_usage"):
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 0.1})))
		case strings.Contains(q, "workload_memory_usage"):
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 64 * 1024 * 1024})))
		case strings.Contains(q, "workload_replicas"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"1"]}]}}`))
		case strings.Contains(q, "container_cpu_usage_by_workload"):
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 0.1})))
		case strings.Contains(q, "container_memory_by_workload"):
			_, _ = w.Write([]byte(mockPromVector(map[string]float64{"app": 64 * 1024 * 1024})))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))

	pc, err := promclient.New(server.URL)
	if err != nil {
		server.Close()
		t.Fatalf("prometheus client: %v", err)
	}

	objsTyped := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		if co, ok := o.(client.Object); ok {
			objsTyped = append(objsTyped, co)
		}
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objsTyped...).Build()

	return &admitTestEnv{
		handler: &Handler{Client: fc, PrometheusClient: pc},
		server:  server,
	}
}

func (e *admitTestEnv) close() { e.server.Close() }

func basicPolicy(name string, mode sustainv1alpha1.UpdateMode) *sustainv1alpha1.Policy {
	p95 := int32(95)
	return &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &mode},
				},
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
			},
		},
	}
}

func deploymentReplicaSet(ns, rsName, deployName string) *appsv1.ReplicaSet {
	ctrl := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      rsName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployName,
				Controller: &ctrl,
			}},
		},
	}
}

func podWithRSOwner(ns, podName, rsName, policy string) *corev1.Pod {
	ctrl := true
	annotations := map[string]string{}
	if policy != "" {
		annotations[sustainv1alpha1.PolicyAnnotation] = policy
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        podName,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       rsName,
				Controller: &ctrl,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
}

func admissionRequestFor(t *testing.T, pod *corev1.Pod) *admissionv1.AdmissionRequest {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return &admissionv1.AdmissionRequest{
		UID:       "uid-1",
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		Object:    runtime.RawExtension{Raw: raw},
	}
}

// TestAdmit_NoAnnotation_AllowsWithoutPatch verifies pods without the policy
// annotation pass through untouched.
func TestAdmit_NoAnnotation_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t)
	defer env.close()

	pod := podWithRSOwner("default", "p", "rs", "")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_PolicyNotFound_AllowsWithoutPatch verifies fail-open behaviour
// when the annotation references a Policy that does not exist.
func TestAdmit_PolicyNotFound_AllowsWithoutPatch(t *testing.T) {
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, rs)
	defer env.close()

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "missing-policy")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch when policy missing, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_StandalonePod_AllowsWithoutPatch verifies pods without a controller
// owner are skipped — the webhook can't determine workload kind.
func TestAdmit_StandalonePod_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t, basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate))
	defer env.close()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "standalone",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Error("expected no patch for standalone pod")
	}
}

// TestAdmit_KindNotConfigured_AllowsWithoutPatch verifies that a workload kind
// not listed in the policy's update.types is skipped.
func TestAdmit_KindNotConfigured_AllowsWithoutPatch(t *testing.T) {
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				// Only StatefulSet configured — Deployment-owned pods should be skipped.
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{StatefulSet: &mode}},
			},
		},
	}
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)
	defer env.close()

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Error("expected no patch when kind not configured")
	}
}

// TestAdmit_RecommendOnly_AllowsWithoutPatch verifies that recommend-only
// mode returns allow=true with no patch even when injection would have applied.
func TestAdmit_RecommendOnly_AllowsWithoutPatch(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)
	defer env.close()
	env.handler.RecommendOnly = true

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("recommend-only must not patch, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_DeploymentInjection_PatchesResources verifies the happy-path:
// annotated pod owned by a Deployment-backed ReplicaSet gets a JSON Patch
// setting CPU and memory requests for the matching container.
func TestAdmit_DeploymentInjection_PatchesResources(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)
	defer env.close()

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected JSON patch for happy-path injection")
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("expected JSONPatch type, got %v", resp.PatchType)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch op, got %d", len(patches))
	}
	if patches[0].Path != "/spec/containers/0/resources" {
		t.Errorf("patch path = %q", patches[0].Path)
	}
}

// TestServeHTTP_RoundTripsAdmissionReview verifies the HTTP handler decodes
// the request, runs admit, and re-encodes a valid AdmissionReview response
// keyed by the original UID.
func TestServeHTTP_RoundTripsAdmissionReview(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)
	defer env.close()

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1"},
		Request:  admissionRequestFor(t, pod),
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Response == nil {
		t.Fatal("response missing")
	}
	if out.Response.UID != "uid-1" {
		t.Errorf("UID = %q, want uid-1", out.Response.UID)
	}
	if !out.Response.Allowed {
		t.Error("expected allowed=true")
	}
	if out.Response.Patch == nil {
		t.Error("expected patch in response")
	}
}

// TestServeHTTP_BadBody_Returns400 verifies the handler rejects malformed
// AdmissionReview JSON with HTTP 400 instead of allowing through.
func TestServeHTTP_BadBody_Returns400(t *testing.T) {
	env := newAdmitEnv(t)
	defer env.close()

	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestIsValidPolicyName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"good", true},
		{"good-name-123", true},
		{"", false},
		{"UPPER", false},                          // DNS-1123 is lowercase
		{"a/b", false},                            // slash
		{strings.Repeat("a", 254), false},         // > 253 chars
		{"-leading-dash", false},
		{"trailing-dash-", false},
		{"a..b", false},                           // empty label
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isValidPolicyName(c.name)
			if got != c.want {
				t.Errorf("isValidPolicyName(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestAdmit_InvalidPolicyAnnotation_AllowsWithoutPatch verifies that a
// malformed annotation value (uppercase, oversized, etc.) is rejected early
// without flowing into Prometheus selector strings.
func TestAdmit_InvalidPolicyAnnotation_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t, basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate))
	defer env.close()

	pod := podWithRSOwner("default", "p", "rs", strings.Repeat("a", 300))
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow for invalid annotation")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch for invalid policy name, got %d bytes", len(resp.Patch))
	}
}
