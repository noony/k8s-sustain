package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
)

// workloadRow is what every list endpoint returns per workload; endpoints
// that need more embed it.
type workloadRow struct {
	Namespace           string               `json:"namespace"`
	Kind                string               `json:"kind"`
	Name                string               `json:"name"`
	Containers          []containerStatus    `json:"containers"`
	RiskState           string               `json:"riskState"` // safe | drifted | at-risk | blocked
	DriftPercent        float64              `json:"driftPercent"`
	AutoscalerPresent   bool                 `json:"autoscalerPresent"`
	CoordinationFactors *coordinationFactors `json:"coordinationFactors,omitempty"`
	Active              bool                 `json:"active"`
	LastSeenAt          string               `json:"lastSeenAt,omitempty"`
}

func (r *workloadRow) key() string { return workloadKey(r.Namespace, r.Kind, r.Name) }

func (r *workloadRow) setSignals(sig workloadSignals) {
	r.RiskState = sig.RiskState
	r.DriftPercent = sig.DriftPercent
	r.AutoscalerPresent = sig.AutoscalerPresent
	r.CoordinationFactors = sig.CoordinationFactors
}

func liveRow(kind string, e workloadEntry) workloadRow {
	return workloadRow{
		Namespace:  e.Namespace,
		Kind:       kind,
		Name:       e.Name,
		Containers: containerStatuses(e.Containers(), e.InitContainers()),
		Active:     true,
	}
}

type paginatedWorkloads struct {
	Items      []workloadRow `json:"items"`
	Total      int           `json:"total"`
	Page       int           `json:"page"`
	PageSize   int           `json:"pageSize"`
	Namespaces []string      `json:"namespaces"`
}

func (s *Server) handlePolicyWorkloads(w http.ResponseWriter, r *http.Request, policyName string) {
	ctx := r.Context()

	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("policy %q: %v", policyName, err))
		return
	}

	q := r.URL.Query()
	nsFilter := q.Get("namespace")
	page, pageSize, perr := parsePageParams(q, 50, 200)
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	workloads := s.listPolicyWorkloadRows(ctx, policy, policyName)

	namespaces := uniqueValues(workloads, func(w workloadRow) string { return w.Namespace })

	// Narrow before the signal decoration so it only covers returned rows.
	if nsFilter != "" {
		workloads = filterInPlace(workloads, func(w workloadRow) bool { return w.Namespace == nsFilter })
	}
	applySignals(ctx, s, workloads, func(w *workloadRow) *workloadRow { return w })

	total := len(workloads)
	start, end := paginateRange(total, page, pageSize)

	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, paginatedWorkloads{
		Items:      workloads[start:end],
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		Namespaces: namespaces,
	})
}

// listPolicyWorkloadRows gathers a row for every live workload the policy
// manages, then the retained WorkloadRecommendations that are not live.
func (s *Server) listPolicyWorkloadRows(ctx context.Context, policy *sustainv1alpha1.Policy, policyName string) []workloadRow {
	out := []workloadRow{}
	s.forEachPolicyEntry(ctx, policy, policyName, func(kind string, e workloadEntry) {
		out = append(out, liveRow(kind, e))
	})

	inactive, err := s.collectInactiveWorkloads(ctx, liveKeys(out, func(w *workloadRow) *workloadRow { return w }),
		client.MatchingLabels{sustainv1alpha1.WLRPolicyLabel: policyName})
	if err != nil {
		s.Logger.Error(err, "failed to list retained WorkloadRecommendations", "policy", policyName)
		return out
	}
	for _, iw := range inactive {
		if iw.PolicyName != policyName { // defensive vs label drift
			continue
		}
		out = append(out, iw.workloadRow)
	}
	return out
}

// forEachPolicyEntry visits every live workload entry the policy manages, in
// supportedWorkloadKinds order. Per-kind list errors are logged and skipped.
// Namespace annotations are fetched once, before the per-kind loop;
// entryMatchesPolicy is the sole gate, since opting in is necessary but not
// sufficient.
func (s *Server) forEachPolicyEntry(ctx context.Context, policy *sustainv1alpha1.Policy, policyName string, visit func(kind string, e workloadEntry)) {
	nsAnnotations, err := s.namespaceAnnotations(ctx)
	if err != nil {
		s.Logger.Error(err, "failed to list namespaces; namespace-level policy opt-in will not be resolved", "policy", policyName)
		nsAnnotations = nil
	}
	for _, kind := range supportedWorkloadKinds {
		if !kindEnabledInPolicy(policy, kind) {
			continue
		}
		entries, err := s.listWorkloadsOfKind(ctx, kind, nsAnnotations)
		if err != nil {
			s.Logger.Error(err, "failed to list workloads", "kind", kind, "policy", policyName)
			continue
		}
		for _, e := range entries {
			if entryMatchesPolicy(policy, e, s.ExcludedNamespaces) {
				visit(kind, e)
			}
		}
	}
}

