package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// ---- Policy types ----

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

type workloadSummary struct {
	Namespace  string            `json:"namespace"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Containers []containerStatus `json:"containers"`
}

type containerStatus struct {
	Name          string `json:"name"`
	CPURequest    string `json:"cpuRequest"`
	CPULimit      string `json:"cpuLimit"`
	MemoryRequest string `json:"memoryRequest"`
	MemoryLimit   string `json:"memoryLimit"`
}

// ---- Handlers ----

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=30")

	ctx := r.Context()
	var list sustainv1alpha1.PolicyList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing policies: %s", err))
		return
	}

	wl, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain_policy_workload_count", "policy")
	cpu, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain:policy_cpu_savings_cores", "policy")
	mem, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain:policy_memory_savings_bytes", "policy")
	risk, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain_policy_at_risk_count", "policy")

	items := make([]policyListItem, 0, len(list.Items))
	for _, p := range list.Items {
		conditions := make([]conditionSummary, 0, len(p.Status.Conditions))
		for _, c := range p.Status.Conditions {
			conditions = append(conditions, conditionSummary{
				Type:    c.Type,
				Status:  string(c.Status),
				Reason:  c.Reason,
				Message: c.Message,
			})
		}
		items = append(items, policyListItem{
			Name:            p.Name,
			Namespaces:      p.Spec.Selector.Namespaces,
			Update:          p.Spec.Update.Types,
			Conditions:      conditions,
			CreatedAt:       p.CreationTimestamp.Format("2006-01-02T15:04:05Z"),
			WorkloadCount:   int(wl[p.Name]),
			CPUSavingsCores: cpu[p.Name],
			MemSavingsBytes: mem[p.Name],
			AtRiskCount:     int(risk[p.Name]),
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handlePolicyRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	parts := parsePath(r.URL.Path, "/api/policies/")
	if len(parts) == 0 {
		writeError(w, http.StatusBadRequest, "missing policy name")
		return
	}

	policyName := parts[0]

	if len(parts) == 1 {
		s.handlePolicyDetail(w, r, policyName)
		return
	}
	if len(parts) == 2 && parts[1] == "workloads" {
		s.handlePolicyWorkloads(w, r, policyName)
		return
	}
	if len(parts) == 2 && parts[1] == "batch-simulate" {
		s.handlePolicyBatchSimulate(w, r, policyName)
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: name}, policy); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("policy %q not found: %v", name, err))
		return
	}

	conditions := make([]conditionSummary, 0, len(policy.Status.Conditions))
	for _, c := range policy.Status.Conditions {
		conditions = append(conditions, conditionSummary{
			Type:    c.Type,
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	window := r.URL.Query().Get("window")
	if window == "" {
		window = "30d"
	}
	step := r.URL.Query().Get("step")
	if step == "" {
		step = "1h"
	}

	cpuSeries, _ := s.PromClient.QueryRange(ctx, fmt.Sprintf(`k8s_sustain:policy_cpu_savings_cores{policy=%q}`, name), window, step)
	memSeries, _ := s.PromClient.QueryRange(ctx, fmt.Sprintf(`k8s_sustain:policy_memory_savings_bytes{policy=%q}`, name), window, step)
	if cpuSeries == nil {
		cpuSeries = []promclient.TimeValue{}
	}
	if memSeries == nil {
		memSeries = []promclient.TimeValue{}
	}

	// Query the policy-level rollup gauges so the detail view also has them.
	wl, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain_policy_workload_count", "policy")
	cpuByPolicy, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain:policy_cpu_savings_cores", "policy")
	memByPolicy, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain:policy_memory_savings_bytes", "policy")
	risk, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain_policy_at_risk_count", "policy")

	writeJSON(w, http.StatusOK, struct {
		policyDetail
		EffectivenessSeries map[string][]promclient.TimeValue `json:"effectivenessSeries"`
	}{
		policyDetail: policyDetail{
			policyListItem: policyListItem{
				Name:            policy.Name,
				Namespaces:      policy.Spec.Selector.Namespaces,
				Update:          policy.Spec.Update.Types,
				Conditions:      conditions,
				CreatedAt:       policy.CreationTimestamp.Format("2006-01-02T15:04:05Z"),
				WorkloadCount:   int(wl[name]),
				CPUSavingsCores: cpuByPolicy[name],
				MemSavingsBytes: memByPolicy[name],
				AtRiskCount:     int(risk[name]),
			},
			Spec: policy.Spec,
		},
		EffectivenessSeries: map[string][]promclient.TimeValue{"cpu": cpuSeries, "memory": memSeries},
	})
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
		writeError(w, http.StatusNotFound, fmt.Sprintf("policy %q not found: %v", policyName, err))
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=30")

	// Query params
	nsFilter := r.URL.Query().Get("namespace")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}

	var workloads []workloadSummary

	if policy.Spec.Update.Types.Deployment != nil {
		wl, err := s.listDeploymentWorkloads(ctx, policyName)
		if err != nil {
			s.Logger.Error(err, "failed to list deployments", "policy", policyName)
		} else {
			workloads = append(workloads, wl...)
		}
	}
	if policy.Spec.Update.Types.StatefulSet != nil {
		wl, err := s.listStatefulSetWorkloads(ctx, policyName)
		if err != nil {
			s.Logger.Error(err, "failed to list statefulsets", "policy", policyName)
		} else {
			workloads = append(workloads, wl...)
		}
	}
	if policy.Spec.Update.Types.DaemonSet != nil {
		wl, err := s.listDaemonSetWorkloads(ctx, policyName)
		if err != nil {
			s.Logger.Error(err, "failed to list daemonsets", "policy", policyName)
		} else {
			workloads = append(workloads, wl...)
		}
	}
	if policy.Spec.Update.Types.CronJob != nil {
		wl, err := s.listCronJobWorkloads(ctx, policyName)
		if err != nil {
			s.Logger.Error(err, "failed to list cronjobs", "policy", policyName)
		} else {
			workloads = append(workloads, wl...)
		}
	}

	if workloads == nil {
		workloads = []workloadSummary{}
	}

	// Collect unique namespaces before filtering
	nsSet := make(map[string]struct{})
	for _, w := range workloads {
		nsSet[w.Namespace] = struct{}{}
	}
	namespaces := make([]string, 0, len(nsSet))
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}

	// Apply namespace filter
	if nsFilter != "" {
		filtered := workloads[:0]
		for _, w := range workloads {
			if w.Namespace == nsFilter {
				filtered = append(filtered, w)
			}
		}
		workloads = filtered
	}

	total := len(workloads)

	// Paginate
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	writeJSON(w, http.StatusOK, paginatedWorkloads{
		Items:      workloads[start:end],
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		Namespaces: namespaces,
	})
}

func (s *Server) listDeploymentWorkloads(ctx context.Context, policyName string) ([]workloadSummary, error) {
	var list appsv1.DeploymentList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []workloadSummary
	for _, d := range list.Items {
		if d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policyName {
			continue
		}
		out = append(out, workloadSummary{
			Namespace:  d.Namespace,
			Kind:       "Deployment",
			Name:       d.Name,
			Containers: containerStatuses(d.Spec.Template.Spec.Containers),
		})
	}
	return out, nil
}

func (s *Server) listStatefulSetWorkloads(ctx context.Context, policyName string) ([]workloadSummary, error) {
	var list appsv1.StatefulSetList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []workloadSummary
	for _, st := range list.Items {
		if st.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policyName {
			continue
		}
		out = append(out, workloadSummary{
			Namespace:  st.Namespace,
			Kind:       "StatefulSet",
			Name:       st.Name,
			Containers: containerStatuses(st.Spec.Template.Spec.Containers),
		})
	}
	return out, nil
}

func (s *Server) listDaemonSetWorkloads(ctx context.Context, policyName string) ([]workloadSummary, error) {
	var list appsv1.DaemonSetList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []workloadSummary
	for _, ds := range list.Items {
		if ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policyName {
			continue
		}
		out = append(out, workloadSummary{
			Namespace:  ds.Namespace,
			Kind:       "DaemonSet",
			Name:       ds.Name,
			Containers: containerStatuses(ds.Spec.Template.Spec.Containers),
		})
	}
	return out, nil
}

func (s *Server) listCronJobWorkloads(ctx context.Context, policyName string) ([]workloadSummary, error) {
	var list batchv1.CronJobList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []workloadSummary
	for _, cj := range list.Items {
		if cj.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policyName {
			continue
		}
		out = append(out, workloadSummary{
			Namespace:  cj.Namespace,
			Kind:       "CronJob",
			Name:       cj.Name,
			Containers: containerStatuses(cj.Spec.JobTemplate.Spec.Template.Spec.Containers),
		})
	}
	return out, nil
}

func containerStatuses(containers []corev1.Container) []containerStatus {
	out := make([]containerStatus, 0, len(containers))
	for _, c := range containers {
		cs := containerStatus{Name: c.Name}
		if req := c.Resources.Requests; req != nil {
			if cpu := req.Cpu(); cpu != nil && !cpu.IsZero() {
				cs.CPURequest = cpu.String()
			}
			if mem := req.Memory(); mem != nil && !mem.IsZero() {
				cs.MemoryRequest = mem.String()
			}
		}
		if lim := c.Resources.Limits; lim != nil {
			if cpu := lim.Cpu(); cpu != nil && !cpu.IsZero() {
				cs.CPULimit = cpu.String()
			}
			if mem := lim.Memory(); mem != nil && !mem.IsZero() {
				cs.MemoryLimit = mem.String()
			}
		}
		out = append(out, cs)
	}
	return out
}

// ---- All workloads (cluster-wide) ----

type allWorkloadSummary struct {
	Namespace      string            `json:"namespace"`
	Kind           string            `json:"kind"`
	Name           string            `json:"name"`
	Containers     []containerStatus `json:"containers"`
	Automated      bool              `json:"automated"`
	PolicyName     string            `json:"policyName,omitempty"`
	RiskState      string            `json:"riskState"` // safe | drifted | at-risk | blocked
	DriftPercent   float64           `json:"driftPercent"`
	LastRecycledAt string            `json:"lastRecycledAt,omitempty"`
	HPAPresent     bool              `json:"hpaPresent"`
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

func (s *Server) handleAllWorkloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=30")

	ctx := r.Context()
	nsFilter := r.URL.Query().Get("namespace")
	kindFilter := r.URL.Query().Get("kind")
	automatedFilter := r.URL.Query().Get("automated")
	search := strings.ToLower(r.URL.Query().Get("search"))
	riskFilter := r.URL.Query().Get("risk")
	hpaFilter := r.URL.Query().Get("hpa")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}

	var listOpts []client.ListOption
	if nsFilter != "" {
		listOpts = append(listOpts, client.InNamespace(nsFilter))
	}

	var workloads []allWorkloadSummary

	if kindFilter == "" || kindFilter == "Deployment" {
		var list appsv1.DeploymentList
		if err := s.K8sClient.List(ctx, &list, listOpts...); err != nil {
			s.Logger.Error(err, "failed to list deployments")
		} else {
			for _, d := range list.Items {
				policyName := d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]
				workloads = append(workloads, allWorkloadSummary{
					Namespace:  d.Namespace,
					Kind:       "Deployment",
					Name:       d.Name,
					Containers: containerStatuses(d.Spec.Template.Spec.Containers),
					Automated:  policyName != "",
					PolicyName: policyName,
				})
			}
		}
	}
	if kindFilter == "" || kindFilter == "StatefulSet" {
		var list appsv1.StatefulSetList
		if err := s.K8sClient.List(ctx, &list, listOpts...); err != nil {
			s.Logger.Error(err, "failed to list statefulsets")
		} else {
			for _, st := range list.Items {
				policyName := st.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]
				workloads = append(workloads, allWorkloadSummary{
					Namespace:  st.Namespace,
					Kind:       "StatefulSet",
					Name:       st.Name,
					Containers: containerStatuses(st.Spec.Template.Spec.Containers),
					Automated:  policyName != "",
					PolicyName: policyName,
				})
			}
		}
	}
	if kindFilter == "" || kindFilter == "DaemonSet" {
		var list appsv1.DaemonSetList
		if err := s.K8sClient.List(ctx, &list, listOpts...); err != nil {
			s.Logger.Error(err, "failed to list daemonsets")
		} else {
			for _, ds := range list.Items {
				policyName := ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]
				workloads = append(workloads, allWorkloadSummary{
					Namespace:  ds.Namespace,
					Kind:       "DaemonSet",
					Name:       ds.Name,
					Containers: containerStatuses(ds.Spec.Template.Spec.Containers),
					Automated:  policyName != "",
					PolicyName: policyName,
				})
			}
		}
	}
	if kindFilter == "" || kindFilter == "CronJob" {
		var list batchv1.CronJobList
		if err := s.K8sClient.List(ctx, &list, listOpts...); err != nil {
			s.Logger.Error(err, "failed to list cronjobs")
		} else {
			for _, cj := range list.Items {
				policyName := cj.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]
				workloads = append(workloads, allWorkloadSummary{
					Namespace:  cj.Namespace,
					Kind:       "CronJob",
					Name:       cj.Name,
					Containers: containerStatuses(cj.Spec.JobTemplate.Spec.Template.Spec.Containers),
					Automated:  policyName != "",
					PolicyName: policyName,
				})
			}
		}
	}

	if workloads == nil {
		workloads = []allWorkloadSummary{}
	}

	// Decorate workloads with Prometheus-derived risk/drift/HPA signals.
	oomByName, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain:workload_oom_24h", "owner_name")
	driftByName, _ := s.PromClient.QueryByLabel(ctx, "max by (owner_name) (abs(1 - k8s_sustain_workload_drift_ratio))", "owner_name")
	blockedByName, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain_workload_retry_state == 1", "owner_name")
	hpaByName, _ := s.PromClient.QueryByLabel(ctx, "k8s_sustain_hpa_present", "owner_name")

	for i := range workloads {
		wl := &workloads[i]
		wl.HPAPresent = hpaByName[wl.Name] > 0
		if drift, ok := driftByName[wl.Name]; ok {
			wl.DriftPercent = drift * 100
		}
		switch {
		case oomByName[wl.Name] > 0:
			wl.RiskState = "at-risk"
		case blockedByName[wl.Name] > 0:
			wl.RiskState = "blocked"
		case wl.DriftPercent > 10:
			wl.RiskState = "drifted"
		default:
			wl.RiskState = "safe"
		}
	}

	// Collect unique namespaces and kinds before filtering
	nsSet := make(map[string]struct{})
	kindSet := make(map[string]struct{})
	for _, w := range workloads {
		nsSet[w.Namespace] = struct{}{}
		kindSet[w.Kind] = struct{}{}
	}
	namespaces := make([]string, 0, len(nsSet))
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}

	// Apply automated filter
	if automatedFilter == "true" || automatedFilter == "false" {
		wantAutomated := automatedFilter == "true"
		filtered := workloads[:0]
		for _, w := range workloads {
			if w.Automated == wantAutomated {
				filtered = append(filtered, w)
			}
		}
		workloads = filtered
	}

	// Apply search filter
	if search != "" {
		filtered := workloads[:0]
		for _, w := range workloads {
			if strings.Contains(strings.ToLower(w.Name), search) {
				filtered = append(filtered, w)
			}
		}
		workloads = filtered
	}

	// Apply risk filter
	if riskFilter != "" {
		filtered := workloads[:0]
		for _, w := range workloads {
			if w.RiskState == riskFilter {
				filtered = append(filtered, w)
			}
		}
		workloads = filtered
	}

	// Apply HPA filter
	if hpaFilter == "has-hpa" || hpaFilter == "no-hpa" {
		wantHPA := hpaFilter == "has-hpa"
		filtered := workloads[:0]
		for _, w := range workloads {
			if w.HPAPresent == wantHPA {
				filtered = append(filtered, w)
			}
		}
		workloads = filtered
	}

	// Counts
	counts := workloadCounts{Total: len(workloads)}
	for _, w := range workloads {
		if w.Automated {
			counts.Automated++
		} else {
			counts.Manual++
		}
	}

	total := len(workloads)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	writeJSON(w, http.StatusOK, paginatedAllWorkloads{
		Items:      workloads[start:end],
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		Namespaces: namespaces,
		Kinds:      kinds,
		Counts:     counts,
	})
}

// ---- Workload recommendations ----

type recommendationResult struct {
	Automated          bool                                 `json:"automated"`
	PolicyName         string                               `json:"policyName,omitempty"`
	Containers         map[string]simulationContainerResult `json:"containers,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

func (s *Server) handleWorkloadRecommendations(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	ctx := r.Context()

	w.Header().Set("Cache-Control", "public, max-age=60")

	// Look up the workload to get its policy annotation
	policyName, err := s.getWorkloadPolicyAnnotation(ctx, namespace, kind, name)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("workload not found: %v", err))
		return
	}

	if policyName == "" {
		writeJSON(w, http.StatusOK, recommendationResult{Automated: false})
		return
	}

	// Fetch the policy to get its config
	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("policy %q not found: %v", policyName, err))
		return
	}

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

	// Chart time range from query params (defaults to CPU window for backward compat)
	chartTimeRange := r.URL.Query().Get("window")
	if chartTimeRange == "" {
		chartTimeRange = cpuWindow
	}
	step := r.URL.Query().Get("step")
	if step == "" {
		step = "5m"
	}

	req := simulateRequest{
		Namespace: namespace,
		OwnerKind: kind,
		OwnerName: name,
		Window:    chartTimeRange,
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
		s := cpuCfg.Requests.MinAllowed.String()
		req.CPU.MinAllowed = &s
	}
	if cpuCfg.Requests.MaxAllowed != nil {
		s := cpuCfg.Requests.MaxAllowed.String()
		req.CPU.MaxAllowed = &s
	}
	if memCfg.Requests.MinAllowed != nil {
		s := memCfg.Requests.MinAllowed.String()
		req.Memory.MinAllowed = &s
	}
	if memCfg.Requests.MaxAllowed != nil {
		s := memCfg.Requests.MaxAllowed.String()
		req.Memory.MaxAllowed = &s
	}

	result, err := s.runSimulation(ctx, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("computing recommendations: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, recommendationResult{
		Automated:          true,
		PolicyName:         policyName,
		Containers:         result.Containers,
		CPURecommendations: result.CPURecommendations,
		MemRecommendations: result.MemRecommendations,
	})
}

