package dashboard

import (
	"context"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// inactiveWorkload is a list row reconstructed from a WorkloadRecommendation
// retained after its workload object disappeared (ephemeral bare pods,
// deleted or terminal Jobs — see the controller's retention sweep). It keeps
// ephemeral workloads visible on the dashboard until the retention window
// lapses.
type inactiveWorkload struct {
	Namespace  string
	Kind       string
	Name       string
	PolicyName string
	LastSeenAt string
	Containers []containerStatus
}

// collectInactiveWorkloads lists WorkloadRecommendations (narrowed by opts,
// e.g. a policy-label selector) and returns one row per WLR whose workload
// identity is absent from live — the workloadKey set of rows already built
// from live objects. WLRs with an empty spec.policy (unmatched, pending GC)
// are skipped. The error is returned so callers can degrade to live-only.
func (s *Server) collectInactiveWorkloads(ctx context.Context, live map[string]struct{}, opts ...client.ListOption) ([]inactiveWorkload, error) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	out := []inactiveWorkload{}
	for i := range list.Items {
		wlr := &list.Items[i]
		if wlr.Spec.Policy == "" {
			continue
		}
		ref := wlr.Spec.WorkloadRef
		if _, ok := live[workloadKey(ref.Namespace, ref.Kind, ref.Name)]; ok {
			continue
		}
		out = append(out, inactiveWorkload{
			Namespace:  ref.Namespace,
			Kind:       ref.Kind,
			Name:       ref.Name,
			PolicyName: wlr.Spec.Policy,
			LastSeenAt: wlr.Status.ObservedAt.UTC().Format(time.RFC3339),
			Containers: observedContainerStatuses(wlr.Status.ObservedResources),
		})
	}
	return out, nil
}

// observedContainerStatuses converts the WLR observed-resources snapshot into
// the containerStatus rows list responses use: regular containers first, then
// init containers, alphabetical within each group (map order is random).
func observedContainerStatuses(obs map[string]sustainv1alpha1.ObservedContainerResources) []containerStatus {
	out := make([]containerStatus, 0, len(obs))
	for name, res := range obs {
		cs := containerStatus{Name: name, Init: res.Init}
		if res.CPURequest != nil {
			cs.CPURequest = res.CPURequest.String()
		}
		if res.CPULimit != nil {
			cs.CPULimit = res.CPULimit.String()
		}
		if res.MemoryRequest != nil {
			cs.MemoryRequest = res.MemoryRequest.String()
		}
		if res.MemoryLimit != nil {
			cs.MemoryLimit = res.MemoryLimit.String()
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Init != out[j].Init {
			return !out[i].Init
		}
		return out[i].Name < out[j].Name
	})
	return out
}
