package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// memHealthSignals is the in-memory HealthSignals adapter: identity health as
// a plain map, so tests state signals instead of PromQL.
type memHealthSignals map[promclient.WorkloadIdentity]identityHealth

func (m memHealthSignals) forIdentities(_ context.Context, ids []promclient.WorkloadIdentity) (map[promclient.WorkloadIdentity]identityHealth, error) {
	out := map[promclient.WorkloadIdentity]identityHealth{}
	for _, id := range ids {
		if h, ok := m[id]; ok {
			out[id] = h
		}
	}
	return out, nil
}

func (m memHealthSignals) unhealthy(context.Context) (map[promclient.WorkloadIdentity]identityHealth, error) {
	out := map[promclient.WorkloadIdentity]identityHealth{}
	for id, h := range m {
		if riskStateOf(h) != riskSafe {
			out[id] = h
		}
	}
	return out, nil
}

func identity(ns, kind, name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: ns, OwnerKind: kind, OwnerName: name}
}

func TestRiskStateOfPrecedence(t *testing.T) {
	blocked := &blockedSignal{Reason: "patch", Attempts: 2}
	cases := []struct {
		name string
		h    identityHealth
		want riskState
	}{
		{"no signal", identityHealth{}, riskSafe},
		{"pods all on the recommendation", identityHealth{TotalPods: 3}, riskSafe},
		{"stale pods", identityHealth{StalePods: 1}, riskDrifted},
		{"OOM", identityHealth{OOM24h: 1}, riskAtRisk},
		{"blocked", identityHealth{Blocked: blocked}, riskBlocked},
		{"OOM over stale pods", identityHealth{OOM24h: 1, StalePods: 2}, riskAtRisk},
		{"blocked over stale pods", identityHealth{Blocked: blocked, StalePods: 2}, riskBlocked},
		{"blocked over OOM", identityHealth{Blocked: blocked, OOM24h: 3}, riskBlocked},
		{"blocked over OOM and stale pods", identityHealth{Blocked: blocked, OOM24h: 3, StalePods: 2}, riskBlocked},
		{"conflicted", identityHealth{Conflicted: true}, riskConflicted},
		{"conflicted over everything", identityHealth{Conflicted: true, Blocked: blocked, OOM24h: 3, StalePods: 2}, riskConflicted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := riskStateOf(tc.h); got != tc.want {
				t.Errorf("riskStateOf(%+v) = %q, want %q", tc.h, got, tc.want)
			}
		})
	}
}

// stubSignalQuerier answers each query by the first metric name it contains,
// standing in for Prometheus's aggregation output.
type stubSignalQuerier struct {
	byMetric map[string]map[string]float64
	failing  string
}

func (s stubSignalQuerier) QueryByLabels(_ context.Context, query string, _ ...string) (map[string]float64, error) {
	if s.failing != "" && strings.Contains(query, s.failing) {
		return nil, errors.New("prometheus unavailable")
	}
	for metric, v := range s.byMetric {
		if strings.Contains(query, metric+"{") || strings.Contains(query, metric+")") || strings.Contains(query, metric+" ") {
			return v, nil
		}
	}
	return map[string]float64{}, nil
}

func TestPrometheusHealthSignalsFoldsSignalsPerIdentity(t *testing.T) {
	api, web := identity("prod", "Deployment", "api"), identity("prod", "Deployment", "web")
	q := stubSignalQuerier{byMetric: map[string]map[string]float64{
		promclient.MetricWorkloadOOM24h:        {"prod|Deployment|web": 2, "prod|Deployment|api": 0},
		promclient.MetricWorkloadStalePods:     {"prod|Deployment|api": 2},
		promclient.MetricWorkloadPods:          {"prod|Deployment|api": 5},
		promclient.MetricWorkloadRetryState:    {"prod|Deployment|api|patch": 1},
		promclient.MetricWorkloadRetryAttempts: {"prod|Deployment|api": 4, "prod|Deployment|web": 9},
		promclient.MetricAutoscalerPresent:     {"prod|Deployment|api": 1},
		promclient.MetricCoordinationFactor: {
			"prod|Deployment|api|cpu|overhead": 1.2,
			"prod|Deployment|web|cpu|overhead": 1.5,
		},
	}}

	got, err := NewPrometheusHealthSignals(q).forIdentities(context.Background(), []promclient.WorkloadIdentity{api, web})
	if err != nil {
		t.Fatal(err)
	}
	a := got[api]
	if a.StalePods != 2 || a.TotalPods != 5 || a.OOM24h != 0 {
		t.Errorf("api pods = %+v, want stale 2 of 5, no OOM", a)
	}
	if a.Blocked == nil || a.Blocked.Reason != "patch" || a.Blocked.Attempts != 4 {
		t.Errorf("api blocked = %+v, want patch after 4 attempts", a.Blocked)
	}
	if !a.AutoscalerPresent || a.CoordinationFactors == nil || a.CoordinationFactors.CPUOverhead != 1.2 {
		t.Errorf("api autoscaler = %v %+v, want present with cpu overhead 1.2", a.AutoscalerPresent, a.CoordinationFactors)
	}
	w := got[web]
	if w.OOM24h != 2 || w.Blocked != nil {
		t.Errorf("web = %+v, want 2 OOMs and not blocked despite a retry counter", w)
	}
	if w.CoordinationFactors != nil {
		t.Errorf("web has no autoscaler, so its leftover coordination factors must not show, got %+v", w.CoordinationFactors)
	}

	unhealthy, err := NewPrometheusHealthSignals(q).unhealthy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(unhealthy) != 2 {
		t.Errorf("unhealthy = %v, want api (blocked) and web (OOM)", unhealthy)
	}
}

