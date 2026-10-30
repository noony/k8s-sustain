package controller_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/controller"
	"github.com/noony/k8s-sustain/internal/dashboard"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

var webKey = promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"}

// parityCluster holds Policy p, coordinating with autoscalers and excluding
// init containers or not, and the owner-name identity Deployment/web of
// members web-blue and web-green, an hour old. Only web-green runs the init
// container migrate, and an HPA at 50% CPU targets web-green, never web.
func parityCluster(t *testing.T, excludeInit bool) client.Client {
	t.Helper()
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &ongoing
	policy.Spec.RightSizing.AutoscalerCoordination.Enabled = true
	policy.Spec.RightSizing.ExcludeInitContainers = excludeInit
	policy.Spec.RightSizing.ResourcesConfigs.Memory.Requests.Headroom = ptr.To[int32](20)

	objs := []client.Object{policy}
	for _, name := range []string{"web-blue", "web-green"} {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name, UID: types.UID(name + "-uid"),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}
		d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
		d.Spec.Template.Labels = map[string]string{"app": name}
		d.Spec.Template.Annotations = map[string]string{
			sustainv1alpha1.PolicyAnnotation:    "p",
			sustainv1alpha1.OwnerNameAnnotation: "web",
		}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		}}}
		objs = append(objs, d)
	}
	objs[2].(*appsv1.Deployment).Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
	objs = append(objs, &autoscalingv2.HorizontalPodAutoscaler{
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
	})
	return fake.NewClientBuilder().
		WithScheme(dashboard.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(objs...).
		Build()
}

// noCharts is a Prometheus with no series to chart: a Simulation's values
// come from its Inputs alone.
type noCharts struct{ dashboard.PromQuerier }

func (noCharts) QueryCPURangeByContainer(context.Context, string, string, string, promclient.TimeRange, string) (promclient.ContainerTimeSeries, error) {
	return nil, nil
}

func (noCharts) QueryMemoryRangeByContainer(context.Context, string, string, string, promclient.TimeRange, string) (promclient.ContainerTimeSeries, error) {
	return nil, nil
}

func (noCharts) QueryCPURequestRangeByContainer(context.Context, string, string, string, promclient.TimeRange, string) (promclient.ContainerTimeSeries, error) {
	return nil, nil
}

func (noCharts) QueryMemoryRequestRangeByContainer(context.Context, string, string, string, promclient.TimeRange, string) (promclient.ContainerTimeSeries, error) {
	return nil, nil
}

func (noCharts) QueryWorkloadCPURecommendationRangeByContainer(context.Context, string, string, string, float64, string, promclient.TimeRange, string) (promclient.ContainerTimeSeries, error) {
	return nil, nil
}

func (noCharts) QueryWorkloadMemoryRecommendationRangeByContainer(context.Context, string, string, string, float64, string, promclient.TimeRange, string) (promclient.ContainerTimeSeries, error) {
	return nil, nil
}

// recommended is a container's Recommendation as the Simulation API renders
// it.
type recommended struct {
	CPURequest         string `json:"cpuRequest"`
	MemoryRequest      string `json:"memoryRequest"`
	CPULimit           string `json:"cpuLimit,omitempty"`
	MemoryLimit        string `json:"memoryLimit,omitempty"`
	CPULimitRemoved    bool   `json:"cpuLimitRemoved,omitempty"`
	MemoryLimitRemoved bool   `json:"memoryLimitRemoved,omitempty"`
}

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}

func renderStored(recs map[string]sustainv1alpha1.ContainerRecommendation) map[string]recommended {
	out := make(map[string]recommended, len(recs))
	for name, r := range recs {
		out[name] = recommended{
			CPURequest:         quantityString(r.CPURequest),
			MemoryRequest:      quantityString(r.MemoryRequest),
			CPULimit:           quantityString(r.CPULimit),
			MemoryLimit:        quantityString(r.MemoryLimit),
			CPULimitRemoved:    r.RemoveCPULimit,
			MemoryLimitRemoved: r.RemoveMemoryLimit,
		}
	}
	return out
}

