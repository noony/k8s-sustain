package dashboard

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/recommender"
)

// ---- Summary types ----

// summaryKPI is the top-row of cluster-wide KPIs surfaced by /api/summary.
type summaryKPI struct {
	CPUSavedCores float64   `json:"cpuSavedCores"`
	CPUSavedRatio float64   `json:"cpuSavedRatio"`
	CPUSpark7d    []float64 `json:"cpuSpark7d"`
	MemSavedBytes float64   `json:"memSavedBytes"`
	MemSavedRatio float64   `json:"memSavedRatio"`
	MemSpark7d    []float64 `json:"memSpark7d"`
	AtRiskCount   int       `json:"atRiskCount"`
	DriftedCount  int       `json:"driftedCount"`
}

type headroomBreakdown struct {
	Used float64 `json:"used"`
	Idle float64 `json:"idle"`
	Free float64 `json:"free"`
}

type attentionRow struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Policy    string `json:"policy,omitempty"`
	Signal    string `json:"signal"`
	Detail    string `json:"detail,omitempty"`
	LastSeen  string `json:"lastSeen,omitempty"`
}

type policyRollup struct {
	Name            string  `json:"name"`
	WorkloadCount   int     `json:"workloadCount"`
	CPUSavingsCores float64 `json:"cpuSavingsCores"`
	MemSavingsBytes float64 `json:"memSavingsBytes"`
	AtRiskCount     int     `json:"atRiskCount"`
	LastAppliedAt   string  `json:"lastAppliedAt,omitempty"`
}

type summaryResponseV2 struct {
	KPI       summaryKPI                   `json:"kpi"`
	Headroom  map[string]headroomBreakdown `json:"headroom"`
	Attention map[string][]attentionRow    `json:"attention"`
	Policies  []policyRollup               `json:"policies"`
}

type savingsAggregate struct {
	CurrentMillis        int64   `json:"currentMillis"`
	RecommendedMillis    int64   `json:"recommendedMillis"`
	SavingsMillis        int64   `json:"savingsMillis"`
	SavingsPercent       float64 `json:"savingsPercent"`
	CurrentFormatted     string  `json:"currentFormatted"`
	RecommendedFormatted string  `json:"recommendedFormatted"`
	SavingsFormatted     string  `json:"savingsFormatted"`
}

// ---- Batch simulate types ----

type batchSimulateResponse struct {
	PolicyName string                `json:"policyName"`
	CPU        savingsAggregate      `json:"cpu"`
	Memory     savingsAggregate      `json:"memory"`
	Workloads  []workloadBatchResult `json:"workloads"`
}

type workloadBatchResult struct {
	Namespace  string                          `json:"namespace"`
	Kind       string                          `json:"kind"`
	Name       string                          `json:"name"`
	Containers map[string]batchContainerResult `json:"containers"`
	Error      string                          `json:"error,omitempty"`
}

type batchContainerResult struct {
	CurrentCPU        string  `json:"currentCpu"`
	RecommendedCPU    string  `json:"recommendedCpu"`
	CPUDeltaPercent   float64 `json:"cpuDeltaPercent"`
	CurrentMemory     string  `json:"currentMemory"`
	RecommendedMemory string  `json:"recommendedMemory"`
	MemDeltaPercent   float64 `json:"memDeltaPercent"`
}

// ---- Internal types ----

type automatedWorkload struct {
	Namespace  string
	Kind       string
	Name       string
	PolicyName string
	Containers []corev1.Container
}

