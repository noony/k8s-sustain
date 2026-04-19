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

type summaryResponse struct {
	TotalWorkloads int              `json:"totalWorkloads"`
	Automated      int              `json:"automated"`
	Manual         int              `json:"manual"`
	CPU            savingsAggregate `json:"cpu"`
	Memory         savingsAggregate `json:"memory"`
	Workloads      []workloadSaving `json:"workloads"`
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

type workloadSaving struct {
	Namespace            string            `json:"namespace"`
	Kind                 string            `json:"kind"`
	Name                 string            `json:"name"`
	PolicyName           string            `json:"policyName"`
	Containers           []containerSaving `json:"containers"`
	CPUCurrentMillis     int64             `json:"cpuCurrentMillis"`
	CPURecommendedMillis int64             `json:"cpuRecommendedMillis"`
	CPUDeltaPercent      float64           `json:"cpuDeltaPercent"`
	MemCurrentMillis     int64             `json:"memCurrentMillis"`
	MemRecommendedMillis int64             `json:"memRecommendedMillis"`
	MemDeltaPercent      float64           `json:"memDeltaPercent"`
}

type containerSaving struct {
	Name              string  `json:"name"`
	CurrentCPU        string  `json:"currentCpu"`
	RecommendedCPU    string  `json:"recommendedCpu"`
	CPUDeltaPercent   float64 `json:"cpuDeltaPercent"`
	CurrentMemory     string  `json:"currentMemory"`
	RecommendedMemory string  `json:"recommendedMemory"`
	MemDeltaPercent   float64 `json:"memDeltaPercent"`
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
	ctx := r.Context()

	var policyList sustainv1alpha1.PolicyList
	if err := s.K8sClient.List(ctx, &policyList); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing policies: %s", err))
		return
	}
	policyMap := make(map[string]*sustainv1alpha1.Policy, len(policyList.Items))
	for i := range policyList.Items {
		policyMap[policyList.Items[i].Name] = &policyList.Items[i]
	}

	workloads, totalCount := s.collectAutomatedWorkloads(ctx)

	type recResult struct {
		recs map[string]simulationContainerResult
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

			policy, ok := policyMap[wl.PolicyName]
			if !ok {
				return
			}
			recs, err := s.computeRecommendations(ctx, wl.Namespace, wl.Kind, wl.Name, policy)
			if err != nil {
				s.Logger.V(1).Info("summary: recommendation failed", "workload", wl.Name, "error", err)
				return
			}
			results[idx] = recResult{recs: recs}
		}(i, wl)
	}
	wg.Wait()

	resp := summaryResponse{
		TotalWorkloads: totalCount,
		Automated:      len(workloads),
		Manual:         totalCount - len(workloads),
	}

	var totalCPUCurr, totalCPURec, totalMemCurr, totalMemRec int64

	for i, r := range results {
		if r.recs == nil {
			continue
		}
		ws := buildWorkloadSaving(workloads[i], r.recs)
		totalCPUCurr += ws.CPUCurrentMillis
		totalCPURec += ws.CPURecommendedMillis
		totalMemCurr += ws.MemCurrentMillis
		totalMemRec += ws.MemRecommendedMillis
		resp.Workloads = append(resp.Workloads, ws)
	}

	sort.Slice(resp.Workloads, func(i, j int) bool {
		di := abs64(resp.Workloads[i].CPUCurrentMillis - resp.Workloads[i].CPURecommendedMillis)
		dj := abs64(resp.Workloads[j].CPUCurrentMillis - resp.Workloads[j].CPURecommendedMillis)
		return di > dj
	})

	resp.CPU = buildSavingsAggregate(totalCPUCurr, totalCPURec, "cpu")
	resp.Memory = buildSavingsAggregate(totalMemCurr, totalMemRec, "memory")

	if resp.Workloads == nil {
		resp.Workloads = []workloadSaving{}
	}

	writeJSON(w, http.StatusOK, resp)
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

			if req := c.Resources.Requests; req != nil {
				if cpu := req.Cpu(); cpu != nil && !cpu.IsZero() {
					cpuCurr = cpu.MilliValue()
					bcr.CurrentCPU = cpu.String()
				}
				if mem := req.Memory(); mem != nil && !mem.IsZero() {
					memCurr = mem.MilliValue()
					bcr.CurrentMemory = mem.String()
				}
			}

			if rec, ok := r.recs[c.Name]; ok {
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
			if qty := recommender.ComputeCPURequest(cores, cpuCfg.Requests); qty != nil {
				cr.CPURequest = qty.String()
			}
		}
		if bytes, ok := memValues[n]; ok {
			if qty := recommender.ComputeMemoryRequest(bytes, memCfg.Requests); qty != nil {
				cr.MemoryRequest = qty.String()
			}
		}
		containers[n] = cr
	}

	return containers, nil
}

// ---- Workload collection helpers ----

