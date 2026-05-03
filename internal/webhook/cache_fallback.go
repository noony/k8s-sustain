package webhook

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// DefaultCacheStaleness bounds how old a cached WorkloadRecommendation may be
// before the webhook refuses to use it. Tuned to one full reconcile interval
// (5m) plus headroom for backed-up controllers and small clock skew.
const DefaultCacheStaleness = 30 * time.Minute

// fetchCachedRecommendations reads the WorkloadRecommendation cached by the
// controller for (kind, namespace, name) and returns its container map if
// it exists and was observed within staleness.
//
// Returns (nil, nil) when the cache is missing or stale — the caller treats
// that as "no fallback available".
func (h *Handler) fetchCachedRecommendations(
	ctx context.Context,
	kind, namespace, name string,
	now time.Time,
	staleness time.Duration,
) (map[string]workload.ContainerRecommendation, error) {
	objName := wlrName(kind, name)
	var wlr sustainv1alpha1.WorkloadRecommendation
	err := h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: objName}, &wlr)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading WorkloadRecommendation %s/%s: %w", namespace, objName, err)
	}
	if wlr.Status.ObservedAt.IsZero() {
		return nil, nil
	}
	if now.Sub(wlr.Status.ObservedAt.Time) > staleness {
		return nil, nil
	}
	if len(wlr.Status.Containers) == 0 {
		return nil, nil
	}

	out := make(map[string]workload.ContainerRecommendation, len(wlr.Status.Containers))
	for cname, c := range wlr.Status.Containers {
		out[cname] = workload.ContainerRecommendation{
			CPURequest:    c.CPURequest,
			MemoryRequest: c.MemoryRequest,
			CPULimit:      c.CPULimit,
			MemoryLimit:   c.MemoryLimit,
		}
	}
	return out, nil
}

// wlrName mirrors the controller-side name generator. Kept in sync via tests.
func wlrName(kind, name string) string {
	return fmt.Sprintf("%s-%s", strings.ToLower(kind), name)
}

// silence "unused client" import in builds where the file is read in
// isolation; client is referenced elsewhere in the package.
var _ = client.IgnoreNotFound