// ---- Summary handler ----

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	if cached, ok := s.summaryCache.Get("summary"); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	ctx := r.Context()
	resp := summaryResponseV2{
		Headroom:  map[string]headroomBreakdown{},
		Attention: map[string][]attentionRow{"risk": {}, "drift": {}, "blocked": {}},
		Policies:  []policyRollup{},
	}

	var promErrs int
	recordErr := func(err error) {
		if err != nil {
			promErrs++
		}
	}

	var err error
	resp.KPI.CPUSavedCores, err = s.PromClient.QueryInstant(ctx, "k8s_sustain:cluster_cpu_savings_cores")
	recordErr(err)
	resp.KPI.CPUSavedRatio, err = s.PromClient.QueryInstant(ctx, "k8s_sustain:cluster_cpu_savings_ratio")
	recordErr(err)
	resp.KPI.MemSavedBytes, err = s.PromClient.QueryInstant(ctx, "k8s_sustain:cluster_memory_savings_bytes")
	recordErr(err)
	resp.KPI.MemSavedRatio, err = s.PromClient.QueryInstant(ctx, "k8s_sustain:cluster_memory_savings_ratio")
	recordErr(err)

	var sparkErr error
	resp.KPI.CPUSpark7d, sparkErr = sparklinePoints(ctx, s.PromClient, "k8s_sustain:cluster_cpu_savings_cores", "168h", "30m")
	recordErr(sparkErr)
	resp.KPI.MemSpark7d, sparkErr = sparklinePoints(ctx, s.PromClient, "k8s_sustain:cluster_memory_savings_bytes", "168h", "30m")
	recordErr(sparkErr)

	atRiskByPolicy, err := s.PromClient.QueryByLabel(ctx, "k8s_sustain_policy_at_risk_count", "policy")
	recordErr(err)
	for _, n := range atRiskByPolicy {
		resp.KPI.AtRiskCount += int(n)
	}
	drifted, err := s.PromClient.QueryInstant(ctx, "count(k8s_sustain:workload_drifted == 1)")
	recordErr(err)
	resp.KPI.DriftedCount = int(drifted)

	var hrErr error
	resp.Headroom["cpu"], hrErr = readHeadroom(ctx, s.PromClient, "k8s_sustain:cluster_cpu_headroom_breakdown")
	recordErr(hrErr)
	resp.Headroom["memory"], hrErr = readHeadroom(ctx, s.PromClient, "k8s_sustain:cluster_memory_headroom_breakdown")
	recordErr(hrErr)

	var attErr error
	resp.Attention["risk"], attErr = collectAttention(ctx, s.PromClient, "k8s_sustain:workload_oom_24h > 0", "OOM")
	recordErr(attErr)
	resp.Attention["drift"], attErr = collectAttention(ctx, s.PromClient, "k8s_sustain:workload_drifted == 1", "drift")
	recordErr(attErr)
	resp.Attention["blocked"], attErr = collectAttention(ctx, s.PromClient, "k8s_sustain_workload_retry_state == 1", "blocked")
	recordErr(attErr)

	wlByPolicy, err := s.PromClient.QueryByLabel(ctx, "k8s_sustain_policy_workload_count", "policy")
	recordErr(err)
	cpuByPolicy, err := s.PromClient.QueryByLabel(ctx, "k8s_sustain:policy_cpu_savings_cores", "policy")
	recordErr(err)
	memByPolicy, err := s.PromClient.QueryByLabel(ctx, "k8s_sustain:policy_memory_savings_bytes", "policy")
	recordErr(err)

	// Iterate union of policy keys so partial-data rollups still surface.
	policyNames := make(map[string]struct{}, len(wlByPolicy)+len(cpuByPolicy)+len(memByPolicy))
	for n := range wlByPolicy {
		policyNames[n] = struct{}{}
	}
	for n := range cpuByPolicy {
		policyNames[n] = struct{}{}
	}
	for n := range memByPolicy {
		policyNames[n] = struct{}{}
	}
	sortedPolicyNames := make([]string, 0, len(policyNames))
	for n := range policyNames {
		sortedPolicyNames = append(sortedPolicyNames, n)
	}
	sort.Strings(sortedPolicyNames)
	for _, name := range sortedPolicyNames {
		resp.Policies = append(resp.Policies, policyRollup{
			Name:            name,
			WorkloadCount:   int(wlByPolicy[name]),
			CPUSavingsCores: cpuByPolicy[name],
			MemSavingsBytes: memByPolicy[name],
			AtRiskCount:     int(atRiskByPolicy[name]),
		})
	}

	if promErrs == 0 {
		s.summaryCache.Set("summary", resp)
	} else {
		s.Logger.V(1).Info("summary: prometheus errors", "count", promErrs)
	}
	writeJSON(w, http.StatusOK, resp)
}

func sparklinePoints(ctx context.Context, p PromQuerier, expr, window, step string) ([]float64, error) {
	pts, err := p.QueryRange(ctx, expr, window, step)
	if err != nil {
		return []float64{}, err
	}
	if len(pts) == 0 {
		return []float64{}, nil
	}
	out := make([]float64, 0, len(pts))
	for _, v := range pts {
		out = append(out, v.Value)
	}
	return out, nil
}

func readHeadroom(ctx context.Context, p PromQuerier, expr string) (headroomBreakdown, error) {
	bySeg, err := p.QueryByLabel(ctx, expr, "segment")
	if err != nil {
		return headroomBreakdown{}, err
	}
	return headroomBreakdown{Used: bySeg["used"], Idle: bySeg["idle"], Free: bySeg["free"]}, nil
}

