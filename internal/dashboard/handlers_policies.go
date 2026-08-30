package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

type policyListItem struct {
	Name            string                      `json:"name"`
	Namespaces      []string                    `json:"namespaces"`
	Update          sustainv1alpha1.UpdateTypes `json:"update"`
	Conditions      []conditionSummary          `json:"conditions"`
	CreatedAt       string                      `json:"createdAt"`
	WorkloadCount   int                         `json:"workloadCount"`
	CPUSavingsCores float64                     `json:"cpuSavingsCores"`
	MemSavingsBytes float64                     `json:"memSavingsBytes"`
	AtRiskCount     int                         `json:"atRiskCount"`
}

type conditionSummary struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type policyDetail struct {
	policyListItem
	Spec sustainv1alpha1.PolicySpec `json:"spec"`
}

// policyRollups holds the four per-policy gauge maps the list/detail handlers
// both need. Keyed by policy name.
type policyRollups struct {
	workloadCount   map[string]float64
	cpuSavingsCores map[string]float64
	memSavingsBytes map[string]float64
	atRiskCount     map[string]float64
}

func (s *Server) fetchPolicyRollups(ctx context.Context) policyRollups {
	wl, _ := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyWorkloadCount, "policy")
	cpu, _ := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyCPUSavingsCores, "policy")
	mem, _ := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyMemorySavingsBytes, "policy")
	risk, _ := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyAtRiskCount, "policy")
	return policyRollups{workloadCount: wl, cpuSavingsCores: cpu, memSavingsBytes: mem, atRiskCount: risk}
}

func policyListItemFor(p sustainv1alpha1.Policy, rollups policyRollups) policyListItem {
	return policyListItem{
		Name:            p.Name,
		Namespaces:      p.Spec.Selector.Namespaces,
		Update:          p.Spec.RightSizing.Update.Types,
		Conditions:      conditionSummariesFor(p),
		CreatedAt:       p.CreationTimestamp.Format("2006-01-02T15:04:05Z"),
		WorkloadCount:   int(rollups.workloadCount[p.Name]),
		CPUSavingsCores: rollups.cpuSavingsCores[p.Name],
		MemSavingsBytes: rollups.memSavingsBytes[p.Name],
		AtRiskCount:     int(rollups.atRiskCount[p.Name]),
	}
}

func conditionSummariesFor(p sustainv1alpha1.Policy) []conditionSummary {
	out := make([]conditionSummary, 0, len(p.Status.Conditions))
	for _, c := range p.Status.Conditions {
		out = append(out, conditionSummary{
			Type:    c.Type,
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return out
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()
	var list sustainv1alpha1.PolicyList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing policies: %s", err))
		return
	}

	rollups := s.fetchPolicyRollups(ctx)

	items := make([]policyListItem, 0, len(list.Items))
	for _, p := range list.Items {
		items = append(items, policyListItemFor(p, rollups))
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: name}, policy); err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("policy %q: %v", name, err))
		return
	}

	q := r.URL.Query()
	tr, perr := parseTimeRange(q, "30d", time.Now())
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	step, perr := parseStepParam(q, "1h")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	cpuSeries, _ := s.PromClient.QueryRange(ctx, fmt.Sprintf(`%s{policy=%q}`, promclient.MetricPolicyCPUSavingsCores, name), tr, step)
	memSeries, _ := s.PromClient.QueryRange(ctx, fmt.Sprintf(`%s{policy=%q}`, promclient.MetricPolicyMemorySavingsBytes, name), tr, step)
	if cpuSeries == nil {
		cpuSeries = []promclient.TimeValue{}
	}
	if memSeries == nil {
		memSeries = []promclient.TimeValue{}
	}

	rollups := s.fetchPolicyRollups(ctx)

	writeJSON(w, http.StatusOK, struct {
		policyDetail
		EffectivenessSeries map[string][]promclient.TimeValue `json:"effectivenessSeries"`
	}{
		policyDetail: policyDetail{
			policyListItem: policyListItemFor(*policy, rollups),
			Spec:           policy.Spec,
		},
		EffectivenessSeries: map[string][]promclient.TimeValue{"cpu": cpuSeries, "memory": memSeries},
	})
}

// policiesByName lists every Policy once and indexes it by name, for callers
// that need to check policymatch.Matches against a resolved policy name for
// many workloads without a *Policy already in hand (a single Get per row
// would scale with workload count instead of Policy count). Returns a
// non-nil map — possibly empty — on success; a nil return always means the
// List failed, which callers use to distinguish "no Policies exist" from
// "could not check".
func (s *Server) policiesByName(ctx context.Context) (map[string]*sustainv1alpha1.Policy, error) {
	var list sustainv1alpha1.PolicyList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]*sustainv1alpha1.Policy, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, nil
}