func (s *Server) getWorkloadPolicyAnnotation(ctx context.Context, namespace, kind, name string) (string, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	switch kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return "", err
		}
		return obj.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation], nil
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return "", err
		}
		return obj.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation], nil
	case "DaemonSet":
		obj := &appsv1.DaemonSet{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return "", err
		}
		return obj.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation], nil
	case "CronJob":
		obj := &batchv1.CronJob{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return "", err
		}
		return obj.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation], nil
	default:
		return "", fmt.Errorf("unsupported kind %q", kind)
	}
}

// ---- Workload metrics ----

func (s *Server) handleWorkloadRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// /api/workloads/:namespace/:kind/:name[/metrics|recommendations]
	parts := parsePath(r.URL.Path, "/api/workloads/")
	if len(parts) < 3 {
		writeError(w, http.StatusBadRequest, "expected /api/workloads/:namespace/:kind/:name[/metrics|recommendations]")
		return
	}

	namespace := parts[0]
	kind := parts[1]
	name := parts[2]

	if len(parts) == 3 {
		s.handleWorkloadDetail(w, r, namespace, kind, name)
		return
	}

	if parts[3] == "recommendations" {
		s.handleWorkloadRecommendations(w, r, namespace, kind, name)
		return
	}
	if parts[3] != "metrics" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")

	window := r.URL.Query().Get("window")
	if window == "" {
		window = "168h"
	}
	step := r.URL.Query().Get("step")
	if step == "" {
		step = "5m"
	}

	cpuSeries, err := s.PromClient.QueryCPURangeByContainer(r.Context(), namespace, kind, name, window, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("cpu range query: %v", err))
		return
	}

	memSeries, err := s.PromClient.QueryMemoryRangeByContainer(r.Context(), namespace, kind, name, window, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("memory range query: %v", err))
		return
	}

	// OOM kill events (best-effort, non-fatal)
	oomEvents, _ := s.PromClient.QueryOOMKillEvents(r.Context(), namespace, kind, name, window, step)

	// Fetch current resource requests/limits from the workload spec
	resources := s.getContainerResources(r.Context(), namespace, kind, name)

	// Fetch historical resource request time-series from Prometheus (best-effort)
	cpuRequests, _ := s.PromClient.QueryCPURequestRangeByContainer(r.Context(), namespace, kind, name, window, step)
	memRequests, _ := s.PromClient.QueryMemoryRequestRangeByContainer(r.Context(), namespace, kind, name, window, step)

	writeJSON(w, http.StatusOK, map[string]any{
		"cpu":            cpuSeries,
		"memory":         memSeries,
		"resources":      resources,
		"cpuRequests":    cpuRequests,
		"memoryRequests": memRequests,
		"oomEvents":      oomEvents,
	})
}