func collectAttention(ctx context.Context, p PromQuerier, expr, signal string) ([]attentionRow, error) {
	rows := []attentionRow{}
	bySeries, err := p.QueryByLabel(ctx, expr, "owner_name")
	if err != nil {
		return rows, err
	}
	if len(bySeries) == 0 {
		return rows, nil
	}
	// Sort deterministically: descending by value, ties broken alphabetically.
	names := make([]string, 0, len(bySeries))
	for n := range bySeries {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if bySeries[names[i]] != bySeries[names[j]] {
			return bySeries[names[i]] > bySeries[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > 10 {
		names = names[:10]
	}
	for _, name := range names {
		rows = append(rows, attentionRow{Name: name, Signal: signal})
	}
	return rows, nil
}

// ---- Batch simulate handler ----

func (s *Server) handlePolicyBatchSimulate(w http.ResponseWriter, r *http.Request, policyName string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	ctx := r.Context()

	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("policy %q not found: %v", policyName, err))
		return
	}

	workloads := s.collectPolicyWorkloads(ctx, policyName, policy)

	type recResult struct {
		recs map[string]simulationContainerResult
		err  error
	}
	results := make([]recResult, len(workloads))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i, wl := range workloads {
		wg.Add(1)
		go func(idx int, wl automatedWorkload) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			recs, err := s.computeRecommendations(ctx, wl.Namespace, wl.Kind, wl.Name, policy)
			results[idx] = recResult{recs: recs, err: err}
		}(i, wl)
	}
	wg.Wait()

	resp := batchSimulateResponse{PolicyName: policyName}
	var totalCPUCurr, totalCPURec, totalMemCurr, totalMemRec int64

	for i, r := range results {
		wl := workloads[i]
		wbr := workloadBatchResult{
			Namespace:  wl.Namespace,
			Kind:       wl.Kind,
			Name:       wl.Name,
			Containers: make(map[string]batchContainerResult),
		}

		if r.err != nil {
			wbr.Error = r.err.Error()
			resp.Workloads = append(resp.Workloads, wbr)
			continue
		}

		for _, c := range wl.Containers {
			bcr := batchContainerResult{}
			var cpuCurr, memCurr int64

			if rec, ok := r.recs[c.Name]; ok {
				// Use actual Prometheus usage for "current" instead of k8s resource requests
				if rec.CPUUsageCores > 0 {
					cpuCurr = int64(rec.CPUUsageCores * 1000)
					cpuQty := resource.NewMilliQuantity(cpuCurr, resource.DecimalSI)
					bcr.CurrentCPU = cpuQty.String()
				}
				if rec.MemoryUsageBytes > 0 {
					memCurr = int64(rec.MemoryUsageBytes) * 1000
					memQty := resource.NewQuantity(int64(rec.MemoryUsageBytes), resource.BinarySI)
					bcr.CurrentMemory = memQty.String()
				}

				bcr.RecommendedCPU = rec.CPURequest
				bcr.RecommendedMemory = rec.MemoryRequest

				if rec.CPURequest != "" {
					if q, err := resource.ParseQuantity(rec.CPURequest); err == nil {
						cpuRec := q.MilliValue()
						totalCPURec += cpuRec
						if cpuCurr > 0 {
							bcr.CPUDeltaPercent = deltaPercent(cpuCurr, cpuRec)
						}
					}
				}
				if rec.MemoryRequest != "" {
					if q, err := resource.ParseQuantity(rec.MemoryRequest); err == nil {
						memRec := q.MilliValue()
						totalMemRec += memRec
						if memCurr > 0 {
							bcr.MemDeltaPercent = deltaPercent(memCurr, memRec)
						}
					}
				}
			}

			totalCPUCurr += cpuCurr
			totalMemCurr += memCurr
			wbr.Containers[c.Name] = bcr
		}

		resp.Workloads = append(resp.Workloads, wbr)
	}

	resp.CPU = buildSavingsAggregate(totalCPUCurr, totalCPURec, "cpu")
	resp.Memory = buildSavingsAggregate(totalMemCurr, totalMemRec, "memory")

	if resp.Workloads == nil {
		resp.Workloads = []workloadBatchResult{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---- Shared recommendation computation ----

func (s *Server) computeRecommendations(ctx context.Context, namespace, kind, name string, policy *sustainv1alpha1.Policy) (map[string]simulationContainerResult, error) {
	cpuCfg := policy.Spec.RightSizing.ResourcesConfigs.CPU
	memCfg := policy.Spec.RightSizing.ResourcesConfigs.Memory

	cpuWindow := recommender.ResourceWindow(cpuCfg.Window)
	memWindow := recommender.ResourceWindow(memCfg.Window)
	cpuQuantile := recommender.PercentileQuantile(cpuCfg.Requests.Percentile)
	memQuantile := recommender.PercentileQuantile(memCfg.Requests.Percentile)

	cpuValues, err := s.PromClient.QueryCPUByContainer(ctx, namespace, kind, name, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}
	memValues, err := s.PromClient.QueryMemoryByContainer(ctx, namespace, kind, name, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
	}

	allContainers := make(map[string]struct{})
	for n := range cpuValues {
		allContainers[n] = struct{}{}
	}
	for n := range memValues {
		allContainers[n] = struct{}{}
	}

	containers := make(map[string]simulationContainerResult, len(allContainers))
	for n := range allContainers {
		cr := simulationContainerResult{}
		if cores, ok := cpuValues[n]; ok {
			cr.CPUUsageCores = cores
			if qty := recommender.ComputeCPURequest(cores, cpuCfg.Requests); qty != nil {
				cr.CPURequest = qty.String()
			}
		}
		if bytes, ok := memValues[n]; ok {
			cr.MemoryUsageBytes = bytes
			if qty := recommender.ComputeMemoryRequest(bytes, memCfg.Requests); qty != nil {
				cr.MemoryRequest = qty.String()
			}
		}
		containers[n] = cr
	}

	return containers, nil
}

// ---- Workload collection helpers ----

func (s *Server) collectPolicyWorkloads(ctx context.Context, policyName string, policy *sustainv1alpha1.Policy) []automatedWorkload {
	var workloads []automatedWorkload

	if policy.Spec.RightSizing.Update.Types.Deployment != nil {
		var list appsv1.DeploymentList
		if err := s.K8sClient.List(ctx, &list); err == nil {
			for _, d := range list.Items {
				if d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] == policyName {
					workloads = append(workloads, automatedWorkload{
						Namespace: d.Namespace, Kind: "Deployment", Name: d.Name,
						PolicyName: policyName, Containers: d.Spec.Template.Spec.Containers,
					})
				}
			}
		}
	}

	if policy.Spec.RightSizing.Update.Types.StatefulSet != nil {
		var list appsv1.StatefulSetList
		if err := s.K8sClient.List(ctx, &list); err == nil {
			for _, st := range list.Items {
				if st.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] == policyName {
					workloads = append(workloads, automatedWorkload{
						Namespace: st.Namespace, Kind: "StatefulSet", Name: st.Name,
						PolicyName: policyName, Containers: st.Spec.Template.Spec.Containers,
					})
				}
			}
		}
	}

	if policy.Spec.RightSizing.Update.Types.DaemonSet != nil {
		var list appsv1.DaemonSetList
		if err := s.K8sClient.List(ctx, &list); err == nil {
			for _, ds := range list.Items {
				if ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] == policyName {
					workloads = append(workloads, automatedWorkload{
						Namespace: ds.Namespace, Kind: "DaemonSet", Name: ds.Name,
						PolicyName: policyName, Containers: ds.Spec.Template.Spec.Containers,
					})
				}
			}
		}
	}

	if policy.Spec.RightSizing.Update.Types.CronJob != nil {
		var list batchv1.CronJobList
		if err := s.K8sClient.List(ctx, &list); err == nil {
			for _, cj := range list.Items {
				if cj.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] == policyName {
					workloads = append(workloads, automatedWorkload{
						Namespace: cj.Namespace, Kind: "CronJob", Name: cj.Name,
						PolicyName: policyName, Containers: cj.Spec.JobTemplate.Spec.Template.Spec.Containers,
					})
				}
			}
		}
	}

	return workloads
}

// ---- Utility functions ----

func deltaPercent(current, recommended int64) float64 {
	if current == 0 {
		return 0
	}
	return math.Round((float64(recommended-current)/float64(current)*100)*10) / 10
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func buildSavingsAggregate(current, recommended int64, resourceType string) savingsAggregate {
	savings := current - recommended
	var pct float64
	if current > 0 {
		pct = math.Round((float64(savings)/float64(current)*100)*10) / 10
	}
	return savingsAggregate{
		CurrentMillis:        current,
		RecommendedMillis:    recommended,
		SavingsMillis:        savings,
		SavingsPercent:       pct,
		CurrentFormatted:     formatQuantity(current, resourceType),
		RecommendedFormatted: formatQuantity(recommended, resourceType),
		SavingsFormatted:     formatQuantity(abs64(savings), resourceType),
	}
}
