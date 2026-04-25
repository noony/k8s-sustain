package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleWorkloadDetailReturnsSnapshot(t *testing.T) {
	srv := newTestServerWithDeployment(t, "default", "web")
	srv.PromClient = &fakePromClient{instant: map[string]float64{
		"k8s_sustain:workload_oom_24h{namespace=\"default\",owner_kind=\"Deployment\",owner_name=\"web\"}": 1,
	}}
	rec := httptest.NewRecorder()
	srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web", nil),
		"default", "Deployment", "web")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		UpdateMode   string  `json:"updateMode"`
		OOM24h       int     `json:"oom24h"`
		DriftPercent float64 `json:"driftPercent"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.OOM24h != 1 {
		t.Fatalf("oom24h: got %d want 1", got.OOM24h)
	}
}
