package dashboard

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// workloadRow is what every list endpoint returns per identity; endpoints
// that need more embed it.
type workloadRow struct {
	Namespace           string               `json:"namespace"`
	Kind                string               `json:"kind"`
	Name                string               `json:"name"`
	Containers          []containerStatus    `json:"containers"`
	RiskState           riskState            `json:"riskState"`
	StalePods           int                  `json:"stalePods"`
	TotalPods           int                  `json:"totalPods"`
	AutoscalerPresent   bool                 `json:"autoscalerPresent"`
	CoordinationFactors *coordinationFactors `json:"coordinationFactors,omitempty"`
	Departed            bool                 `json:"departed"`
	LastSeenAt          string               `json:"lastSeenAt,omitempty"`

	conflicted bool
}

func (r *workloadRow) identity() promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: r.Namespace, OwnerKind: r.Kind, OwnerName: r.Name}
}

func (r *workloadRow) setHealth(h identityHealth) {
	h.Conflicted = r.conflicted
	r.RiskState = riskStateOf(h)
	r.StalePods = h.StalePods
	r.TotalPods = h.TotalPods
	r.AutoscalerPresent = h.AutoscalerPresent
}

// rowFor renders one identity. A Departed identity shows the containers of
// its stored snapshot and when its Recommendation was last refreshed.
func rowFor(id *inventory.Identity) workloadRow {
	row := workloadRow{
		Namespace:           id.Key.Namespace,
		Kind:                id.Key.OwnerKind,
		Name:                id.Key.OwnerName,
		Containers:          containerStatuses(id.Containers, id.InitContainers),
		CoordinationFactors: coordinationFactorsOf(id.Recommendation),
		Departed:            id.Departed(),
		conflicted:          id.Conflicted,
	}
	if id.Departed() {
		if seen := id.Recommendation.Status.ObservedAt; !seen.IsZero() {
			row.LastSeenAt = seen.UTC().Format(time.RFC3339)
		}
	}
	return row
}

type paginatedWorkloads struct {
	Items []workloadRow `json:"items"`
	Total int           `json:"total"`
	// Matched counts every identity the policy governs, before namespace and
	// search filters; Total is after them.
	Matched    int      `json:"matched"`
	Page       int      `json:"page"`
	PageSize   int      `json:"pageSize"`
	Namespaces []string `json:"namespaces"`
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
	search := strings.ToLower(q.Get("search"))
	sortKey, sortDesc, perr := parseSortParam(q, policyWorkloadSortKeys, "name")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	page, pageSize, perr := parsePageParams(q, 50, 200)
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	governed, err := s.governedBy(ctx, policy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing workloads: %v", err))
		return
	}
	workloads := make([]workloadRow, 0, len(governed))
	for _, id := range governed {
		workloads = append(workloads, rowFor(id))
	}

	matched := len(workloads)
	namespaces := uniqueValues(workloads, func(w workloadRow) string { return w.Namespace })

	// Narrow before the health decoration so it only covers returned rows.
	if nsFilter != "" {
		workloads = filterInPlace(workloads, func(w workloadRow) bool { return w.Namespace == nsFilter })
	}
	if search != "" {
		workloads = filterInPlace(workloads, func(w workloadRow) bool { return nameMatches(&w, search) })
	}
	applyHealth(ctx, s, workloads, identityRow)
	sortWorkloads(workloads, identityRow, rowOrder(sortKey), sortDesc)

	total := len(workloads)
	start, end := paginateRange(total, page, pageSize)

	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, paginatedWorkloads{
		Items:      workloads[start:end],
		Total:      total,
		Matched:    matched,
		Page:       page,
		PageSize:   pageSize,
		Namespaces: namespaces,
	})
}

