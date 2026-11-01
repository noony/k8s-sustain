package dashboard

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	"github.com/noony/k8s-sustain/internal/recommender"
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
	// Outcome is what the identity's computation decided; only a Computed
	// identity counts toward the savings.
	Outcome sustainv1alpha1.RecommendationOutcome `json:"outcome,omitempty"`
	Error   string                                `json:"error,omitempty"`
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
	var workloads []*inventory.Identity
	for _, id := range governed {
		if !id.Departed() {
			workloads = append(workloads, id)
		}
	}
	results := s.computeRecommendations(ctx, policy, workloads)

	resp := batchSimulateResponse{PolicyName: policyName}
	var cpu, mem savings

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

		wbr.Outcome = r.res.Outcome.Recorded()
		cpuTally, memTally := &cpu, &mem
		if r.res.Outcome != recommender.Recommended {
			// A Too young identity's numbers are shown but would not be
			// applied, so they save nothing.
			cpuTally, memTally = &savings{}, &savings{}
		}
		for j, c := range slices.Concat(wl.Containers, wl.InitContainers) {
			rec, ok := r.res.Recs[c.Name]
			if !ok && j >= len(wl.Containers) {
				continue
			}
			var bcr batchContainerResult
			bcr.CurrentCPU, bcr.RecommendedCPU, bcr.CPUDeltaPercent = cpuTally.add(c.Resources.Requests.Cpu(), rec.CPURequest)
			bcr.CurrentMemory, bcr.RecommendedMemory, bcr.MemDeltaPercent = memTally.add(c.Resources.Requests.Memory(), rec.MemoryRequest)
			wbr.Containers[c.Name] = bcr
		}

		resp.Workloads = append(resp.Workloads, wbr)
	}

	resp.CPU = buildSavingsAggregate(cpu.current, cpu.recommended, "cpu")
	resp.Memory = buildSavingsAggregate(mem.current, mem.recommended, "memory")

	if resp.Workloads == nil {
		resp.Workloads = []workloadBatchResult{}
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, resp)
}

// savings totals one resource's requests, in millivalues, as containers run
// with them and as recommended.
type savings struct {
	current, recommended int64
}

// add renders a container's current and recommended request and the change
// between them. Only a container with both counts toward the totals:
// otherwise they compare different container sets, and the savings percent
// is deflated or flips negative.
func (s *savings) add(current, recommended *resource.Quantity) (cur, rec string, delta float64) {
	if !current.IsZero() {
		cur = current.String()
	}
	if recommended != nil {
		rec = recommended.String()
	}
	if cur == "" || rec == "" {
		return cur, rec, 0
	}
	s.current += current.MilliValue()
	s.recommended += recommended.MilliValue()
	return cur, rec, deltaPercent(current.MilliValue(), recommended.MilliValue())
}

type recResult struct {
	res recommender.Result
	err error
}

// computeRecommendations assembles every identity under the policy's own
// right-sizing spec and computes each, exactly as the controller would.
func (s *Server) computeRecommendations(ctx context.Context, policy *sustainv1alpha1.Policy, workloads []*inventory.Identity) []recResult {
	// One snapshot per request: it lists each namespace's autoscalers once.
	assembled := recommender.Assemble(ctx, s.Inputs, autoscaler.NewNamespacedSnapshot(s.K8sClient), s.Clock.Now, policy.Spec.RightSizing, workloads)
	out := make([]recResult, len(workloads))
	for i, a := range assembled {
		if a.Err != nil {
			out[i].err = a.Err
			continue
		}
		out[i].res = recommender.Compute(a.Request)
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