func (s *Server) collectAutomatedWorkloads(ctx context.Context) ([]automatedWorkload, int) {
	var workloads []automatedWorkload
	total := 0

	var depList appsv1.DeploymentList
	if err := s.K8sClient.List(ctx, &depList); err == nil {
		for _, d := range depList.Items {
			total++
			if pn := d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]; pn != "" {
				workloads = append(workloads, automatedWorkload{
					Namespace: d.Namespace, Kind: "Deployment", Name: d.Name,
					PolicyName: pn, Containers: d.Spec.Template.Spec.Containers,
				})
			}
		}
	}

	var stsList appsv1.StatefulSetList
	if err := s.K8sClient.List(ctx, &stsList); err == nil {
		for _, st := range stsList.Items {
			total++
			if pn := st.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]; pn != "" {
				workloads = append(workloads, automatedWorkload{
					Namespace: st.Namespace, Kind: "StatefulSet", Name: st.Name,
					PolicyName: pn, Containers: st.Spec.Template.Spec.Containers,
				})
			}
		}
	}

	var dsList appsv1.DaemonSetList
	if err := s.K8sClient.List(ctx, &dsList); err == nil {
		for _, ds := range dsList.Items {
			total++
			if pn := ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]; pn != "" {
				workloads = append(workloads, automatedWorkload{
					Namespace: ds.Namespace, Kind: "DaemonSet", Name: ds.Name,
					PolicyName: pn, Containers: ds.Spec.Template.Spec.Containers,
				})
			}
		}
	}

	var cjList batchv1.CronJobList
	if err := s.K8sClient.List(ctx, &cjList); err == nil {
		for _, cj := range cjList.Items {
			total++
			if pn := cj.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation]; pn != "" {
				workloads = append(workloads, automatedWorkload{
					Namespace: cj.Namespace, Kind: "CronJob", Name: cj.Name,
					PolicyName: pn, Containers: cj.Spec.JobTemplate.Spec.Template.Spec.Containers,
				})
			}
		}
	}

	return workloads, total
}

func (s *Server) collectPolicyWorkloads(ctx context.Context, policyName string, policy *sustainv1alpha1.Policy) []automatedWorkload {
	var workloads []automatedWorkload

	if policy.Spec.Update.Types.Deployment != nil {
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

	if policy.Spec.Update.Types.StatefulSet != nil {
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

	if policy.Spec.Update.Types.DaemonSet != nil {
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

	if policy.Spec.Update.Types.CronJob != nil {
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

func buildWorkloadSaving(wl automatedWorkload, recs map[string]simulationContainerResult) workloadSaving {
	ws := workloadSaving{
		Namespace:  wl.Namespace,
		Kind:       wl.Kind,
		Name:       wl.Name,
		PolicyName: wl.PolicyName,
	}

	for _, c := range wl.Containers {
		cs := containerSaving{Name: c.Name}
		var cpuCurr, memCurr int64

		if req := c.Resources.Requests; req != nil {
			if cpu := req.Cpu(); cpu != nil && !cpu.IsZero() {
				cpuCurr = cpu.MilliValue()
				cs.CurrentCPU = cpu.String()
			}
			if mem := req.Memory(); mem != nil && !mem.IsZero() {
				memCurr = mem.MilliValue()
				cs.CurrentMemory = mem.String()
			}
		}

		if rec, ok := recs[c.Name]; ok {
			cs.RecommendedCPU = rec.CPURequest
			cs.RecommendedMemory = rec.MemoryRequest

			if rec.CPURequest != "" {
				if q, err := resource.ParseQuantity(rec.CPURequest); err == nil {
					cpuRec := q.MilliValue()
					ws.CPURecommendedMillis += cpuRec
					if cpuCurr > 0 {
						cs.CPUDeltaPercent = deltaPercent(cpuCurr, cpuRec)
					}
				}
			}
			if rec.MemoryRequest != "" {
				if q, err := resource.ParseQuantity(rec.MemoryRequest); err == nil {
					memRec := q.MilliValue()
					ws.MemRecommendedMillis += memRec
					if memCurr > 0 {
						cs.MemDeltaPercent = deltaPercent(memCurr, memRec)
					}
				}
			}
		}

		ws.CPUCurrentMillis += cpuCurr
		ws.MemCurrentMillis += memCurr
		ws.Containers = append(ws.Containers, cs)
	}

	if ws.CPUCurrentMillis > 0 && ws.CPURecommendedMillis > 0 {
		ws.CPUDeltaPercent = deltaPercent(ws.CPUCurrentMillis, ws.CPURecommendedMillis)
	}
	if ws.MemCurrentMillis > 0 && ws.MemRecommendedMillis > 0 {
		ws.MemDeltaPercent = deltaPercent(ws.MemCurrentMillis, ws.MemRecommendedMillis)
	}

	return ws
}

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
