package dashboard

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// inactiveWorkload is a list row reconstructed from a WorkloadRecommendation
// retained after its workload object disappeared (ephemeral bare pods,
// deleted or terminal Jobs — see the controller's retention sweep). It keeps
// ephemeral workloads visible on the dashboard until the retention window
// lapses.
type inactiveWorkload struct {
	workloadRow
	PolicyName string
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
		containers, initContainers := wlrcache.ContainersFromObserved(wlr.Status.ObservedResources)
		out = append(out, inactiveWorkload{
			workloadRow: workloadRow{
				Namespace:  ref.Namespace,
				Kind:       ref.Kind,
				Name:       ref.Name,
				Containers: containerStatuses(containers, initContainers),
				LastSeenAt: wlr.Status.ObservedAt.UTC().Format(time.RFC3339),
			},
			PolicyName: wlr.Spec.Policy,
		})
	}
	return out, nil
}
