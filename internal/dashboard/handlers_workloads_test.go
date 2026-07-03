package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func newTestServerWithDeployment(t *testing.T, ns, name string) *Server {
	t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: map[string]string{}},
	}
	d.Spec.Template.Annotations = map[string]string{"k8s.sustain.io/policy": "p"}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	return &Server{K8sClient: c, Logger: testLogger(t)}
}

func TestAllWorkloadsIncludesRiskDriftHPA(t *testing.T) {
	srv := newTestServerWithDeployment(t, "default", "web")
	srv.PromClient = &fakePromClient{
		byLabels: map[string]map[string]float64{
			"sum by (namespace, owner_kind, owner_name) (k8s_sustain:workload_oom_24h)":              {"default|Deployment|web": 2},
			"max by (namespace, owner_kind, owner_name) (abs(1 - k8s_sustain_workload_drift_ratio))": {"default|Deployment|web": 0.6},
			"k8s_sustain_workload_retry_state == 1":                                                  {},
			"k8s_sustain_autoscaler_present":                                                         {"default|Deployment|web": 1},
			"k8s_sustain_coordination_factor": {
				"default|Deployment|web|cpu|overhead":    1.2,
				"default|Deployment|web|memory|overhead": 1.1,
				"default|Deployment|web|cpu|replica":     0.9,
			},
		},
	}
	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))
	var resp struct {
		Items []struct {
			Name                string  `json:"name"`
			RiskState           string  `json:"riskState"`
			DriftPercent        float64 `json:"driftPercent"`
			AutoscalerPresent   bool    `json:"autoscalerPresent"`
			CoordinationFactors *struct {
				Enabled        bool    `json:"enabled"`
				CPUOverhead    float64 `json:"cpuOverhead"`
				MemoryOverhead float64 `json:"memoryOverhead"`
				CPUReplica     float64 `json:"cpuReplica"`
			} `json:"coordinationFactors"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items", len(resp.Items))
	}
	item := resp.Items[0]
	if item.RiskState != "at-risk" || item.AutoscalerPresent != true {
		t.Fatalf("unexpected row: %+v", item)
	}
	if item.CoordinationFactors == nil {
		t.Fatalf("expected CoordinationFactors to be populated")
	}
	if !item.CoordinationFactors.Enabled {
		t.Errorf("CoordinationFactors.Enabled = false, want true")
	}
	if item.CoordinationFactors.CPUOverhead != 1.2 {
		t.Errorf("CoordinationFactors.CPUOverhead = %v, want 1.2", item.CoordinationFactors.CPUOverhead)
	}
	if item.CoordinationFactors.MemoryOverhead != 1.1 {
		t.Errorf("CoordinationFactors.MemoryOverhead = %v, want 1.1", item.CoordinationFactors.MemoryOverhead)
	}
	if item.CoordinationFactors.CPUReplica != 0.9 {
		t.Errorf("CoordinationFactors.CPUReplica = %v, want 0.9", item.CoordinationFactors.CPUReplica)
	}
}

func TestAllWorkloadsIncludesStandaloneJobButSkipsCronJobOwned(t *testing.T) {
	standalone := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "scenario-job", Name: "oneshot"},
	}
	standalone.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "scenario-job"}
	standalone.Spec.Template.Spec.Containers = []corev1.Container{{Name: "stress"}}

	trueVal := true
	owned := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "scenario-cronjob",
			Name:      "nightly-29384",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       "nightly",
				UID:        types.UID("cj-uid"),
				Controller: &trueVal,
			}},
		},
	}
	owned.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "scenario-cronjob"}
	owned.Spec.Template.Spec.Containers = []corev1.Container{{Name: "stress"}}

	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(standalone, owned).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?kind=Job", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		Items []struct {
			Namespace string `json:"namespace"`
			Kind      string `json:"kind"`
			Name      string `json:"name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d (items=%+v)", resp.Total, resp.Items)
	}
	got := resp.Items[0]
	if got.Kind != "Job" || got.Name != "oneshot" || got.Namespace != "scenario-job" {
		t.Fatalf("unexpected item: %+v", got)
	}
}

