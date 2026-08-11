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

// DefaultRecommendationRetention is the fallback bound on the DEPARTED path
// (see fetchRecommendations) when Handler.RecommendationRetention is unset.
//
// It duplicates the controller's --recommendation-retention default rather than
// importing internal/config, exactly as internal/controller's own tuning
// fallbacks do — the webhook has no business depending on the flag/Viper layer.
// TestRetentionDefaultAgreesWithConfigDefault pins the two together.
//
// A zero field is a fallback, not "retention disabled": with retention disabled
// the controller never marks anything Departed in the first place
// (retainDepartedWLR bails before markDeparted), so this value only ever bounds
// objects some controller did decide to retain.
const DefaultRecommendationRetention = 168 * time.Hour

// ErrRecommendationStale is returned by fetchRecommendations when a
// WorkloadRecommendation exists but its ObservedAt is older than the
// staleness budget. It exists so admit() can tell "stale" apart from
// "missing entirely" with errors.Is, instead of both collapsing into the
// same (nil, nil) result: the two are operationally different (a stale WLR
// means the controller is falling behind or stuck; a missing one just means
// the workload hasn't been reconciled yet, e.g. right after Policy
// creation) and the recommendation-source metric below needs to be able to
// tell them apart. As with every other fetchRecommendations outcome, a stale
// result still means "do not inject" -- the recommendation returned
// alongside this error is always nil.
var ErrRecommendationStale = errors.New("workloadrecommendation is stale")

// ErrRecommendationNoData reports that a WorkloadRecommendation exists and was
// evaluated, but the identity produced nothing recommendable — the "nodata"
// state wlrcache.MarkNoData writes. Not terminal: the controller recomputes
// the identity every cycle and it converges once Prometheus has history.
//
// Split out from the plain (nil, nil) "nothing to inject" result for one
// concrete reason: that result is what drives the webhook to create a stub,
// and a nodata object is one that already exists. Collapsing the two would
// make every admission for such an identity fire a detached Create that can
// only ever return AlreadyExists, plus createStub's follow-up Get — apiserver
// calls per pod for as long as the identity has no history, concentrated on
// exactly the high-churn identities (short recurring Jobs, bare-pod groups)
// that stay nodata longest. That is the same waste the stale path is
// deliberately written to avoid.
var ErrRecommendationNoData = errors.New("workloadrecommendation has no recommendable data")

// fetchRecommendations reads the WorkloadRecommendation the controller wrote
// for (kind, namespace, name) and returns its container map if it exists and
// was observed within staleness. This is the webhook's only recommendation
// source — it never queries Prometheus itself.
//
// Returns (nil, nil) when the WLR is missing (or exists but has never been
// populated, or carries no container data) — the caller treats that as
// "nothing to inject yet" and admits the pod unmutated. Returns
// (nil, ErrRecommendationStale) specifically when the WLR exists but is
// older than staleness — see that error's doc comment for why this case is
// split out. Returns (nil, ErrRecommendationNoData) when the WLR is marked as
// having nothing to recommend; that check precedes the staleness one (see
// below), because that mark is stamped once and never refreshed and so would
// otherwise read as permanently stale.
func (h *Handler) fetchRecommendations(
	ctx context.Context,
	kind, namespace, name string,
	now time.Time,
	staleness time.Duration,
) (recs map[string]workload.ContainerRecommendation, departed bool, err error) {
	objName := wlrName(kind, name)
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
	// Nodata is checked BEFORE staleness, and the order is load-bearing.
	// wlrcache.MarkNoData stamps ObservedAt once and returns early on every
	// later cycle, so a nodata object is older than staleness within minutes
	// and stays that way for as long as the identity has no history — with no
	// reap to bound it. Checked the other way round, every admission of such an
	// identity reports source="stale" — which docs/reference/metrics.md
	// defines as "the controller has fallen behind, is stuck, or Prometheus is
	// down" and tells operators to alert on. On any cluster with recurring short
	// Jobs or bare-pod groups (precisely the identities that stay nodata longest)
	// that healthy traffic would swamp the stale signal and leave the nodata
	// bucket unreachable. "There is nothing to serve" is a fact about the
	// identity's data; staleness is a statement about freshness, and it does not
	// apply to a mark that is deliberately never refreshed.
	if len(wlr.Status.Containers) == 0 && wlr.Status.Source == sustainv1alpha1.RecommendationSourceNoData {
		return nil, false, ErrRecommendationNoData
	}
	// A recommendation the controller is deliberately RETAINING for a departed
	// identity is exempt from the freshness gate. The identity is still
	// recomputed every cycle, but a recompute that finds nothing — its samples
	// have aged out of the query window — deliberately writes nothing rather
	// than overwriting a last-known-good with an empty status, and so does not
	// bump ObservedAt either. ObservedAt therefore stays frozen at the last
	// successful write and exceeds any staleness budget within minutes. Gating
	// on it would admit a daily Job on template resources on every run but its
	// first, while the retained recommendation sitting right there is precisely
	// the last-known-good the retention window exists to preserve.
	//
	// The waiver is BOUNDED by the retention window itself, and checked here
	// rather than left to the sweep. The sweep would delete this object once
	// retention lapsed — but the sweep, and the EnsureExists that clears
	// Departed, both live inside Reconcile, which returns early before either
	// whenever collectTargets fails (RBAC revoked on one workload kind, an
	// unreachable API group, a removed CRD). A persistently failing or wedged
	// controller freezes the flag set, and an unbounded waiver would then inject
	// this identity's last-known-good on every admission at any age, including
	// long after the workload came back to life — which is exactly the
	// "controller has fallen behind" case the staleness gate exists for, with
	// the gate silently disabled. Past the window the object is one the sweep
	// should already have deleted, so this reports plain staleness: the
	// operator-facing meaning ("the controller is stuck or behind") is the same
	// one docs/reference/metrics.md attaches to source="stale".
	//
	// This does not weaken the stale signal. Departed is set only on a
	// positively-confirmed absence, and a workload the controller is merely
	// failing to refresh stays in the target set — so it is never marked, and
	// still trips the gate below. See WorkloadRecommendationStatus.Departed.
	age := now.Sub(wlr.Status.ObservedAt.Time)
	if wlr.Status.Departed {
		if age > h.effectiveRetention() {
			return nil, false, ErrRecommendationStale
		}
	} else if age > staleness {
		return nil, false, ErrRecommendationStale
	}
	if len(wlr.Status.Containers) == 0 {
		return nil, false, nil
	}

	out := make(map[string]workload.ContainerRecommendation, len(wlr.Status.Containers))
	for cname, c := range wlr.Status.Containers {
		out[cname] = workload.ContainerRecommendation{
			CPURequest:        c.CPURequest,
			MemoryRequest:     c.MemoryRequest,
			CPULimit:          c.CPULimit,
			MemoryLimit:       c.MemoryLimit,
			RemoveCPULimit:    c.RemoveCPULimit,
			RemoveMemoryLimit: c.RemoveMemoryLimit,
		}
	}
	return out, wlr.Status.Departed, nil
}

// effectiveRetention is the bound applied to the departed path, falling back
// to DefaultRecommendationRetention when the handler was built without one.
func (h *Handler) effectiveRetention() time.Duration {
	if h.RecommendationRetention <= 0 {
		return DefaultRecommendationRetention
	}
	return h.RecommendationRetention
}

// wlrName delegates to the shared cache package — controller and webhook
// must agree on WLR object names or the read contract breaks silently.
func wlrName(kind, name string) string { return wlrcache.Name(kind, name) }
