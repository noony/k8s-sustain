package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
		byLabel: map[string]map[string]float64{
			"k8s_sustain:workload_oom_24h":                                    {"web": 2},
			"max by (owner_name) (abs(1 - k8s_sustain_workload_drift_ratio))": {"web": 0.6},
			"k8s_sustain_workload_retry_state == 1":                           {},
			"k8s_sustain_hpa_present":                                         {"web": 1},
		},
	}
	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))
	var resp struct {
		Items []struct {
			Name         string  `json:"name"`
			RiskState    string  `json:"riskState"`
			DriftPercent float64 `json:"driftPercent"`
			HPAPresent   bool    `json:"hpaPresent"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items", len(resp.Items))
	}
	if resp.Items[0].RiskState != "at-risk" || resp.Items[0].HPAPresent != true {
		t.Fatalf("unexpected row: %+v", resp.Items[0])
	}
}