// ---- Container resources ----

type containerResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

func (s *Server) getContainerResources(ctx context.Context, namespace, kind, name string) map[string]containerResources {
	containers, err := s.getWorkloadContainers(ctx, namespace, kind, name)
	if err != nil {
		s.Logger.Error(err, "failed to get workload containers", "namespace", namespace, "kind", kind, "name", name)
		return nil
	}
	result := make(map[string]containerResources, len(containers))
	for _, c := range containers {
		cr := containerResources{}
		if req := c.Resources.Requests; req != nil {
			if cpu := req.Cpu(); cpu != nil && !cpu.IsZero() {
				cr.CPURequest = cpu.String()
			}
			if mem := req.Memory(); mem != nil && !mem.IsZero() {
				cr.MemoryRequest = mem.String()
			}
		}
		if lim := c.Resources.Limits; lim != nil {
			if cpu := lim.Cpu(); cpu != nil && !cpu.IsZero() {
				cr.CPULimit = cpu.String()
			}
			if mem := lim.Memory(); mem != nil && !mem.IsZero() {
				cr.MemoryLimit = mem.String()
			}
		}
		result[c.Name] = cr
	}
	return result
}

func (s *Server) getWorkloadContainers(ctx context.Context, namespace, kind, name string) ([]corev1.Container, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	switch kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Template.Spec.Containers, nil
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Template.Spec.Containers, nil
	case "DaemonSet":
		obj := &appsv1.DaemonSet{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Template.Spec.Containers, nil
	case "CronJob":
		obj := &batchv1.CronJob{}
		if err := s.K8sClient.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.JobTemplate.Spec.Template.Spec.Containers, nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
}

// ---- Simulate ----

type simulateRequest struct {
	Namespace string `json:"namespace"`
	OwnerKind string `json:"ownerKind"`
	OwnerName string `json:"ownerName"`
	Window    string `json:"window"`
	Step      string `json:"step"`

	CPU    simulateResourceConfig `json:"cpu"`
	Memory simulateResourceConfig `json:"memory"`
}

type simulateResourceConfig struct {
	Percentile *int32  `json:"percentile,omitempty"`
	Headroom   *int32  `json:"headroom,omitempty"`
	MinAllowed           *string `json:"minAllowed,omitempty"`
	MaxAllowed           *string `json:"maxAllowed,omitempty"`
	Window               string  `json:"window,omitempty"`
}

type simulateContainerResult struct {
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req simulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	// Input validation
	if req.Namespace == "" {
		writeError(w, http.StatusBadRequest, "namespace is required")
		return
	}
	if req.OwnerName == "" {
		writeError(w, http.StatusBadRequest, "ownerName is required")
		return
	}
	validKinds := map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "CronJob": true}
	if !validKinds[req.OwnerKind] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid ownerKind %q: must be one of Deployment, StatefulSet, DaemonSet, CronJob", req.OwnerKind))
		return
	}

	if req.Window == "" {
		req.Window = "168h"
	}
	if req.Step == "" {
		req.Step = "5m"
	}

	result, err := s.runSimulation(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("simulation failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// ---- Workload detail snapshot ----

type workloadDetailResponse struct {
	UpdateMode     string                 `json:"updateMode,omitempty"`
	LastRecycledAt string                 `json:"lastRecycledAt,omitempty"`
	DriftPercent   float64                `json:"driftPercent"`
	OOM24h         int                    `json:"oom24h"`
	HPAMode        string                 `json:"hpaMode,omitempty"`
	Blocked        *workloadDetailBlocked `json:"blocked,omitempty"`
	RecentEvents   []activityItem         `json:"recentEvents"`
}

type workloadDetailBlocked struct {
	Reason      string `json:"reason"`
	Attempts    int    `json:"attempts"`
	NextRetryAt string `json:"nextRetryAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

func (s *Server) handleWorkloadDetail(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	w.Header().Set("Cache-Control", "public, max-age=30")

	resp := workloadDetailResponse{}

	if policyName, _ := s.getWorkloadPolicyAnnotation(ctx, namespace, kind, name); policyName != "" {
		policy := &sustainv1alpha1.Policy{}
		if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err == nil {
			var modePtr *sustainv1alpha1.UpdateMode
			switch kind {
			case "Deployment":
				modePtr = policy.Spec.Update.Types.Deployment
			case "StatefulSet":
				modePtr = policy.Spec.Update.Types.StatefulSet
			case "DaemonSet":
				modePtr = policy.Spec.Update.Types.DaemonSet
			case "CronJob":
				modePtr = policy.Spec.Update.Types.CronJob
			}
			if modePtr != nil {
				resp.UpdateMode = string(*modePtr)
			}
			if policy.Spec.RightSizing.UpdatePolicy.Hpa != nil {
				resp.HPAMode = string(policy.Spec.RightSizing.UpdatePolicy.Hpa.Mode)
			}
		}
	}

	oomExpr := fmt.Sprintf(`k8s_sustain:workload_oom_24h{namespace=%q,owner_kind=%q,owner_name=%q}`, namespace, kind, name)
	if v, _ := s.PromClient.QueryInstant(ctx, oomExpr); v > 0 {
		resp.OOM24h = int(v)
	}
	driftExpr := fmt.Sprintf(`max(abs(1 - k8s_sustain_workload_drift_ratio{namespace=%q,owner_kind=%q,owner_name=%q}))`, namespace, kind, name)
	if v, _ := s.PromClient.QueryInstant(ctx, driftExpr); v > 0 {
		resp.DriftPercent = v * 100
	}
	blockedExpr := fmt.Sprintf(`k8s_sustain_workload_retry_state{namespace=%q,owner_kind=%q,owner_name=%q} == 1`, namespace, kind, name)
	blockedByReason, _ := s.PromClient.QueryByLabel(ctx, blockedExpr, "reason")
	if len(blockedByReason) > 0 {
		var reason string
		for k := range blockedByReason {
			reason = k
			break
		}
		attemptsExpr := fmt.Sprintf(`k8s_sustain_workload_retry_attempts{namespace=%q,owner_kind=%q,owner_name=%q}`, namespace, kind, name)
		attempts, _ := s.PromClient.QueryInstant(ctx, attemptsExpr)
		resp.Blocked = &workloadDetailBlocked{Reason: reason, Attempts: int(attempts)}
	}

	var list corev1.EventList
	_ = s.K8sClient.List(ctx, &list, client.InNamespace(namespace))
	for _, e := range list.Items {
		if e.InvolvedObject.Kind != kind || e.InvolvedObject.Name != name {
			continue
		}
		if e.Source.Component != "k8s-sustain" {
			continue
		}
		resp.RecentEvents = append(resp.RecentEvents, activityItem{
			Timestamp: e.LastTimestamp.Format("2006-01-02T15:04:05Z"),
			Namespace: e.InvolvedObject.Namespace,
			Kind:      e.InvolvedObject.Kind,
			Name:      e.InvolvedObject.Name,
			Reason:    e.Reason,
			Message:   e.Message,
		})
		if len(resp.RecentEvents) >= 10 {
			break
		}
	}
	if resp.RecentEvents == nil {
		resp.RecentEvents = []activityItem{}
	}

	writeJSON(w, http.StatusOK, resp)
}
