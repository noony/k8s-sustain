package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleWorkloadDetailReturnsSnapshot(t *testing.T) {
	srv := newTestServerWithDeployment(t, "default", "web")
	srv.Health = memHealthSignals{
		identity("default", "Deployment", "web"): {
			OOM24h:              1,
			StalePods:           2,
			TotalPods:           5,
			AutoscalerPresent:   true,
			CoordinationFactors: &coordinationFactors{Enabled: true, CPUOverhead: 1.25, MemoryOverhead: 1.10, CPUReplica: 0.80},
		},
	}
	rec := httptest.NewRecorder()
	srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web", nil),
		"default", "Deployment", "web")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		RiskState           riskState            `json:"riskState"`
		OOM24h              int                  `json:"oom24h"`
		StalePods           int                  `json:"stalePods"`
		TotalPods           int                  `json:"totalPods"`
		Blocked             *struct{}            `json:"blocked"`
		CoordinationFactors *coordinationFactors `json:"coordinationFactors"`
	}
	decodeEnvelopeData(t, rec.Body, &got)
	if got.RiskState != riskAtRisk {
		t.Errorf("riskState = %q, want at-risk", got.RiskState)
	}
	if got.OOM24h != 1 || got.StalePods != 2 || got.TotalPods != 5 {
		t.Errorf("oom24h/stale/total = %d/%d/%d, want 1/2/5", got.OOM24h, got.StalePods, got.TotalPods)
	}
	if got.Blocked != nil {
		t.Errorf("blocked = %+v, want absent", got.Blocked)
	}
	if cf := got.CoordinationFactors; cf == nil || !cf.Enabled || cf.CPUOverhead != 1.25 || cf.MemoryOverhead != 1.10 || cf.CPUReplica != 0.80 {
		t.Errorf("coordinationFactors = %+v", cf)
	}
}

func TestHandleWorkloadDetailWithoutSignalsIsSafe(t *testing.T) {
	srv := newTestServerWithDeployment(t, "default", "web")
	rec := httptest.NewRecorder()
	srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web", nil),
		"default", "Deployment", "web")
	var got struct {
		RiskState riskState `json:"riskState"`
	}
	decodeEnvelopeData(t, rec.Body, &got)
	if got.RiskState != riskSafe {
		t.Errorf("riskState = %q, want safe for an identity with no signal", got.RiskState)
	}
}