// uniqueValues collects the distinct, unordered values of valueOf over rows.
func uniqueValues[T any](rows []T, valueOf func(T) string) []string {
	seen := make(map[string]struct{})
	for _, r := range rows {
		seen[valueOf(r)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	return out
}

// filterInPlace keeps the rows keep accepts, reusing the input's backing array.
func filterInPlace[T any](rows []T, keep func(T) bool) []T {
	out := rows[:0]
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// liveKeys returns the workloadKey set of rows, for collectInactiveWorkloads.
func liveKeys[T any](rows []T, rowOf func(*T) *workloadRow) map[string]struct{} {
	live := make(map[string]struct{}, len(rows))
	for i := range rows {
		live[rowOf(&rows[i]).key()] = struct{}{}
	}
	return live
}

// applySignals batches the signal queries for every row and overlays the
// result onto each row in place.
func applySignals[T any](ctx context.Context, s *Server, rows []T, rowOf func(*T) *workloadRow) {
	keys := make([]string, len(rows))
	for i := range rows {
		keys[i] = rowOf(&rows[i]).key()
	}
	signals := s.fetchWorkloadSignals(ctx, keys)
	for i := range rows {
		rowOf(&rows[i]).setSignals(signals[keys[i]])
	}
}

type allWorkloadSummary struct {
	workloadRow
	Automated  bool   `json:"automated"`
	PolicyName string `json:"policyName,omitempty"`
}

func allRowOf(w *allWorkloadSummary) *workloadRow { return &w.workloadRow }

type paginatedAllWorkloads struct {
	Items      []allWorkloadSummary `json:"items"`
	Total      int                  `json:"total"`
	Page       int                  `json:"page"`
	PageSize   int                  `json:"pageSize"`
	Namespaces []string             `json:"namespaces"`
	Kinds      []string             `json:"kinds"`
	Counts     workloadCounts       `json:"counts"`
}

type workloadCounts struct {
	Total     int `json:"total"`
	Automated int `json:"automated"`
	Manual    int `json:"manual"`
}

type allWorkloadFilters struct {
	namespace  string
	kind       string
	search     string
	automated  *bool
	active     *bool
	risk       string
	autoscaler string
	page       int
	pageSize   int
}

func parseAllWorkloadFilters(q url.Values) (allWorkloadFilters, *paramError) {
	f := allWorkloadFilters{
		namespace: q.Get("namespace"),
		search:    strings.ToLower(q.Get("search")),
	}
	var perr *paramError
	if f.kind, perr = parseEnumParam(q, "kind", supportedWorkloadKinds); perr != nil {
		return f, perr
	}
	if f.automated, perr = parseBoolParam(q, "automated"); perr != nil {
		return f, perr
	}
	if f.active, perr = parseBoolParam(q, "active"); perr != nil {
		return f, perr
	}
	if f.risk, perr = parseEnumParam(q, "risk", []string{"safe", "drifted", "at-risk", "blocked"}); perr != nil {
		return f, perr
	}
	if f.autoscaler, perr = parseEnumParam(q, "autoscaler", []string{"has-autoscaler", "no-autoscaler"}); perr != nil {
		return f, perr
	}
	if f.page, f.pageSize, perr = parsePageParams(q, 50, 200); perr != nil {
		return f, perr
	}
	return f, nil
}

func (s *Server) handleAllWorkloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()
	filters, perr := parseAllWorkloadFilters(r.URL.Query())
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	workloads := s.collectAllWorkloads(ctx)

	// Facets come from the full, unfiltered list.
	namespaces := uniqueValues(workloads, func(w allWorkloadSummary) string { return w.Namespace })
	kinds := uniqueValues(workloads, func(w allWorkloadSummary) string { return w.Kind })

	// Narrow before the signal decoration so it only covers returned rows.
	workloads = filterByNamespaceAndKind(workloads, filters)
	applySignals(ctx, s, workloads, allRowOf)

	workloads = applyAllWorkloadFilters(workloads, filters)

	counts := countAllWorkloads(workloads)
	total := len(workloads)
	start, end := paginateRange(total, filters.page, filters.pageSize)

	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, paginatedAllWorkloads{
		Items:      workloads[start:end],
		Total:      total,
		Page:       filters.page,
		PageSize:   filters.pageSize,
		Namespaces: namespaces,
		Kinds:      kinds,
		Counts:     counts,
	})
}

// collectAllWorkloads lists workloads of every supported kind, cluster-wide.
// Namespace/kind narrowing happens in the handler after the facets are
// derived. Namespace annotations are fetched once, before the per-kind loop.
func (s *Server) collectAllWorkloads(ctx context.Context) []allWorkloadSummary {
	nsAnnotations, err := s.namespaceAnnotations(ctx)
	if err != nil {
		s.Logger.Error(err, "failed to list namespaces; namespace-level policy opt-in will not be resolved")
		nsAnnotations = nil
	}
	// A Policies List failure degrades to nil (a successful empty List is a
	// non-nil map) and falls back to each workload's own opt-in, rather than
	// marking every workload unmanaged over a transient error.
	policies, err := s.policiesByName(ctx)
	if err != nil {
		s.Logger.Error(err, "failed to list policies; Policy selector consent will not be checked, falling back to each workload's own opt-in")
		policies = nil
	}
	out := []allWorkloadSummary{}
	for _, kind := range supportedWorkloadKinds {
		entries, err := s.listWorkloadsOfKind(ctx, kind, nsAnnotations)
		if err != nil {
			s.Logger.Error(err, "failed to list workloads", "kind", kind)
			continue
		}
		for _, e := range entries {
			var policyName string
			var automated bool
			if policies != nil {
				policyName, automated = resolveManagingPolicy(e, policies, s.ExcludedNamespaces)
			} else {
				policyName = e.ResolvedPolicy()
				automated = policyName != ""
			}
			out = append(out, allWorkloadSummary{
				workloadRow: liveRow(kind, e),
				Automated:   automated,
				PolicyName:  policyName,
			})
		}
	}

	inactive, err := s.collectInactiveWorkloads(ctx, liveKeys(out, allRowOf))
	if err != nil {
		s.Logger.Error(err, "failed to list retained WorkloadRecommendations")
		return out
	}
	for _, iw := range inactive {
		out = append(out, allWorkloadSummary{
			workloadRow: iw.workloadRow,
			Automated:   true,
			PolicyName:  iw.PolicyName,
		})
	}
	return out
}

// resolveManagingPolicy returns the first member of e whose resolved policy
// also matches that same member, looked up in policies. Returns ("", false)
// when no member's opt-in survives its own Policy's selector.
func resolveManagingPolicy(e workloadEntry, policies map[string]*sustainv1alpha1.Policy, excludedNamespaces []string) (string, bool) {
	if len(e.Members) == 0 {
		name := e.ResolvedPolicy()
		if name == "" {
			return "", false
		}
		p, ok := policies[name]
		if !ok || !entryMatchesPolicy(p, e, excludedNamespaces) {
			return "", false
		}
		return name, true
	}
	for _, m := range e.Members {
		name, _ := policymatch.ResolvePolicy(m.TemplateAnnotations, m.ObjectAnnotations, e.NamespaceAnnotations)
		if name == "" {
			continue
		}
		p, ok := policies[name]
		if !ok {
			continue
		}
		if policymatch.Matches(p, e.Namespace, m.Labels, excludedNamespaces) {
			return name, true
		}
	}
	return "", false
}

// filterByNamespaceAndKind applies the identity filters that do not depend on
// signal decoration, so the signal queries see already-narrowed rows.
func filterByNamespaceAndKind(workloads []allWorkloadSummary, f allWorkloadFilters) []allWorkloadSummary {
	if f.namespace != "" {
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.Namespace == f.namespace })
	}
	if f.kind != "" {
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.Kind == f.kind })
	}
	return workloads
}

func applyAllWorkloadFilters(workloads []allWorkloadSummary, f allWorkloadFilters) []allWorkloadSummary {
	if f.automated != nil {
		want := *f.automated
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.Automated == want })
	}
	if f.active != nil {
		want := *f.active
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.Active == want })
	}
	if f.search != "" {
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool {
			return strings.Contains(strings.ToLower(w.Name), f.search)
		})
	}
	if f.risk != "" {
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.RiskState == f.risk })
	}
	if f.autoscaler != "" {
		want := f.autoscaler == "has-autoscaler"
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.AutoscalerPresent == want })
	}
	return workloads
}

func countAllWorkloads(workloads []allWorkloadSummary) workloadCounts {
	c := workloadCounts{Total: len(workloads)}
	for _, w := range workloads {
		if w.Automated {
			c.Automated++
		} else {
			c.Manual++
		}
	}
	return c
}
