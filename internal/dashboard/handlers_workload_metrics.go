package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// ---- Workload recommendations ----

type recommendationResult struct {
	Automated          bool                                 `json:"automated"`
	PolicyName         string                               `json:"policyName,omitempty"`
	Containers         map[string]simulationContainerResult `json:"containers,omitempty"`
	InitContainers     []string                             `json:"initContainers,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

func (s *Server) handleWorkloadRecommendations(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	ctx := r.Context()

	// Fetch the workload entry once: it supplies the policy annotation here and
	// is threaded into runSimulationWithEntry below so the simulation does not
	// re-Get the same object.
	entry, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("workload %s/%s/%s: %v", namespace, kind, name, err))
		return
	}

	// Opting in is necessary but not sufficient: the candidate Policy's own
	// selector (or --excluded-namespaces) may not reach this workload — the
	// same check listPolicyWorkloadRows, collectPolicyWorkloads and
	// collectAllWorkloads apply before ever reporting a workload as managed.
	// For a grouped identity (entry.Members set), the candidate policy name
	// itself can differ per real object behind the identity, so picking one
	// name off the representative alone and gating only that name — the bug
	// this endpoint used to have — can disagree with /api/workloads, which
	// searches every member. resolveManagingPolicy runs that same full
	// member search here, off one Policy List, so both endpoints ask
	// exactly the same question: is there some real object o behind this
	// identity with ResolvePolicy(o) == p.Name AND Matches(p, o) — and reach
	// the same verdict for it.
	//
	// Entries synthesized from a retained WorkloadRecommendation carry no
	// ObjectLabels/Members (see workloadEntry.FromRetainedWLR), so
	// resolveManagingPolicy's single-entry fallback (len(Members) == 0)
	// applies, which in turn skips only the label half of the gate via
	// entryMatchesPolicy: the WLR's Spec.Policy already IS the controller's
	// verdict that this workload matched its LabelSelector, and evaluating
	// that selector against an empty label set would wrongly report a
	// departed, still-in-window workload as unmanaged. The namespace half
	// (--excluded-namespaces, Policy.Spec.Selector.Namespaces) is still
	// evaluated against entry.Namespace, which these entries do carry.
	//
	// A Policy List failure fails the request outright, unlike
	// collectAllWorkloads (which degrades to trusting each workload's own
	// opt-in for its cluster-wide list view, documented there as a
	// deliberate list-view trade-off): silently reporting Automated: false
	// here would blame an apiserver problem on the workload being
	// unmanaged, which is a different lie than the one this gate exists to
	// prevent.
	policies, err := s.policiesByName(ctx)
	if err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("listing policies: %v", err))
		return
	}
	policyName, ok := resolveManagingPolicy(entry, policies, s.ExcludedNamespaces)
	if !ok {
		w.Header().Set("Cache-Control", "public, max-age=60")
		writeJSON(w, http.StatusOK, recommendationResult{Automated: false})
		return
	}
	policy := policies[policyName]

	req, perr := buildSimulateRequestFromPolicy(r.URL.Query(), policy, namespace, kind, name)
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	result, err := s.runSimulationWithEntry(ctx, req, entry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("computing recommendations: %v", err))
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, recommendationResult{
		Automated:          true,
		PolicyName:         policyName,
		Containers:         result.Containers,
		InitContainers:     result.InitContainers,
		CPURecommendations: result.CPURecommendations,
		MemRecommendations: result.MemRecommendations,
	})
}

// buildSimulateRequestFromPolicy fills out a simulateRequest using a policy's
// resource configs as defaults, then overrides the chart window/step from the
// query string. Returns a paramError on invalid query input.
func buildSimulateRequestFromPolicy(q url.Values, policy *sustainv1alpha1.Policy, namespace, kind, name string) (simulateRequest, *paramError) {
	cpuCfg := policy.Spec.RightSizing.ResourcesConfigs.CPU
	memCfg := policy.Spec.RightSizing.ResourcesConfigs.Memory

	cpuWindow := cpuCfg.Window
	if cpuWindow == "" {
		cpuWindow = "168h"
	}
	memWindow := memCfg.Window
	if memWindow == "" {
		memWindow = "168h"
	}

	chartWindow, perr := parseDurationParam(q, cpuWindow)
	if perr != nil {
		return simulateRequest{}, perr
	}
	step, perr := parseStepParam(q, "5m")
	if perr != nil {
		return simulateRequest{}, perr
	}

	req := simulateRequest{
		Namespace: namespace,
		OwnerKind: kind,
		OwnerName: name,
		Window:    chartWindow,
		Step:      step,
		CPU: simulateResourceConfig{
			Percentile: cpuCfg.Requests.Percentile,
			Headroom:   cpuCfg.Requests.Headroom,
			Window:     cpuWindow,
		},
		Memory: simulateResourceConfig{
			Percentile: memCfg.Requests.Percentile,
			Headroom:   memCfg.Requests.Headroom,
			Window:     memWindow,
		},
	}
	if cpuCfg.Requests.MinAllowed != nil {
		v := cpuCfg.Requests.MinAllowed.String()
		req.CPU.MinAllowed = &v
	}
	if cpuCfg.Requests.MaxAllowed != nil {
		v := cpuCfg.Requests.MaxAllowed.String()
		req.CPU.MaxAllowed = &v
	}
	if memCfg.Requests.MinAllowed != nil {
		v := memCfg.Requests.MinAllowed.String()
		req.Memory.MinAllowed = &v
	}
	if memCfg.Requests.MaxAllowed != nil {
		v := memCfg.Requests.MaxAllowed.String()
		req.Memory.MaxAllowed = &v
	}
	if v := q.Get("from"); v != "" {
		req.FromTs, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("to"); v != "" {
		req.ToTs, _ = strconv.ParseInt(v, 10, 64)
	}
	return req, nil
}

// ---- Workload metrics ----

func (s *Server) handleWorkloadMetrics(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	q := r.URL.Query()
	tr, perr := parseTimeRange(q, "168h", time.Now())
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	step, perr := parseStepParam(q, "5m")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	ctx := r.Context()

	// Fan out the seven Prometheus queries so chart-load latency is bounded
	// by the slowest single query, not their sum. cpu/memory usage are the
	// only two whose failure aborts the response; the rest are best-effort
	// supplementary series and tolerate a Prometheus blip.
	var (
		cpuSeries, memSeries                           promclient.ContainerTimeSeries
		cpuRequests, memRequests, cpuLimits, memLimits promclient.ContainerTimeSeries
		oomEvents                                      []promclient.OOMEvent
		cpuErr, memErr                                 error
	)
	var wg sync.WaitGroup
	wg.Go(func() {
		cpuSeries, cpuErr = s.PromClient.QueryCPURangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		memSeries, memErr = s.PromClient.QueryMemoryRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		oomEvents, _ = s.PromClient.QueryOOMKillEvents(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		cpuRequests, _ = s.PromClient.QueryCPURequestRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		memRequests, _ = s.PromClient.QueryMemoryRequestRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		cpuLimits, _ = s.PromClient.QueryCPULimitRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		memLimits, _ = s.PromClient.QueryMemoryLimitRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Wait()

	if cpuErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("cpu range query: %v", cpuErr))
		return
	}
	if memErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("memory range query: %v", memErr))
		return
	}

	// Fetch the workload entry once and derive both resources and init-container
	// names from it. A failed Get is tolerated (nil results), matching the prior
	// behavior where each helper swallowed its own error.
	entry, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		s.Logger.Error(err, "failed to get workload entry", "namespace", namespace, "kind", kind, "name", name)
		entry = workloadEntry{}
	}
	resources := containerResourcesFromEntry(entry)
	initContainers := initContainerNamesFromEntry(entry)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"cpu":            cpuSeries,
		"memory":         memSeries,
		"resources":      resources,
		"cpuRequests":    cpuRequests,
		"memoryRequests": memRequests,
		"cpuLimits":      cpuLimits,
		"memoryLimits":   memLimits,
		"oomEvents":      oomEvents,
		"initContainers": initContainers,
	})
}

// ---- Container resources / helpers used by metrics + simulate handlers ----

type containerResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// getWorkloadPolicyAnnotation resolves the Policy that manages a workload —
// some real object behind its identity both opts into the Policy
// (policymatch.ResolvePolicy) AND satisfies that Policy's own selector or
// --excluded-namespaces (policymatch.Matches), evaluated on that SAME
// object — the same check listPolicyWorkloadRows, collectPolicyWorkloads,
// collectAllWorkloads and handleWorkloadRecommendations apply before ever
// reporting a workload as managed. An opted-in workload the Policy's own
// selector does not reach is reported as unmanaged ("", nil), not as managed
// under a Policy that never claimed it. For a grouped identity
// (workloadEntry.Members), resolveManagingPolicy searches every member
// rather than only the representative, so this agrees with /api/workloads
// even when different real objects behind the identity opt into different
// Policies.
//
// Entries synthesized from a retained WorkloadRecommendation
// (workloadEntry.FromRetainedWLR) carry no ObjectLabels/Members, so
// resolveManagingPolicy's single-entry fallback applies, which in turn skips
// only the label half of the gate via entryMatchesPolicy: the WLR's own
// Spec.Policy — the controller's last verdict on the LabelSelector — is used
// as-is instead of being wrongly re-derived from an empty label set. The
// namespace half (--excluded-namespaces, Policy.Spec.Selector.Namespaces) is
// still evaluated, since entry.Namespace is available. See the longer
// comment in handleWorkloadRecommendations.
func (s *Server) getWorkloadPolicyAnnotation(ctx context.Context, namespace, kind, name string) (string, error) {
	e, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		return "", err
	}
	policies, err := s.policiesByName(ctx)
	if err != nil {
		return "", err
	}
	policyName, ok := resolveManagingPolicy(e, policies, s.ExcludedNamespaces)
	if !ok {
		return "", nil
	}
	return policyName, nil
}

func containerResourcesFromEntry(e workloadEntry) map[string]containerResources {
	all := append([]corev1.Container{}, e.Containers()...)
	all = append(all, e.InitContainers()...)
	if len(all) == 0 {
		// Preserve the nil map returned when the workload could not be resolved
		// (no template / failed Get), so the JSON stays `null` rather than `{}`.
		return nil
	}
	result := make(map[string]containerResources, len(all))
	for _, c := range all {
		cpuReq, cpuLim, memReq, memLim := resourceStrings(c)
		result[c.Name] = containerResources{
			CPURequest:    cpuReq,
			CPULimit:      cpuLim,
			MemoryRequest: memReq,
			MemoryLimit:   memLim,
		}
	}
	return result
}

func initContainerNamesFromEntry(e workloadEntry) []string {
	initCs := e.InitContainers()
	if len(initCs) == 0 {
		return nil
	}
	out := make([]string, len(initCs))
	for i, c := range initCs {
		out[i] = c.Name
	}
	return out
}
