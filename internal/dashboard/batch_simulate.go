package dashboard

import (
	"context"
	"fmt"
	"math"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

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

type savingsAggregate struct {
	CurrentMillis        int64   `json:"currentMillis"`
	RecommendedMillis    int64   `json:"recommendedMillis"`
	SavingsMillis        int64   `json:"savingsMillis"`
	SavingsPercent       float64 `json:"savingsPercent"`
	CurrentFormatted     string  `json:"currentFormatted"`
	RecommendedFormatted string  `json:"recommendedFormatted"`
	SavingsFormatted     string  `json:"savingsFormatted"`
}

func (s *Server) handlePolicyBatchSimulate(w http.ResponseWriter, r *http.Request, policyName string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()

	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("policy %q: %v", policyName, err))
		return
	}

	governed, err := s.governedBy(ctx, policy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("listing workloads: %v", err))
		return
	}
	workloads := governed[:0:0]
	for _, id := range governed {
		if !id.Departed() {
			workloads = append(workloads, id)
		}
	}
	results := s.computeRecommendations(ctx, policy, workloads)

	resp := batchSimulateResponse{PolicyName: policyName}
	var totalCPUCurr, totalCPURec, totalMemCurr, totalMemRec int64

	for i, r := range results {
		wl := workloads[i]
		wbr := workloadBatchResult{
			Namespace:  wl.Key.Namespace,
			Kind:       wl.Key.OwnerKind,
			Name:       wl.Key.OwnerName,
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

				// Only fold a recommendation into the aggregate when its
				// current usage was counted too — otherwise the totals compare
				// different container sets and the savings percent is deflated
				// (or flips negative).
				if rec.CPURequest != "" {
					if q, err := resource.ParseQuantity(rec.CPURequest); err == nil {
						cpuRec := q.MilliValue()
						if cpuCurr > 0 {
							totalCPURec += cpuRec
							bcr.CPUDeltaPercent = deltaPercent(cpuCurr, cpuRec)
						}
					}
				}
				if rec.MemoryRequest != "" {
					if q, err := resource.ParseQuantity(rec.MemoryRequest); err == nil {
						memRec := q.MilliValue()
						if memCurr > 0 {
							totalMemRec += memRec
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

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, resp)
}

type recResult struct {
	recs map[string]simulationContainerResult
	err  error
}

// computeRecommendations fetches every identity's inputs in one batch and
// runs the shared algorithm on each under the policy's own configuration,
// exactly as the controller would.
func (s *Server) computeRecommendations(ctx context.Context, policy *sustainv1alpha1.Policy, workloads []*inventory.Identity) []recResult {
	specs := make([]simulationSpec, len(workloads))
	containers := make([][]corev1.Container, len(workloads))
	reqs := make([]recommender.InputsRequest, len(workloads))
	for i, wl := range workloads {
		specs[i] = policySpec(policy, wl.Key.Namespace, wl.Key.OwnerKind, wl.Key.OwnerName)
		containers[i], _ = workload.MergeContainersForRecommendation(wl.Containers, wl.InitContainers, specs[i].excludeInit)
		reqs[i] = recommender.InputsRequest{Identity: specs[i].identity(), Containers: len(containers[i])}
	}
	fetched := s.Inputs.FetchInputs(ctx, policy.Spec.RightSizing.ResourcesConfigs, reqs)

	// One snapshot per request: it lists each namespace's autoscalers once.
	autoSnap := autoscaler.NewNamespacedSnapshot(s.K8sClient)
	out := make([]recResult, len(workloads))
	for i, spec := range specs {
		f := fetched[spec.identity()]
		if f.Err != nil {
			out[i].err = f.Err
			continue
		}
		res := computeWorkloadRecs(spec, containers[i], workloads[i].Since, s.autoscalerInfo(ctx, autoSnap, spec), f.Inputs)
		out[i].recs = simulationContainers(res, f.Inputs)
	}
	return out
}

func deltaPercent(current, recommended int64) float64 {
	if current == 0 {
		return 0
	}
	return math.Round((float64(recommended-current)/float64(current)*100)*10) / 10
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
		SavingsFormatted:     formatQuantity(max(savings, -savings), resourceType),
	}
}
