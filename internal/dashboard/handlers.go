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
)

// ---- Policy types ----

type policyListItem struct {
	Name       string                      `json:"name"`
	Namespaces []string                    `json:"namespaces"`
	Update     sustainv1alpha1.UpdateTypes `json:"update"`
	Conditions []conditionSummary          `json:"conditions"`
	CreatedAt  string                      `json:"createdAt"`
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

	var list sustainv1alpha1.PolicyList
	if err := s.K8sClient.List(r.Context(), &list); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing policies: %s", err))
		return
	}

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
			Name:       p.Name,
			Namespaces: p.Spec.Selector.Namespaces,
			Update:     p.Spec.Update.Types,
			Conditions: conditions,
			CreatedAt:  p.CreationTimestamp.Format("2006-01-02T15:04:05Z"),
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
	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(r.Context(), client.ObjectKey{Name: name}, policy); err != nil {
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

	writeJSON(w, http.StatusOK, policyDetail{
		policyListItem: policyListItem{
			Name:       policy.Name,
			Namespaces: policy.Spec.Selector.Namespaces,
			Update:     policy.Spec.Update.Types,
			Conditions: conditions,
			CreatedAt:  policy.CreationTimestamp.Format("2006-01-02T15:04:05Z"),
		},
		Spec: policy.Spec,
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
	Namespace  string            `json:"namespace"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Containers []containerStatus `json:"containers"`
	Automated  bool              `json:"automated"`
	PolicyName string            `json:"policyName,omitempty"`
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
	Automated  bool                                 `json:"automated"`
	PolicyName string                               `json:"policyName,omitempty"`
	Containers map[string]simulationContainerResult `json:"containers,omitempty"`
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

	window := cpuCfg.Window
	if window == "" {
		window = "168h"
	}

	req := simulateRequest{
		Namespace: namespace,
		OwnerKind: kind,
		OwnerName: name,
		Window:    window,
		Step:      "5m",
		CPU: simulateResourceConfig{
			PercentilePercentage: cpuCfg.Requests.PercentilePercentage,
			HeadroomPercentage:   cpuCfg.Requests.HeadroomPercentage,
		},
		Memory: simulateResourceConfig{
			PercentilePercentage: memCfg.Requests.PercentilePercentage,
			HeadroomPercentage:   memCfg.Requests.HeadroomPercentage,
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
		Automated:  true,
		PolicyName: policyName,
		Containers: result.Containers,
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

	// /api/workloads/:namespace/:kind/:name/metrics|recommendations
	parts := parsePath(r.URL.Path, "/api/workloads/")
	if len(parts) < 4 {
		writeError(w, http.StatusBadRequest, "expected /api/workloads/:namespace/:kind/:name/metrics|recommendations")
		return
	}

	namespace := parts[0]
	kind := parts[1]
	name := parts[2]

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

	writeJSON(w, http.StatusOK, map[string]any{
		"cpu":       cpuSeries,
		"memory":    memSeries,
		"resources": resources,
		"oomEvents": oomEvents,
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
	PercentilePercentage *int32  `json:"percentilePercentage,omitempty"`
	HeadroomPercentage   *int32  `json:"headroomPercentage,omitempty"`
	MinAllowed           *string `json:"minAllowed,omitempty"`
	MaxAllowed           *string `json:"maxAllowed,omitempty"`
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