// serve runs one request against the dashboard's API and decodes the data of
// its envelope into out.
func serve(t *testing.T, srv *dashboard.Server, req *http.Request, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s = %d: %s", req.Method, req.URL, rec.Code, rec.Body)
	}
	if err := json.NewDecoder(rec.Body).Decode(&struct {
		Data any `json:"data"`
	}{Data: out}); err != nil {
		t.Fatalf("decode %s: %v", req.URL, err)
	}
}

// With no override, the dashboard's Simulation of an identity and its batch
// simulate recommend what the controller's pass stores in the identity's
// WorkloadRecommendation, from the same objects and inputs: with init
// containers excluded or not, for an owner-name group coordinated against
// the HPA of one of its members.
func TestSimulationRecommendsWhatTheControllerStores(t *testing.T) {
	for _, excludeInit := range []bool{false, true} {
		t.Run("excludeInitContainers="+strconv.FormatBool(excludeInit), func(t *testing.T) {
			ctx := context.Background()
			c := parityCluster(t, excludeInit)
			inputs := recommendertest.NewStaticInputs().Set(webKey, &recommender.WorkloadInputs{
				CPUPerPod: promclient.ContainerValues{"main": 1, "migrate": 0.5},
				MemPerPod: promclient.ContainerValues{"main": 64 << 20, "migrate": 32 << 20},
			})

			if _, err := controller.ReconcilerForTest(c, inputs).Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var wlrs sustainv1alpha1.WorkloadRecommendationList
			if err := c.List(ctx, &wlrs, client.InNamespace("default")); err != nil || len(wlrs.Items) != 1 {
				t.Fatalf("WorkloadRecommendations = %d (%v), want web's alone", len(wlrs.Items), err)
			}
			stored := wlrs.Items[0].Status
			want := renderStored(stored.Containers)

			srv := &dashboard.Server{K8sClient: c, PromClient: noCharts{}, Inputs: inputs, Logger: logr.Discard()}
			var sim struct {
				Outcome    sustainv1alpha1.RecommendationOutcome     `json:"outcome"`
				Containers map[string]recommended                    `json:"containers"`
				Trace      map[string]sustainv1alpha1.ContainerTrace `json:"trace"`
			}
			serve(t, srv, httptest.NewRequest(http.MethodPost, "/api/simulate",
				strings.NewReader(`{"namespace":"default","ownerKind":"Deployment","ownerName":"web"}`)), &sim)
			var batch struct {
				Workloads []struct {
					Name       string                                `json:"name"`
					Outcome    sustainv1alpha1.RecommendationOutcome `json:"outcome"`
					Containers map[string]struct {
						RecommendedCPU    string `json:"recommendedCpu"`
						RecommendedMemory string `json:"recommendedMemory"`
					} `json:"containers"`
				} `json:"workloads"`
			}
			serve(t, srv, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), &batch)

			if stored.Outcome != sustainv1alpha1.OutcomeComputed || stored.ComputedBy != "p" {
				t.Fatalf("stored = %s by %q, want Computed by p", stored.Outcome, stored.ComputedBy)
			}
			if names := slices.Sorted(maps.Keys(want)); slices.Contains(names, "migrate") == excludeInit || want["main"].CPURequest != "2200m" {
				t.Errorf("stored containers %v, main %+v; want migrate only when init containers are recommended and main at 2200m: web-green's HPA",
					names, want["main"])
			}
			if sim.Outcome != stored.Outcome || !maps.Equal(sim.Containers, want) || !equality.Semantic.DeepEqual(sim.Trace, stored.Trace) {
				t.Errorf("simulation = %s %+v, want the stored %s %+v with its trace", sim.Outcome, sim.Containers, stored.Outcome, want)
			}
			if len(batch.Workloads) != 1 || batch.Workloads[0].Name != "web" || batch.Workloads[0].Outcome != stored.Outcome {
				t.Fatalf("batch = %+v, want web alone, %s", batch.Workloads, stored.Outcome)
			}
			got := batch.Workloads[0].Containers
			if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want))) {
				t.Errorf("batch containers %v, want the stored %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
			}
			for name, w := range want {
				if g := got[name]; g.RecommendedCPU != w.CPURequest || g.RecommendedMemory != w.MemoryRequest {
					t.Errorf("batch %s = %+v, want the stored %+v", name, g, w)
				}
			}
		})
	}
}
