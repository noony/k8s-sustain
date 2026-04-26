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
		byLabels: map[string]map[string]float64{
			"k8s_sustain:workload_oom_24h": {"default|Deployment|web": 2},
			"max by (namespace, owner_kind, owner_name) (abs(1 - k8s_sustain_workload_drift_ratio))": {"default|Deployment|web": 0.6},
			"k8s_sustain_workload_retry_state == 1": {},
			"k8s_sustain_autoscaler_present":        {"default|Deployment|web": 1},
			`k8s_sustain_coordination_factor{namespace="default",owner_kind="Deployment",owner_name="web"}`: {
				"cpu|overhead":    1.2,
				"memory|overhead": 1.1,
				"cpu|replica":     0.9,
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
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
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
