package webhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// DefaultCacheStaleness bounds how old a WorkloadRecommendation may be before
// the webhook refuses to inject from it. Tuned to one full reconcile interval
// (5m) plus headroom for backed-up controllers and small clock skew.
const DefaultCacheStaleness = 30 * time.Minute

// DefaultRecommendationRetention is the fallback bound on the departed path
// when Handler.RecommendationRetention is unset. It duplicates the controller's
// --recommendation-retention default rather than importing internal/config —
// TestRetentionDefaultAgreesWithConfigDefault pins the two together. A zero
// field means "fall back", not "retention disabled".
const DefaultRecommendationRetention = 168 * time.Hour

// ErrRecommendationStale is returned when a WorkloadRecommendation exists but
// its ObservedAt is older than the staleness budget. Kept distinct from the
// missing case because the two are operationally different — stale means the
// controller is behind or stuck, missing means the workload was never
// reconciled — and the recommendation-source metric must tell them apart.
var ErrRecommendationStale = errors.New("workloadrecommendation is stale")

// ErrRecommendationNoData reports that a WorkloadRecommendation exists but the
// identity produced nothing recommendable — the "nodata" state
// wlrcache.MarkNoData writes. Kept distinct from the plain (nil, nil) result
// because that result drives stub creation, and a nodata object already
// exists: collapsing them would fire a doomed Create per pod for exactly the
// high-churn identities that stay nodata longest.
var ErrRecommendationNoData = errors.New("workloadrecommendation has no recommendable data")

// fetchRecommendations reads the WorkloadRecommendation the controller wrote
// for (kind, namespace, name) and returns its container map when it exists and
// was observed within staleness. This is the webhook's only recommendation
// source — it never queries Prometheus itself. A missing or unpopulated object
// yields (nil, nil); the two error cases are ErrRecommendationStale and
// ErrRecommendationNoData.
func (h *Handler) fetchRecommendations(
	ctx context.Context,
	kind, namespace, name string,
	now time.Time,
	staleness time.Duration,
) (recs map[string]workload.ContainerRecommendation, departed bool, err error) {
	objName := wlrcache.Name(kind, name)
	var wlr sustainv1alpha1.WorkloadRecommendation
	err = h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: objName}, &wlr)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading WorkloadRecommendation %s/%s: %w", namespace, objName, err)
	}
	if wlr.Status.ObservedAt.IsZero() {
		return nil, false, nil
	}
	// Nodata is checked BEFORE staleness, and the order is load-bearing:
	// MarkNoData stamps ObservedAt once and never refreshes it, so checked the
	// other way round every such admission would report source="stale" and
	// swamp the signal operators alert on.
	if len(wlr.Status.Containers) == 0 && wlr.Status.Source == sustainv1alpha1.RecommendationSourceNoData {
		return nil, false, ErrRecommendationNoData
	}
	// A recommendation retained for a departed identity is exempt from the
	// freshness gate: its ObservedAt is deliberately frozen at the last
	// successful write, so gating on it would put a daily Job back on template
	// resources on every run but its first.
	//
	// The waiver is bounded here rather than left to the controller's sweep,
	// because that sweep lives inside Reconcile and is skipped whenever
	// collectTargets fails — a wedged controller would otherwise disable the
	// staleness gate outright for these objects. Past the window this reports
	// plain staleness, which carries the same operator-facing meaning.
	age := now.Sub(wlr.Status.ObservedAt.Time)
	if wlr.Status.Departed {
		if age > h.effectiveRetention() {
			return nil, false, ErrRecommendationStale
		}
	} else if age > staleness {
		return nil, false, ErrRecommendationStale
	}
	recs = wlrcache.RecsFromStatus(wlr.Status)
	if recs == nil {
		return nil, false, nil
	}
	return recs, wlr.Status.Departed, nil
}

// effectiveRetention is the bound applied to the departed path, falling back
// to DefaultRecommendationRetention when the handler was built without one.
func (h *Handler) effectiveRetention() time.Duration {
	if h.RecommendationRetention <= 0 {
		return DefaultRecommendationRetention
	}
	return h.RecommendationRetention
}
