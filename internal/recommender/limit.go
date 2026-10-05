package recommender

import (
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Stage names of the limit side that are not a signal's: one per limit
// strategy, named after its Policy field, and the guard.
const (
	stageKeepLimit             = "keepLimit"
	stageNoLimit               = "noLimit"
	stageEqualsToRequest       = "equalsToRequest"
	stageRequestsLimitsRatio   = "requestsLimitsRatio"
	stageKeepLimitRequestRatio = "keepLimitRequestRatio"
	stageGuard                 = "guard"
)

const factRatio = "ratio"

// limit is a resource's limit as its chain carries it.
type limit struct {
	// value is the limit, nil for none.
	value *resource.Quantity
	// removed strips the container's limit.
	removed bool
	// set says the Recommendation sets value. Otherwise the container keeps
	// its own limit, which value then is.
	set bool
}

// recommendation is the limit as a Recommendation carries it: a quantity to
// set, the intent to strip the limit, or neither to leave the container's own.
func (l limit) recommendation() (*resource.Quantity, bool) {
	if l.set {
		return l.value, false
	}
	return nil, l.removed
}

// stage is a limit stage named name holding l.
func (l limit) stage(name string, facts map[string]string) sustainv1alpha1.TraceStage {
	st := sustainv1alpha1.TraceStage{Stage: name, Removed: l.removed, Facts: facts}
	if l.value != nil {
		v := l.value.DeepCopy()
		st.Value = &v
	}
	return st
}

// limitChain runs one container's limit for res through its stages: the
// Policy's limit strategy applied to the final request, the limit adjusters,
// then the guard. A limit below the request, derived or the container's own,
// is rejected by the apiserver, so the guard raises it to the request; the
// request is never capped instead, as it may carry the OOM floor
// (docs/adr/0005).
func limitChain(
	res resourceKind,
	request resource.Quantity,
	c corev1.Container,
	in *WorkloadInputs,
	cfg sustainv1alpha1.ResourceLimitsConfig,
) (limit, []sustainv1alpha1.TraceStage) {
	l, strategyStage := strategy(res, request, c.Resources, cfg)
	stages := []sustainv1alpha1.TraceStage{strategyStage}

	var raised []contributed
	winner := -1
	for _, s := range inSlot(slot{role: limitAdjuster, resource: res}) {
		ct, ok := s.contribute(in, c.Name)
		if !ok {
			continue
		}
		// Rounded up to a whole millicore or MiB, as a request is.
		if q := res.withHeadroom(ct.value, nil); l.value != nil && q.Cmp(*l.value) > 0 {
			l, winner = limit{value: &q, set: true}, len(raised)
		}
		raised = append(raised, contributed{signal: s, value: ct.value})
	}
	guarded := l.value != nil && l.value.Cmp(request) < 0
	if guarded {
		q := request.DeepCopy()
		l, winner = limit{value: &q, set: true}, -1
	}

	for i, r := range raised {
		stages = append(stages, r.signal.stage(in, c.Name, r.value, i == winner))
	}
	if guarded {
		stages = append(stages, l.stage(stageGuard, nil))
	}
	return l, stages
}

// strategy applies the Policy's limit strategy to the final request. A
// strategy that cannot derive a limit keeps the container's own.
func strategy(
	res resourceKind,
	request resource.Quantity,
	current corev1.ResourceRequirements,
	cfg sustainv1alpha1.ResourceLimitsConfig,
) (limit, sustainv1alpha1.TraceStage) {
	own := limit{value: res.of(current.Limits)}
	switch {
	case cfg.KeepLimit:
		return own, own.stage(stageKeepLimit, nil)
	case cfg.NoLimit:
		l := limit{removed: true}
		return l, l.stage(stageNoLimit, nil)
	case cfg.EqualsToRequest:
		q := request.DeepCopy()
		l := limit{value: &q, set: true}
		return l, l.stage(stageEqualsToRequest, nil)
	case cfg.RequestsLimitsRatio != nil:
		return ratioLimit(stageRequestsLimitsRatio, request, *cfg.RequestsLimitsRatio)
	case cfg.KeepLimitRequestRatio:
		if req := res.of(current.Requests); req != nil && own.value != nil {
			return ratioLimit(stageKeepLimitRequestRatio, request, float64(own.value.MilliValue())/float64(req.MilliValue()))
		}
		return own, own.stage(stageKeepLimitRequestRatio, nil)
	}
	return own, own.stage(stageKeepLimit, nil)
}

func ratioLimit(name string, request resource.Quantity, ratio float64) (limit, sustainv1alpha1.TraceStage) {
	l := limit{value: scaleByRatio(request, ratio), set: true}
	return l, l.stage(name, map[string]string{factRatio: formatFactor(ratio)})
}

// scaleByRatio multiplies request by ratio, rounding up. Memory quantities
// (BinarySI) are scaled in whole bytes — milli math would carry float64
// representation error into fractional-byte limits (e.g. 100Mi * 1.1 →
// 115343360001 milli-bytes), which Kubernetes warns about in pod specs.
// CPU keeps millivalue precision.
func scaleByRatio(request resource.Quantity, ratio float64) *resource.Quantity {
	if request.Format == resource.BinarySI {
		return resource.NewQuantity(
			int64(math.Ceil(float64(request.Value())*ratio)),
			request.Format,
		)
	}
	return resource.NewMilliQuantity(
		int64(math.Ceil(float64(request.MilliValue())*ratio)),
		request.Format,
	)
}