// governedBy returns the identities policy governs, Departed ones included,
// reading only the namespaces and kinds it can govern.
func (s *Server) governedBy(ctx context.Context, policy *sustainv1alpha1.Policy) ([]*inventory.Identity, error) {
	kinds := managedKinds(policy)
	if len(kinds) == 0 {
		return nil, nil
	}
	snap, err := s.identities(ctx, inventory.Options{Namespaces: policy.Spec.Selector.Namespaces, Kinds: kinds})
	if err != nil {
		return nil, err
	}
	return snap.GovernedBy(policy.Name), nil
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

// applyHealth reads every row's identity health in one batch and overlays it,
// with its Risk state, onto each row in place. A failed read degrades to the
// signals that could be read.
func applyHealth[T any](ctx context.Context, s *Server, rows []T, rowOf func(*T) *workloadRow) {
	ids := make([]promclient.WorkloadIdentity, len(rows))
	for i := range rows {
		ids[i] = rowOf(&rows[i]).identity()
	}
	health, err := s.Health.forIdentities(ctx, ids)
	if err != nil {
		s.Logger.V(1).Info("reading identity health failed; rows show what could be read", "error", err.Error())
	}
	for i := range rows {
		rowOf(&rows[i]).setHealth(health[ids[i]])
	}
}

type allWorkloadSummary struct {
	workloadRow
	Automated  bool   `json:"automated"`
	PolicyName string `json:"policyName,omitempty"`
	// ConflictingPolicies names the Policies a Conflicted identity's members
	// opt into; no Policy governs it.
	ConflictingPolicies []string `json:"conflictingPolicies,omitempty"`
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

// workloadCounts splits identities three ways: governed by a Policy
// (Automated), Conflicted, and governed by none (Manual).
type workloadCounts struct {
	Total      int `json:"total"`
	Automated  int `json:"automated"`
	Manual     int `json:"manual"`
	Conflicted int `json:"conflicted"`
}

type allWorkloadFilters struct {
	namespace  string
	kind       string
	search     string
	automated  *bool
	departed   *bool
	risk       string
	autoscaler string
	sortKey    string
	sortDesc   bool
	page       int
	pageSize   int
}

var (
	policyWorkloadSortKeys = []string{"name", "namespace", "kind", "stalePods"}
	allWorkloadSortKeys    = append(slices.Clone(policyWorkloadSortKeys), "policyName")
)

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
	if f.departed, perr = parseBoolParam(q, "departed"); perr != nil {
		return f, perr
	}
	if f.risk, perr = parseEnumParam(q, "risk", riskStates); perr != nil {
		return f, perr
	}
	if f.autoscaler, perr = parseEnumParam(q, "autoscaler", []string{"has-autoscaler", "no-autoscaler"}); perr != nil {
		return f, perr
	}
	if f.sortKey, f.sortDesc, perr = parseSortParam(q, allWorkloadSortKeys, "name"); perr != nil {
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

	snap, err := s.identities(ctx, inventory.Options{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing workloads: %v", err))
		return
	}
	workloads := make([]allWorkloadSummary, 0, len(snap.Identities))
	for i := range snap.Identities {
		id := &snap.Identities[i]
		workloads = append(workloads, allWorkloadSummary{
			workloadRow:         rowFor(id),
			Automated:           id.Policy != "",
			PolicyName:          id.Policy,
			ConflictingPolicies: conflictingPolicies(id),
		})
	}

	// Facets come from the full, unfiltered list.
	namespaces := uniqueValues(workloads, func(w allWorkloadSummary) string { return w.Namespace })
	kinds := uniqueValues(workloads, func(w allWorkloadSummary) string { return w.Kind })

	// Narrow before the health decoration so it only covers returned rows.
	workloads = filterByNamespaceAndKind(workloads, filters)
	applyHealth(ctx, s, workloads, allRowOf)

	workloads = applyAllWorkloadFilters(workloads, filters)
	sortWorkloads(workloads, allRowOf, allWorkloadOrder(filters.sortKey), filters.sortDesc)

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

// filterByNamespaceAndKind applies the identity filters that do not depend on
// health decoration, so the health read sees already-narrowed rows.
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
	if f.departed != nil {
		want := *f.departed
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.Departed == want })
	}
	if f.search != "" {
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool {
			return nameMatches(&w.workloadRow, f.search)
		})
	}
	if f.risk != "" {
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return string(w.RiskState) == f.risk })
	}
	if f.autoscaler != "" {
		want := f.autoscaler == "has-autoscaler"
		workloads = filterInPlace(workloads, func(w allWorkloadSummary) bool { return w.AutoscalerPresent == want })
	}
	return workloads
}

func identityRow(w *workloadRow) *workloadRow { return w }

// nameMatches expects needle already lower-cased.
func nameMatches(w *workloadRow, needle string) bool {
	return strings.Contains(strings.ToLower(w.Name), needle)
}

func rowOrder(key string) func(a, b *workloadRow) int {
	return func(a, b *workloadRow) int {
		switch key {
		case "namespace":
			return cmp.Compare(a.Namespace, b.Namespace)
		case "kind":
			return cmp.Compare(a.Kind, b.Kind)
		case "stalePods":
			return cmp.Compare(a.StalePods, b.StalePods)
		default:
			return cmp.Compare(a.Name, b.Name)
		}
	}
}

func allWorkloadOrder(key string) func(a, b *allWorkloadSummary) int {
	if key == "policyName" {
		return func(a, b *allWorkloadSummary) int { return cmp.Compare(a.PolicyName, b.PolicyName) }
	}
	byRow := rowOrder(key)
	return func(a, b *allWorkloadSummary) int { return byRow(&a.workloadRow, &b.workloadRow) }
}

// sortWorkloads runs before pagination so a sort spans every page. Ties fall
// back to name/namespace/kind so page boundaries stay stable across requests.
func sortWorkloads[T any](items []T, rowOf func(*T) *workloadRow, primary func(a, b *T) int, desc bool) {
	slices.SortFunc(items, func(a, b T) int {
		ra, rb := rowOf(&a), rowOf(&b)
		c := cmp.Or(
			primary(&a, &b),
			cmp.Compare(ra.Name, rb.Name),
			cmp.Compare(ra.Namespace, rb.Namespace),
			cmp.Compare(ra.Kind, rb.Kind),
		)
		if desc {
			return -c
		}
		return c
	})
}

func countAllWorkloads(workloads []allWorkloadSummary) workloadCounts {
	c := workloadCounts{Total: len(workloads)}
	for _, w := range workloads {
		switch {
		case w.Automated:
			c.Automated++
		case w.conflicted:
			c.Conflicted++
		default:
			c.Manual++
		}
	}
	return c
}
