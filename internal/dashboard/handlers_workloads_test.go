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
	return &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}
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
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

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

// Facets are derived from the full list, not the filtered subset.
func TestAllWorkloadsNamespaceFilterKeepsFacets(t *testing.T) {
	dA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "web"}}
	dB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "api"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "oneshot"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(dA, dB, job).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

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

// Error responses must not carry Cache-Control, or intermediaries pin a
// transient failure for the success max-age.
func TestPolicyWorkloadsMissingPolicyIs404WithoutCacheControl(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/ghost/workloads", nil), "ghost")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q, want empty on error responses", got)
	}
}

func TestPolicyWorkloadsAPIServerErrorIs500(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("apiserver is down")
			},
		}).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

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
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

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

// retainedWLR builds a WorkloadRecommendation as the retention sweep leaves it.
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

func TestAllWorkloadsListsDepartedIdentity(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Pod = ptrMode(sustainv1alpha1.UpdateModeOngoing)
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(policy, retainedWLR("p", "airflow", "Pod", "etl")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))

	var resp struct {
		Items []struct {
			Namespace  string `json:"namespace"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			Departed   bool   `json:"departed"`
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
		t.Fatalf("got %d items, want 1 departed row", len(resp.Items))
	}
	row := resp.Items[0]
	if !row.Departed {
		t.Error("row.Departed = false, want true")
	}
	if row.Kind != "Pod" || row.Name != "etl" || row.Namespace != "airflow" {
		t.Errorf("identity wrong: %+v", row)
	}
	if row.LastSeenAt == "" {
		t.Error("lastSeenAt missing on departed row")
	}
	if !row.Automated || row.PolicyName != "p" {
		t.Errorf("policy fields wrong: %+v", row)
	}
	if len(row.Containers) != 1 || row.Containers[0].CPURequest != "500m" {
		t.Errorf("observed containers wrong: %+v", row.Containers)
	}
}

func TestAllWorkloadsLiveRowSuppressesWLRTwin(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(d, retainedWLR("p", "prod", "Deployment", "web")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))
	var resp struct {
		Items []struct {
			Departed bool `json:"departed"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1 (no WLR duplicate)", len(resp.Items))
	}
	if resp.Items[0].Departed {
		t.Error("an identity with a live member is not departed")
	}
}

