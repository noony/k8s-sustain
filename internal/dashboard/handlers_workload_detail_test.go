package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleWorkloadDetailReturnsSnapshot(t *testing.T) {
	srv := newTestServerWithDeployment(t, "default", "web")
	srv.PromClient = &fakePromClient{
		instant: map[string]float64{
			"k8s_sustain:workload_oom_24h{namespace=\"default\",owner_kind=\"Deployment\",owner_name=\"web\"}": 1,
		},
		byLabels: map[string]map[string]float64{
			`k8s_sustain_coordination_factor{namespace="default",owner_kind="Deployment",owner_name="web"}`: {
				"cpu|overhead":    1.25,
				"memory|overhead": 1.10,
				"cpu|replica":     0.80,
			},
		},
	}
	rec := httptest.NewRecorder()
	srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web", nil),
		"default", "Deployment", "web")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		UpdateMode          string  `json:"updateMode"`
		OOM24h              int     `json:"oom24h"`
		DriftPercent        float64 `json:"driftPercent"`
		CoordinationFactors *struct {
			Enabled        bool    `json:"enabled"`
			CPUOverhead    float64 `json:"cpuOverhead"`
			MemoryOverhead float64 `json:"memoryOverhead"`
			CPUReplica     float64 `json:"cpuReplica"`
		} `json:"coordinationFactors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.OOM24h != 1 {
		t.Fatalf("oom24h: got %d want 1", got.OOM24h)
	}
	if got.CoordinationFactors == nil {
		t.Fatalf("expected CoordinationFactors to be populated")
	}
	if !got.CoordinationFactors.Enabled {
		t.Errorf("CoordinationFactors.Enabled = false, want true")
	}
	if got.CoordinationFactors.CPUOverhead != 1.25 {
		t.Errorf("CoordinationFactors.CPUOverhead = %v, want 1.25", got.CoordinationFactors.CPUOverhead)
	}
	if got.CoordinationFactors.MemoryOverhead != 1.10 {
		t.Errorf("CoordinationFactors.MemoryOverhead = %v, want 1.10", got.CoordinationFactors.MemoryOverhead)
	}
	if got.CoordinationFactors.CPUReplica != 0.80 {
		t.Errorf("CoordinationFactors.CPUReplica = %v, want 0.80", got.CoordinationFactors.CPUReplica)
	}
}
