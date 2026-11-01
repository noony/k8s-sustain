package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// storedRecommendation reads the WorkloadRecommendation the controller keeps
// for id, nil when there is none. It is the webhook's only recommendation
// source: it never queries Prometheus itself.
func (h *Handler) storedRecommendation(ctx context.Context, id promclient.WorkloadIdentity) (*sustainv1alpha1.WorkloadRecommendation, error) {
	key := wlrcache.Key(id)
	var wlr sustainv1alpha1.WorkloadRecommendation
	err := h.Client.Get(ctx, key, &wlr)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading WorkloadRecommendation %s: %w", key, err)
	}
	return &wlr, nil
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
