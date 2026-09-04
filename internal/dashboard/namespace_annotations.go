package dashboard

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// namespaceAnnotations returns every Namespace's annotations, keyed by name,
// supplying the least-specific policy opt-in level (see
// policymatch.ResolvePolicy).
//
// Callers that loop over supportedWorkloadKinds must call this ONCE and thread
// the map into every listWorkloadsOfKind call, or it degrades to one
// cluster-wide List per kind. The dashboard's client is uncached (k8s.New), so
// every avoidable List is a real round-trip.
//
// A List failure is returned rather than swallowed; every caller today degrades
// to "no namespace-level opt-in" rather than failing the request.
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
