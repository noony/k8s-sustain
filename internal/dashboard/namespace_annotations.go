package dashboard

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// namespaceAnnotations returns every Namespace's annotations, keyed by name,
// supplying the least-specific policy opt-in level (see
// policymatch.ResolvePolicy).
//
// One List for the whole cluster rather than a Get per workload — but that
// only holds if this is called ONCE per request and the result threaded
// through, not fetched inside listWorkloadsOfKind itself. A request serving
// the workload list calls listWorkloadsOfKind once per kind — up to seven
// times — over supportedWorkloadKinds; a caller doing that loop
// (listPolicyWorkloadRows, collectAllWorkloads, collectPolicyWorkloads) must
// call this once before the loop and pass the same map into every
// listWorkloadsOfKind call, or it becomes one List per kind instead of one
// per request. getWorkloadEntry is the exception: it addresses a single kind
// in a single namespace, not a loop, so it calls this itself and stays at one
// List. Unlike the controller and the webhook, the dashboard's client is
// uncached (k8s.New), so every avoidable List here is a real round-trip.
//
// A List failure is returned rather than swallowed; callers decide, and today
// every caller degrades to "no namespace-level opt-in" rather than failing the
// request, which is the same read-only-view degradation the dashboard already
// applies to a failed Prometheus query.
func (s *Server) namespaceAnnotations(ctx context.Context) (map[string]map[string]string, error) {
	var list corev1.NamespaceList
	if err := s.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(list.Items))
	for i := range list.Items {
		ns := &list.Items[i]
		if len(ns.Annotations) > 0 {
			out[ns.Name] = ns.Annotations
		}
	}
	return out, nil
}
