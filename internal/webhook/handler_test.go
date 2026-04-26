package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	}

	tests := []struct {
		kind string
		want *sustainv1alpha1.UpdateMode
	}{
		{"Deployment", &ongoing},
		{"StatefulSet", &onCreate},
		{"CronJob", &ongoing},
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
