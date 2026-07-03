package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// ---- Workloads scoped to one policy ----

type workloadSummary struct {
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

type paginatedWorkloads struct {
	Items      []workloadSummary `json:"items"`
	Total      int               `json:"total"`
	Page       int               `json:"page"`
	PageSize   int               `json:"pageSize"`
	Namespaces []string          `json:"namespaces"`
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

	// Namespace dropdown is derived from the full, unfiltered list.
	namespaces := uniqueNamespaces(workloads)

	// Narrow to the requested namespace before the (potentially N+1) signal
	// decoration so we only do that work for the rows we will return.
	if nsFilter != "" {
		workloads = filterByNamespace(workloads, nsFilter)
	}
	applyWorkloadSignals(ctx, s, workloads)

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

// listPolicyWorkloadRows gathers workload rows for every kind the policy
// opts in to. Per-kind list errors are logged and skipped.
func (s *Server) listPolicyWorkloadRows(ctx context.Context, policy *sustainv1alpha1.Policy, policyName string) []workloadSummary {
	out := []workloadSummary{}
	for _, kind := range supportedWorkloadKinds {
		if !kindEnabledInPolicy(policy, kind) {
			continue
		}
		entries, err := s.listWorkloadsOfKind(ctx, kind)
		if err != nil {
			s.Logger.Error(err, "failed to list workloads", "kind", kind, "policy", policyName)
			continue
		}
		for _, e := range entries {
			if e.PolicyAnnotation() != policyName {
				continue
			}
			out = append(out, workloadSummary{
				Namespace:  e.Namespace,
				Kind:       kind,
				Name:       e.Name,
				Containers: containerStatuses(e.Containers(), e.InitContainers()),
				Active:     true,
			})
		}
	}

	live := make(map[string]struct{}, len(out))
	for _, w := range out {
		live[workloadKey(w.Namespace, w.Kind, w.Name)] = struct{}{}
	}
	inactive, err := s.collectInactiveWorkloads(ctx, live,
		client.MatchingLabels{sustainv1alpha1.WLRPolicyLabel: policyName})
	if err != nil {
		s.Logger.Error(err, "failed to list retained WorkloadRecommendations", "policy", policyName)
		return out
	}
	for _, iw := range inactive {
		if iw.PolicyName != policyName { // defensive vs label drift
			continue
		}
		out = append(out, workloadSummary{
			Namespace:  iw.Namespace,
			Kind:       iw.Kind,
			Name:       iw.Name,
			Containers: iw.Containers,
			LastSeenAt: iw.LastSeenAt,
		})
	}
	return out
}

func uniqueNamespaces(workloads []workloadSummary) []string {
	return uniqueValues(workloads, func(w workloadSummary) string { return w.Namespace })
}

// uniqueValues collects the distinct, unordered values produced by valueOf over
// rows. It backs the per-kind unique-set helpers, which differ only in the row
// type and which field(s) they pull.
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

func filterByNamespace(workloads []workloadSummary, ns string) []workloadSummary {
	out := workloads[:0]
	for _, w := range workloads {
		if w.Namespace == ns {
			out = append(out, w)
		}
	}
	return out
}

// applyWorkloadSignals overlays Prometheus-derived signals onto each row.
func applyWorkloadSignals(ctx context.Context, s *Server, workloads []workloadSummary) {
	applySignals(ctx, s, workloads,
		func(w workloadSummary) string { return workloadKey(w.Namespace, w.Kind, w.Name) },
		func(w *workloadSummary, sig workloadSignals) {
			w.RiskState = sig.RiskState
			w.DriftPercent = sig.DriftPercent
			w.AutoscalerPresent = sig.AutoscalerPresent
			w.CoordinationFactors = sig.CoordinationFactors
		})
}

// applySignals batches the signal queries for every row and overlays the
// result back onto each row. keyOf derives the Prometheus workload key for a
// row; set copies the fetched signals onto a row in place. It is the shared
// body behind applyWorkloadSignals and applyAllWorkloadSignals, which differ
// only in their row struct type.
func applySignals[T any](ctx context.Context, s *Server, rows []T, keyOf func(T) string, set func(*T, workloadSignals)) {
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = keyOf(r)
	}
	signals := s.fetchWorkloadSignals(ctx, keys)
	for i := range rows {
		set(&rows[i], signals[keys[i]])
	}
}

// ---- All workloads (cluster-wide) ----

type allWorkloadSummary struct {
	Namespace           string               `json:"namespace"`
	Kind                string               `json:"kind"`
	Name                string               `json:"name"`
	Containers          []containerStatus    `json:"containers"`
	Automated           bool                 `json:"automated"`
	PolicyName          string               `json:"policyName,omitempty"`
	RiskState           string               `json:"riskState"` // safe | drifted | at-risk | blocked
	DriftPercent        float64              `json:"driftPercent"`
	AutoscalerPresent   bool                 `json:"autoscalerPresent"`
	CoordinationFactors *coordinationFactors `json:"coordinationFactors,omitempty"`
	Active              bool                 `json:"active"`
	LastSeenAt          string               `json:"lastSeenAt,omitempty"`
}

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

	// Namespace/kind dropdowns are derived from the full, unfiltered list so
	// picking one value doesn't collapse the other options (mirrors
	// handlePolicyWorkloads).
	namespaces, kinds := uniqueNamespacesAndKinds(workloads)

	// Narrow by namespace/kind before the signal decoration so that work only
	// covers the rows we may return.
	workloads = filterByNamespaceAndKind(workloads, filters)
	applyAllWorkloadSignals(ctx, s, workloads)

	// Filters that depend on signal data run after decoration.
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
// Namespace/kind narrowing happens in the handler after the facet sets are
// derived, so the dropdowns always reflect the full population.
func (s *Server) collectAllWorkloads(ctx context.Context) []allWorkloadSummary {
	out := []allWorkloadSummary{}
	for _, kind := range supportedWorkloadKinds {
		entries, err := s.listWorkloadsOfKind(ctx, kind)
		if err != nil {
			s.Logger.Error(err, "failed to list workloads", "kind", kind)
			continue
		}
		for _, e := range entries {
			policyName := e.PolicyAnnotation()
			out = append(out, allWorkloadSummary{
				Namespace:  e.Namespace,
				Kind:       kind,
				Name:       e.Name,
				Containers: containerStatuses(e.Containers(), e.InitContainers()),
				Automated:  policyName != "",
				PolicyName: policyName,
				Active:     true,
			})
		}
	}

	live := make(map[string]struct{}, len(out))
	for _, w := range out {
		live[workloadKey(w.Namespace, w.Kind, w.Name)] = struct{}{}
	}
	inactive, err := s.collectInactiveWorkloads(ctx, live)
	if err != nil {
		s.Logger.Error(err, "failed to list retained WorkloadRecommendations")
		return out
	}
	for _, iw := range inactive {
		out = append(out, allWorkloadSummary{
			Namespace:  iw.Namespace,
			Kind:       iw.Kind,
			Name:       iw.Name,
			Containers: iw.Containers,
			Automated:  true,
			PolicyName: iw.PolicyName,
			LastSeenAt: iw.LastSeenAt,
		})
	}
	return out
}

func applyAllWorkloadSignals(ctx context.Context, s *Server, workloads []allWorkloadSummary) {
	applySignals(ctx, s, workloads,
		func(w allWorkloadSummary) string { return workloadKey(w.Namespace, w.Kind, w.Name) },
		func(w *allWorkloadSummary, sig workloadSignals) {
			w.RiskState = sig.RiskState
			w.DriftPercent = sig.DriftPercent
			w.AutoscalerPresent = sig.AutoscalerPresent
			w.CoordinationFactors = sig.CoordinationFactors
		})
}

func uniqueNamespacesAndKinds(workloads []allWorkloadSummary) (namespaces, kinds []string) {
	seenNamespaces := make(map[string]struct{})
	seenKinds := make(map[string]struct{})
	for _, w := range workloads {
		seenNamespaces[w.Namespace] = struct{}{}
		seenKinds[w.Kind] = struct{}{}
	}
	namespaces = make([]string, 0, len(seenNamespaces))
	for ns := range seenNamespaces {
		namespaces = append(namespaces, ns)
	}
	kinds = make([]string, 0, len(seenKinds))
	for k := range seenKinds {
		kinds = append(kinds, k)
	}
	return
}

// filterByNamespaceAndKind applies the identity filters that don't depend on
// signal decoration. Kept separate from applyAllWorkloadFilters so the rows
// passed to the (potentially expensive) signal queries are already narrowed.
func filterByNamespaceAndKind(workloads []allWorkloadSummary, f allWorkloadFilters) []allWorkloadSummary {
	if f.namespace != "" {
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool { return w.Namespace == f.namespace })
	}
	if f.kind != "" {
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool { return w.Kind == f.kind })
	}
	return workloads
}

func applyAllWorkloadFilters(workloads []allWorkloadSummary, f allWorkloadFilters) []allWorkloadSummary {
	if f.automated != nil {
		want := *f.automated
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool { return w.Automated == want })
	}
	if f.active != nil {
		want := *f.active
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool { return w.Active == want })
	}
	if f.search != "" {
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool {
			return strings.Contains(strings.ToLower(w.Name), f.search)
		})
	}
	if f.risk != "" {
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool { return w.RiskState == f.risk })
	}
	if f.autoscaler != "" {
		want := f.autoscaler == "has-autoscaler"
		workloads = filterAllWorkloads(workloads, func(w allWorkloadSummary) bool { return w.AutoscalerPresent == want })
	}
	return workloads
}

func filterAllWorkloads(workloads []allWorkloadSummary, keep func(allWorkloadSummary) bool) []allWorkloadSummary {
	out := workloads[:0]
	for _, w := range workloads {
		if keep(w) {
			out = append(out, w)
		}
	}
	return out
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