func TestPrometheusHealthSignalsReturnsWhatItCouldRead(t *testing.T) {
	api := identity("prod", "Deployment", "api")
	q := stubSignalQuerier{
		byMetric: map[string]map[string]float64{promclient.MetricWorkloadStalePods: {"prod|Deployment|api": 1}},
		failing:  promclient.MetricWorkloadOOM24h,
	}
	got, err := NewPrometheusHealthSignals(q).forIdentities(context.Background(), []promclient.WorkloadIdentity{api})
	if err == nil {
		t.Error("a failed query must surface as an error")
	}
	if got[api].StalePods != 1 {
		t.Errorf("signals that were read must survive a failed query, got %+v", got[api])
	}
}

func ownerNameGroupServer(t *testing.T, health memHealthSignals) *Server {
	t.Helper()
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = ptrMode(sustainv1alpha1.UpdateModeOngoing)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	objs := []client.Object{
		policy,
		deploymentWithOwnerName("prod", "api-blue", "api", base),
		deploymentWithOwnerName("prod", "api-green", "api", base.Add(time.Hour)),
		deploymentWithOwnerName("prod", "web-blue", "web", base),
		deploymentWithOwnerName("prod", "web-green", "web", base.Add(time.Hour)),
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	return &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: health}
}

// Owner-name groups are listed under their identity, so their health must be
// read by the identity too; keyed by a member name it never showed.
func TestOwnerNameGroupShowsBlockedAndDrifted(t *testing.T) {
	srv := ownerNameGroupServer(t, memHealthSignals{
		identity("prod", "Deployment", "api"): {Blocked: &blockedSignal{Reason: "patch", Attempts: 3}, StalePods: 1, TotalPods: 4},
		identity("prod", "Deployment", "web"): {StalePods: 2, TotalPods: 2},
	})
	want := map[string]riskState{"api": riskBlocked, "web": riskDrifted}

	for _, path := range []string{"/api/workloads", "/api/policies/p/workloads"} {
		rec := httptest.NewRecorder()
		if path == "/api/workloads" {
			srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, path, nil))
		} else {
			srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, path, nil), "p")
		}
		var resp struct {
			Items []struct {
				Name      string    `json:"name"`
				RiskState riskState `json:"riskState"`
				StalePods int       `json:"stalePods"`
			} `json:"items"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		if len(resp.Items) != 2 {
			t.Fatalf("%s: got %d rows, want one per identity", path, len(resp.Items))
		}
		for _, row := range resp.Items {
			if row.RiskState != want[row.Name] {
				t.Errorf("%s: %s riskState = %q, want %q", path, row.Name, row.RiskState, want[row.Name])
			}
		}
	}

	for name, state := range want {
		rec := httptest.NewRecorder()
		srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/prod/Deployment/"+name, nil),
			"prod", "Deployment", name)
		var got struct {
			RiskState riskState `json:"riskState"`
			StalePods int       `json:"stalePods"`
			TotalPods int       `json:"totalPods"`
			Blocked   *struct {
				Reason   string `json:"reason"`
				Attempts int    `json:"attempts"`
			} `json:"blocked"`
		}
		decodeEnvelopeData(t, rec.Body, &got)
		if got.RiskState != state {
			t.Errorf("detail %s riskState = %q, want %q", name, got.RiskState, state)
		}
		if state == riskBlocked && (got.Blocked == nil || got.Blocked.Reason != "patch" || got.Blocked.Attempts != 3) {
			t.Errorf("detail %s blocked = %+v, want patch after 3 attempts", name, got.Blocked)
		}
	}
}

func TestAllWorkloadsRiskFilterUsesClassifiedState(t *testing.T) {
	srv := ownerNameGroupServer(t, memHealthSignals{
		identity("prod", "Deployment", "api"): {Blocked: &blockedSignal{Reason: "patch"}, OOM24h: 1},
		identity("prod", "Deployment", "web"): {OOM24h: 2},
	})
	for risk, wantName := range map[string]string{"blocked": "api", "at-risk": "web"} {
		rec := httptest.NewRecorder()
		srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?risk="+risk, nil))
		var resp struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		decodeEnvelopeData(t, rec.Body, &resp)
		if len(resp.Items) != 1 || resp.Items[0].Name != wantName {
			t.Errorf("risk=%s: got %+v, want only %s", risk, resp.Items, wantName)
		}
	}
}

func TestAllWorkloadsCarriesAutoscalerAndCoordination(t *testing.T) {
	srv := ownerNameGroupServer(t, memHealthSignals{
		identity("prod", "Deployment", "api"): {
			AutoscalerPresent:   true,
			CoordinationFactors: &coordinationFactors{Enabled: true, CPUOverhead: 1.2, MemoryOverhead: 1.1, CPUReplica: 0.9},
		},
	})
	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?search=api", nil))
	var resp struct {
		Items []struct {
			AutoscalerPresent   bool                 `json:"autoscalerPresent"`
			CoordinationFactors *coordinationFactors `json:"coordinationFactors"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Items) != 1 || !resp.Items[0].AutoscalerPresent {
		t.Fatalf("got %+v, want the api identity with its autoscaler", resp.Items)
	}
	if cf := resp.Items[0].CoordinationFactors; cf == nil || cf.CPUOverhead != 1.2 || cf.MemoryOverhead != 1.1 || cf.CPUReplica != 0.9 {
		t.Errorf("coordinationFactors = %+v", cf)
	}
}