// TestAllWorkloadsNamespaceFilterKeepsFacets pins the facet fix: filtering by
// namespace (or kind) must not collapse the namespace/kind dropdowns to the
// filtered subset — they are derived from the full, unfiltered list.
func TestAllWorkloadsNamespaceFilterKeepsFacets(t *testing.T) {
	dA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "web"}}
	dB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "api"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "oneshot"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(dA, dB, job).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?namespace=ns-a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		Items []struct {
			Namespace string `json:"namespace"`
		} `json:"items"`
		Total      int      `json:"total"`
		Namespaces []string `json:"namespaces"`
		Kinds      []string `json:"kinds"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].Namespace != "ns-a" {
		t.Fatalf("expected only ns-a items, got %+v", resp)
	}
	if !slices.Contains(resp.Namespaces, "ns-a") || !slices.Contains(resp.Namespaces, "ns-b") {
		t.Errorf("namespaces facet = %v, want both ns-a and ns-b", resp.Namespaces)
	}
	if !slices.Contains(resp.Kinds, "Deployment") || !slices.Contains(resp.Kinds, "Job") {
		t.Errorf("kinds facet = %v, want Deployment and Job", resp.Kinds)
	}
}

// TestPolicyWorkloadsMissingPolicyIs404WithoutCacheControl pins two error-path
// contracts at once: a missing policy is a 404 (not a 500), and error
// responses must not carry Cache-Control — intermediaries could otherwise pin
// a transient failure for the success max-age.
func TestPolicyWorkloadsMissingPolicyIs404WithoutCacheControl(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/ghost/workloads", nil), "ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q, want empty on error responses", got)
	}
}

// TestPolicyWorkloadsAPIServerErrorIs500 verifies that a non-NotFound Get
// failure (apiserver outage, RBAC, timeout) is reported as a 500, not
// disguised as a 404.
func TestPolicyWorkloadsAPIServerErrorIs500(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("apiserver is down")
			},
		}).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/workloads", nil), "p")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPolicyWorkloadsIncludesStandaloneJob(t *testing.T) {
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "scenario-job"}}
	policy.Spec.RightSizing.Update.Types.Job = &mode

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "scenario-job", Name: "oneshot"},
	}
	job.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "scenario-job"}
	job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "stress"}}

	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, job).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/scenario-job/workloads", nil), "scenario-job")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		Items []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if resp.Total != 1 || resp.Items[0].Kind != "Job" || resp.Items[0].Name != "oneshot" {
		t.Fatalf("expected 1 standalone Job, got %+v", resp)
	}
}

// retainedWLR builds a WorkloadRecommendation as the retention sweep leaves
// it: labeled, policy set, observed resources snapshotted.
func retainedWLR(policy, ns, kind, name string) *sustainv1alpha1.WorkloadRecommendation {
	cpu := resource.MustParse("500m")
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      strings.ToLower(kind) + "-" + name,
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: policy},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: ns, Name: name},
			Policy:      policy,
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.Now(),
			ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{
				"main": {CPURequest: &cpu},
			},
		},
	}
}

// TestAllWorkloadsIncludesInactiveFromRetainedWLR: a retained WLR with no
// live object becomes an inactive row with lastSeenAt and the observed
// container resources.
func TestAllWorkloadsIncludesInactiveFromRetainedWLR(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(retainedWLR("p", "airflow", "Pod", "etl")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))

	var resp struct {
		Items []struct {
			Namespace  string `json:"namespace"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			Active     bool   `json:"active"`
			LastSeenAt string `json:"lastSeenAt"`
			PolicyName string `json:"policyName"`
			Automated  bool   `json:"automated"`
			Containers []struct {
				Name       string `json:"name"`
				CPURequest string `json:"cpuRequest"`
			} `json:"containers"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1 inactive row", len(resp.Items))
	}
	row := resp.Items[0]
	if row.Active {
		t.Error("row.Active = true, want false")
	}
	if row.Kind != "Pod" || row.Name != "etl" || row.Namespace != "airflow" {
		t.Errorf("identity wrong: %+v", row)
	}
	if row.LastSeenAt == "" {
		t.Error("lastSeenAt missing on inactive row")
	}
	if !row.Automated || row.PolicyName != "p" {
		t.Errorf("policy fields wrong: %+v", row)
	}
	if len(row.Containers) != 1 || row.Containers[0].CPURequest != "500m" {
		t.Errorf("observed containers wrong: %+v", row.Containers)
	}
}

// TestAllWorkloadsLiveRowSuppressesWLRTwin: when the workload object still
// exists, its WLR must NOT produce a duplicate row.
func TestAllWorkloadsLiveRowSuppressesWLRTwin(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(d, retainedWLR("p", "prod", "Deployment", "web")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))
	var resp struct {
		Items []struct {
			Active bool `json:"active"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1 (no WLR duplicate)", len(resp.Items))
	}
	if !resp.Items[0].Active {
		t.Error("live row must have active=true")
	}
}

// TestAllWorkloadsActiveFilter: ?active=false returns only inactive rows,
// ?active=true only live ones.
func TestAllWorkloadsActiveFilter(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(d, retainedWLR("p", "airflow", "Pod", "etl")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	for query, wantName := range map[string]string{"false": "etl", "true": "web"} {
		rec := httptest.NewRecorder()
		srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?active="+query, nil))
		var resp struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		if len(resp.Items) != 1 || resp.Items[0].Name != wantName {
			t.Errorf("active=%s: got %+v, want single row %q", query, resp.Items, wantName)
		}
	}
}

// TestPolicyWorkloadsIncludesInactiveScopedToPolicy: the policy-scoped list
// only merges that policy's retained WLRs.
func TestPolicyWorkloadsIncludesInactiveScopedToPolicy(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	onCreate := sustainv1alpha1.UpdateModeOnCreate
	policy.Spec.RightSizing.Update.Types.Pod = &onCreate
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(policy,
			retainedWLR("p", "airflow", "Pod", "etl"),
			retainedWLR("other", "airflow", "Pod", "other-etl")).
		Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}}

	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/workloads", nil), "p")
	var resp struct {
		Items []struct {
			Name   string `json:"name"`
			Active bool   `json:"active"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 || resp.Items[0].Name != "etl" || resp.Items[0].Active {
		t.Errorf("got %+v, want single inactive row etl", resp.Items)
	}
}
