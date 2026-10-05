package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// storedRecommendation reads the WorkloadRecommendation the controller keeps
// for identity (kind, name) in namespace, nil when there is none. It is the
// webhook's only recommendation source: it never queries Prometheus itself.
func (h *Handler) storedRecommendation(ctx context.Context, namespace, kind, name string) (*sustainv1alpha1.WorkloadRecommendation, error) {
	objName := wlrcache.Name(kind, name)
	var wlr sustainv1alpha1.WorkloadRecommendation
	err := h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: objName}, &wlr)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading WorkloadRecommendation %s/%s: %w", namespace, objName, err)
	}
	return &wlr, nil
}

// freshness is the bound the read verdict applies, the handler's own or the
// defaults.
func (h *Handler) freshness() wlrcache.Freshness {
	return wlrcache.Freshness{Staleness: h.CacheStaleness, Retention: h.RecommendationRetention}
}

// recommendationSource is the RecommendationSourceTotal label of a verdict.
func recommendationSource(v wlrcache.Verdict) string {
	switch v {
	case wlrcache.Absent:
		return RecSourceMissing
	case wlrcache.Undecided:
		return RecSourceUndecided
	case wlrcache.NoData:
		return RecSourceNoData
	case wlrcache.Withheld:
		return RecSourceOtherPolicy
	case wlrcache.Stale:
		return RecSourceStale
	case wlrcache.Retained:
		return RecSourceRetained
	default:
		return RecSourceHit
	}
}