func summaryServer(t *testing.T, health memHealthSignals) *Server {
	t.Helper()
	return &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Health:     health,
		Logger:     testr.New(t),
	}
}

// The At risk KPI used to sum the controller's blocked count; it is the
// identities whose Risk state is At risk, and Blocked is its own count.
func TestSummaryKPIsCountIdentitiesByRiskState(t *testing.T) {
	srv := summaryServer(t, memHealthSignals{
		identity("shop", "Deployment", "checkout"): {OOM24h: 3},
		identity("prod", "StatefulSet", "db"):      {OOM24h: 1, StalePods: 1},
		identity("prod", "Deployment", "worker"):   {Blocked: &blockedSignal{Reason: "patch"}},
		identity("prod", "Deployment", "api"):      {Blocked: &blockedSignal{Reason: "prometheus"}, OOM24h: 2},
		identity("prod", "Deployment", "web"):      {StalePods: 2, TotalPods: 3},
		identity("prod", "Deployment", "quiet"):    {TotalPods: 3},
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	var got summaryResponseV2
	decodeEnvelopeData(t, rec.Body, &got)

	if got.KPI.AtRiskCount != 2 {
		t.Errorf("atRiskCount = %d, want 2 OOM identities (the blocked one with an OOM counts as Blocked)", got.KPI.AtRiskCount)
	}
	if got.KPI.BlockedCount != 2 {
		t.Errorf("blockedCount = %d, want 2", got.KPI.BlockedCount)
	}
	if got.KPI.DriftedCount != 1 {
		t.Errorf("driftedCount = %d, want 1 (stale pods behind an OOM count as At risk)", got.KPI.DriftedCount)
	}
}

func TestSummaryAttentionQueueFilesEachIdentityUnderItsRiskState(t *testing.T) {
	srv := summaryServer(t, memHealthSignals{
		identity("shop", "Deployment", "checkout"): {OOM24h: 3},
		identity("prod", "StatefulSet", "db"):      {OOM24h: 1},
		identity("prod", "Deployment", "api"):      {Blocked: &blockedSignal{Reason: "prometheus", Attempts: 5}, OOM24h: 2},
		identity("prod", "Deployment", "web"):      {StalePods: 2},
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	var got summaryResponseV2
	decodeEnvelopeData(t, rec.Body, &got)

	names := func(rows []attentionRow) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Namespace+"/"+r.Kind+"/"+r.Name)
		}
		return out
	}
	want := map[string][]string{
		"risk":    {"shop/Deployment/checkout", "prod/StatefulSet/db"},
		"blocked": {"prod/Deployment/api"},
		"drift":   {"prod/Deployment/web"},
	}
	for group, w := range want {
		if g := names(got.Attention[group]); strings.Join(g, ",") != strings.Join(w, ",") {
			t.Errorf("attention[%s] = %v, want %v (worst first, one group per identity)", group, g, w)
		}
	}
	if s := got.Attention["risk"][0].Signal; s != "OOM" {
		t.Errorf("risk row signal = %q, want OOM", s)
	}
}

func TestSummaryAttentionQueueIsCapped(t *testing.T) {
	health := memHealthSignals{}
	for i := range maxAttentionRows + 5 {
		health[identity("prod", "Deployment", "w"+string(rune('a'+i)))] = identityHealth{StalePods: i + 1}
	}
	srv := summaryServer(t, health)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	var got summaryResponseV2
	decodeEnvelopeData(t, rec.Body, &got)
	if n := len(got.Attention["drift"]); n != maxAttentionRows {
		t.Errorf("drift rows = %d, want the cap %d", n, maxAttentionRows)
	}
	if got.KPI.DriftedCount != maxAttentionRows+5 {
		t.Errorf("driftedCount = %d, want every drifted identity, not just the listed ones", got.KPI.DriftedCount)
	}
}
