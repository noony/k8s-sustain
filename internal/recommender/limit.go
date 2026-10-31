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

type limit struct {
	// value is the limit, nil for none.
	value   *resource.Quantity
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
	s := strategyOf(cfg)
	l, facts := s.limit(limitBasis{res: res, request: request, current: c.Resources, cfg: cfg})
	stages := []sustainv1alpha1.TraceStage{l.stage(s.name, facts)}

	raised := contributionsTo(in, c.Name, res, limitAdjuster)
	winner := -1
	for i, ct := range raised {
		// Rounded up to a whole millicore or MiB, as a request is.
		if q := res.withHeadroom(ct.value, nil); l.value != nil && q.Cmp(*l.value) > 0 {
			l, winner = limit{value: &q, set: true}, i
		}
	}
	guarded := l.value != nil && l.value.Cmp(request) < 0
	if guarded {
		q := request.DeepCopy()
		l, winner = limit{value: &q, set: true}, -1
	}

	stages = append(stages, stagesOf(raised, in, c.Name, winner)...)
	if guarded {
		stages = append(stages, l.stage(stageGuard, nil))
	}
	return l, stages
}

// limitBasis is what a limit strategy derives a limit from: the final request
// of res, and the container's current requests and limits.
type limitBasis struct {
	res     resourceKind
	request resource.Quantity
	current corev1.ResourceRequirements
	cfg     sustainv1alpha1.ResourceLimitsConfig
}

// limitStrategy is one of the Policy's limit strategies, named after the
// field that chooses it. limit is its limit and the facts its stage records.
type limitStrategy struct {
	name   string
	chosen func(sustainv1alpha1.ResourceLimitsConfig) bool
	limit  func(limitBasis) (limit, map[string]string)
}

// limitStrategies is every limit strategy. A Policy chooses at most one, so
// their order only settles a Policy admitted before the CRD enforced that.
// The first is the default.
var limitStrategies = []limitStrategy{
	{
		name:   stageKeepLimit,
		chosen: func(cfg sustainv1alpha1.ResourceLimitsConfig) bool { return cfg.KeepLimit },
		limit:  ownLimit,
	},
	{
		name:   stageNoLimit,
		chosen: func(cfg sustainv1alpha1.ResourceLimitsConfig) bool { return cfg.NoLimit },
		limit:  func(limitBasis) (limit, map[string]string) { return limit{removed: true}, nil },
	},
	{
		name:   stageEqualsToRequest,
		chosen: func(cfg sustainv1alpha1.ResourceLimitsConfig) bool { return cfg.EqualsToRequest },
		limit: func(b limitBasis) (limit, map[string]string) {
			q := b.request.DeepCopy()
			return limit{value: &q, set: true}, nil
		},
	},
	{
		name:   stageRequestsLimitsRatio,
		chosen: func(cfg sustainv1alpha1.ResourceLimitsConfig) bool { return cfg.RequestsLimitsRatio != nil },
		limit: func(b limitBasis) (limit, map[string]string) {
			return ratioLimit(b.request, *b.cfg.RequestsLimitsRatio)
		},
	},
	{
		name:   stageKeepLimitRequestRatio,
		chosen: func(cfg sustainv1alpha1.ResourceLimitsConfig) bool { return cfg.KeepLimitRequestRatio },
		limit: func(b limitBasis) (limit, map[string]string) {
			own, req := b.res.of(b.current.Limits), b.res.of(b.current.Requests)
			if own == nil || req == nil {
				return ownLimit(b)
			}
			return ratioLimit(b.request, float64(own.MilliValue())/float64(req.MilliValue()))
		},
	},
}

// strategyOf is the limit strategy cfg chooses, the default when it chooses
// none.
func strategyOf(cfg sustainv1alpha1.ResourceLimitsConfig) limitStrategy {
	for _, s := range limitStrategies {
		if s.chosen(cfg) {
			return s
		}
	}
	return limitStrategies[0]
}

func ownLimit(b limitBasis) (limit, map[string]string) {
	return limit{value: b.res.of(b.current.Limits)}, nil
}

func ratioLimit(request resource.Quantity, ratio float64) (limit, map[string]string) {
	return limit{value: scaleByRatio(request, ratio), set: true}, map[string]string{factRatio: formatFactor(ratio)}
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
