package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

func coordinatedWLR(ns, name string) *sustainv1alpha1.WorkloadRecommendation {
	cpu := resource.MustParse("300m")
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: wlrcache.Name("Deployment", name)},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: ns, Name: name},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			Outcome:    sustainv1alpha1.OutcomeComputed,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{name: {CPURequest: &cpu}},
			Trace: map[string]sustainv1alpha1.ContainerTrace{name: {
				CPU: &sustainv1alpha1.ResourceTrace{WithHeadroom: cpu, Clamped: cpu, Coordination: &sustainv1alpha1.CoordinationTrace{
					OverheadFactor: 1.2, Scaled: cpu, Value: cpu,
				}},
			}},
		},
	}
}

// riskBoardServer has one identity per Risk state and a few that test the
// precedence: split is Conflicted and OOM-killed, api is Blocked and
// OOM-killed, db is OOM-killed with stale pods. gone has an OOM series but no
// object left.
func riskBoardServer(t *testing.T) *Server {
	t.Helper()
	now := time.Now()
	var objs []client.Object
	objs = append(objs, deploymentPolicy("p"), deploymentPolicy("q"),
		deploymentOptingInto("p", "prod", "split-blue", "split", now),
		deploymentOptingInto("q", "prod", "split-green", "split", now),
		coordinatedWLR("prod", "quiet"))
	for _, name := range []string{"checkout", "db", "worker", "api", "web", "quiet"} {
		objs = append(objs, deploymentOptingInto("p", "prod", name, "", now))
	}
	srv := identityServer(t, memHealthSignals{
		identity("prod", "Deployment", "split"):    {OOM24h: 4},
		identity("prod", "Deployment", "checkout"): {OOM24h: 3},
		identity("prod", "Deployment", "db"):       {OOM24h: 1, StalePods: 1},
		identity("prod", "Deployment", "worker"):   {Blocked: &blockedSignal{Reason: "patch", Attempts: 1}},
		identity("prod", "Deployment", "api"):      {Blocked: &blockedSignal{Reason: "prometheus", Attempts: 5}, OOM24h: 2},
		identity("prod", "Deployment", "web"):      {StalePods: 2, TotalPods: 3},
		identity("prod", "Deployment", "quiet"):    {TotalPods: 3},
		identity("prod", "Deployment", "gone"):     {OOM24h: 5},
	}, objs...)
	srv.PromClient = &fakePromClient{byLabel: map[string]map[string]float64{
		"k8s_sustain_policy_workload_count": {"p": 6},
	}}
	return srv
}

func overview(t *testing.T, srv *Server) summaryResponseV2 {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got summaryResponseV2
	decodeEnvelopeData(t, rec.Body, &got)
	return got
}

func attentionNames(g attentionGroup) []string {
	out := []string{}
	for _, r := range g.Rows {
		out = append(out, r.Name)
	}
	return out
}

func TestOverviewCountsEachIdentityOnceUnderItsRiskState(t *testing.T) {
	got := overview(t, riskBoardServer(t))

	counts := [5]int{got.KPI.ConflictedCount, got.KPI.BlockedCount, got.KPI.AtRiskCount, got.KPI.DriftedCount, got.KPI.CoordinatedCount}
	if want := [5]int{1, 2, 2, 1, 1}; counts != want {
		t.Errorf("conflicted/blocked/at-risk/drifted/coordinated = %v, want %v (split counts as Conflicted despite its OOM, gone is no identity)", counts, want)
	}

	for state, names := range map[riskState][]string{
		riskConflicted: {"split"},
		riskBlocked:    {"api", "worker"},
		riskAtRisk:     {"checkout", "db"},
		riskDrifted:    {"web"},
	} {
		if g := got.Attention[state]; !slices.Equal(attentionNames(g), names) || g.Total != len(names) {
			t.Errorf("attention[%s] = %v of %d, want %v worst first", state, attentionNames(g), g.Total, names)
		}
	}
	if d := got.Attention[riskConflicted].Rows[0].Detail; d != "p, q" {
		t.Errorf("conflicted row detail = %q, want the policies its members opt into", d)
	}

	if len(got.Policies) != 1 || got.Policies[0].BlockedCount != 2 {
		t.Errorf("policies = %+v, want p with its 2 Blocked identities", got.Policies)
	}
}

func TestOverviewAttentionListIsCappedButCountsEveryIdentity(t *testing.T) {
	objs := []client.Object{deploymentPolicy("p")}
	health := memHealthSignals{}
	for i := range maxAttentionRows + 5 {
		name := fmt.Sprintf("w%02d", i)
		objs = append(objs, deploymentOptingInto("p", "prod", name, "", time.Now()))
		health[identity("prod", "Deployment", name)] = identityHealth{StalePods: i + 1}
	}
	got := overview(t, identityServer(t, health, objs...))

	drift := got.Attention[riskDrifted]
	if len(drift.Rows) != maxAttentionRows || drift.Rows[0].Name != "w14" {
		t.Errorf("drift rows = %v, want the %d with the most stale pods, w14 first", attentionNames(drift), maxAttentionRows)
	}
	if drift.Total != maxAttentionRows+5 || got.KPI.DriftedCount != maxAttentionRows+5 {
		t.Errorf("drift total/KPI = %d/%d, want every drifted identity", drift.Total, got.KPI.DriftedCount)
	}
}

// The invariant every view of the board keeps: an Overview count is the
// Workloads list filtered by the same Risk state.
func TestOverviewCountsMatchTheWorkloadsList(t *testing.T) {
	srv := riskBoardServer(t)
	got := overview(t, srv)

	for state, count := range map[riskState]int{
		riskConflicted: got.KPI.ConflictedCount,
		riskBlocked:    got.KPI.BlockedCount,
		riskAtRisk:     got.KPI.AtRiskCount,
		riskDrifted:    got.KPI.DriftedCount,
	} {
		rec := httptest.NewRecorder()
		srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads?risk="+string(state), nil))
		var page struct {
			Total int `json:"total"`
		}
		decodeEnvelopeData(t, rec.Body, &page)
		if page.Total != count || got.Attention[state].Total != count {
			t.Errorf("%s: KPI %d, attention total %d, /api/workloads total %d, want all equal",
				state, count, got.Attention[state].Total, page.Total)
		}
	}
}
