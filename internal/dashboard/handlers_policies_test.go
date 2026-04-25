package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestServerWithPolicy(t *testing.T, name string) *Server {
	t.Helper()
	p := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(p).Build()
	return &Server{K8sClient: c, Logger: testLogger(t)}
}

func TestHandlePoliciesIncludesEffectiveness(t *testing.T) {
	srv := newTestServerWithPolicy(t, "p")
	srv.PromClient = &fakePromClient{
		byLabel: map[string]map[string]float64{
			"k8s_sustain_policy_workload_count":       {"p": 5},
			"k8s_sustain:policy_cpu_savings_cores":    {"p": 1.2},
			"k8s_sustain:policy_memory_savings_bytes": {"p": 2_000_000_000},
			"k8s_sustain_policy_at_risk_count":        {"p": 1},
		},
	}
	rec := httptest.NewRecorder()
	srv.handlePolicies(rec, httptest.NewRequest(http.MethodGet, "/api/policies", nil))
	var got []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["cpuSavingsCores"] != 1.2 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestHandlePolicyDetailIncludesEffectivenessSeries(t *testing.T) {
	srv := newTestServerWithPolicy(t, "p")
	srv.PromClient = &fakePromClient{}
	rec := httptest.NewRecorder()
	srv.handlePolicyDetail(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p", nil), "p")
	var got map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if _, ok := got["effectivenessSeries"]; !ok {
		t.Fatal("expected effectivenessSeries in payload")
	}
}