func TestAllWorkloadsDepartedFilter(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(d, retainedWLR("p", "airflow", "Pod", "etl")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	for query, wantName := range map[string]string{"true": "etl", "false": "web"} {
		rec := httptest.NewRecorder()
		srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?departed="+query, nil))
		var resp struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		if len(resp.Items) != 1 || resp.Items[0].Name != wantName {
			t.Errorf("departed=%s: got %+v, want single row %q", query, resp.Items, wantName)
		}
	}
}

func TestPolicyWorkloadsIncludesDepartedScopedToPolicy(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	onCreate := sustainv1alpha1.UpdateModeOnCreate
	policy.Spec.RightSizing.Update.Types.Pod = &onCreate
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(policy,
			retainedWLR("p", "airflow", "Pod", "etl"),
			retainedWLR("other", "airflow", "Pod", "other-etl")).
		Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/workloads", nil), "p")
	var resp struct {
		Items []struct {
			Name     string `json:"name"`
			Departed bool   `json:"departed"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 || resp.Items[0].Name != "etl" || !resp.Items[0].Departed {
		t.Errorf("got %+v, want single departed row etl", resp.Items)
	}
}

func TestAllWorkloadsSort(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "web"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "web"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-c", Name: "api"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "zeta"}},
	).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"ns-c/api", "ns-a/web", "ns-b/web", "ns-a/zeta"}},
		{"?sort=name", []string{"ns-c/api", "ns-a/web", "ns-b/web", "ns-a/zeta"}},
		{"?sort=-name", []string{"ns-a/zeta", "ns-b/web", "ns-a/web", "ns-c/api"}},
		{"?sort=namespace", []string{"ns-a/web", "ns-a/zeta", "ns-b/web", "ns-c/api"}},
		{"?sort=kind", []string{"ns-a/web", "ns-b/web", "ns-a/zeta", "ns-c/api"}},
		{"?sort=name&pageSize=2&page=2", []string{"ns-b/web", "ns-a/zeta"}},
	} {
		rec := httptest.NewRecorder()
		srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads"+tc.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.query, rec.Code)
		}
		var resp struct {
			Items []struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"items"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		got := make([]string, 0, len(resp.Items))
		for _, it := range resp.Items {
			got = append(got, it.Namespace+"/"+it.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestAllWorkloadsSortRejectsUnknownKey(t *testing.T) {
	srv := newTestServerWithDeployment(t, "ns", "web")
	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?sort=containers", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"sort"`) {
		t.Errorf("body %s should name the sort field", rec.Body.String())
	}
}

func TestAllWorkloadsSearchAcrossPages(t *testing.T) {
	var objs []client.Object
	for _, n := range []string{"web-1", "api", "web-2", "db", "WEB-3", "web-4", "cache"} {
		objs = append(objs, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: strings.ToLower(n)}})
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	for _, tc := range []struct {
		page string
		want []string
	}{
		{"1", []string{"web-1", "web-2"}},
		{"2", []string{"web-3", "web-4"}},
		{"3", []string{}},
	} {
		rec := httptest.NewRecorder()
		srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?search=WeB&pageSize=2&page="+tc.page, nil))
		var resp struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			Total int `json:"total"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		got := []string{}
		for _, it := range resp.Items {
			got = append(got, it.Name)
		}
		if resp.Total != 4 || !slices.Equal(got, tc.want) {
			t.Errorf("page %s: total=%d items=%v, want total=4 items=%v", tc.page, resp.Total, got, tc.want)
		}
	}
}

func TestPolicyWorkloadsSortAndSearch(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy.Spec.RightSizing.Update.Types.Deployment = &mode
	objs := []client.Object{policy}
	for _, ref := range []struct{ ns, name string }{
		{"ns-b", "web-1"}, {"ns-a", "api"}, {"ns-c", "web-2"}, {"ns-a", "web-3"}, {"ns-b", "db"},
	} {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ref.ns, Name: ref.name}}
		d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
		objs = append(objs, d)
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	for _, tc := range []struct {
		query     string
		wantTotal int
		want      []string
	}{
		{"", 5, []string{"ns-a/api", "ns-b/db", "ns-b/web-1", "ns-c/web-2", "ns-a/web-3"}},
		{"?sort=-name", 5, []string{"ns-a/web-3", "ns-c/web-2", "ns-b/web-1", "ns-b/db", "ns-a/api"}},
		{"?sort=namespace", 5, []string{"ns-a/api", "ns-a/web-3", "ns-b/db", "ns-b/web-1", "ns-c/web-2"}},
		{"?search=WEB&pageSize=2&page=2", 3, []string{"ns-a/web-3"}},
		{"?search=web&namespace=ns-b", 1, []string{"ns-b/web-1"}},
	} {
		rec := httptest.NewRecorder()
		srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/workloads"+tc.query, nil), "p")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.query, rec.Code)
		}
		var resp struct {
			Items []struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"items"`
			Total      int      `json:"total"`
			Matched    int      `json:"matched"`
			Namespaces []string `json:"namespaces"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		if resp.Matched != 5 {
			t.Errorf("%s: matched = %d, want 5 regardless of filters", tc.query, resp.Matched)
		}
		got := []string{}
		for _, it := range resp.Items {
			got = append(got, it.Namespace+"/"+it.Name)
		}
		if resp.Total != tc.wantTotal || !slices.Equal(got, tc.want) {
			t.Errorf("%s: total=%d items=%v, want total=%d items=%v", tc.query, resp.Total, got, tc.wantTotal, tc.want)
		}
		if len(resp.Namespaces) != 3 {
			t.Errorf("%s: namespaces facet = %v, want all 3 regardless of filters", tc.query, resp.Namespaces)
		}
	}
}

func TestPolicyWorkloadsRejectsUnknownSort(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}
	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/workloads?sort=policyName", nil), "p")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"sort"`) {
		t.Fatalf("status %d body %s, want 400 naming sort", rec.Code, rec.Body.String())
	}
}
