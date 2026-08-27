package webhook

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Recommendation-source label values for RecommendationSourceTotal. Kept as
// named constants (rather than inline string literals at each call site) so
// the four outcomes can't silently drift apart between admit() and any test
// or dashboard query that references them.
const (
	// RecSourceHit means a fresh, usable WorkloadRecommendation was read for
	// this admission — the normal, healthy-pipeline outcome.
	RecSourceHit = "hit"
	// RecSourceStale means a WorkloadRecommendation object existed but its
	// ObservedAt was older than the staleness budget (DefaultCacheStaleness
	// or Handler.CacheStaleness) — the controller reconcile loop is falling
	// behind, stuck, or the workload was deleted from its policy's scope.
	RecSourceStale = "stale"
	// RecSourceMissing means no WorkloadRecommendation existed for this
	// workload identity at all — expected transiently right after a Policy
	// or workload is created, before the controller's first reconcile. This
	// outcome also triggers a stub WorkloadRecommendation create (see
	// Handler.requestRecommendation), so a *sustained* missing rate for the
	// same identity means the controller is not computing it.
	RecSourceMissing = "missing"
	// RecSourceNoData means a WorkloadRecommendation exists and was evaluated
	// but the identity produced nothing recommendable (too young, no metrics,
	// or the workload object is gone) — the "nodata" state. Distinct from
	// "missing" because it is NOT a request for work: the object is already in
	// the controller's work-list and is recomputed every reconcile interval,
	// so no stub is created on this path. A sustained rate here on a workload
	// you expect to be sized is the signal that its identity is not
	// accumulating history — e.g. a Job whose name changes every run.
	RecSourceNoData = "nodata"
	// RecSourceError means the WorkloadRecommendation read itself failed
	// (apiserver error other than NotFound) — distinct from "missing"
	// because it points at an apiserver/RBAC/cache problem rather than an
	// unreconciled workload.
	RecSourceError = "error"
	// RecSourceRetained means the injected recommendation came from an object
	// the controller is deliberately keeping for a workload identity that has
	// departed — a completed Job, a bare-pod group between runs. The pod WAS
	// rightsized, so this is a success like "hit", but it is counted separately
	// because the data is last-known-good rather than fresh. The identity IS
	// still recomputed every reconcile interval — computation is driven by the
	// WorkloadRecommendation list, not by a workload listing — but a recompute
	// that produced data would have cleared Departed, so reaching this outcome
	// means none has since the sweep confirmed the absence. ObservedAt is
	// deliberately left frozen in that state, so the value's age is bounded
	// only by --recommendation-retention (default 168h), not by the staleness
	// budget. The webhook applies that bound itself (Handler.RecommendationRetention)
	// rather than assuming the controller's sweep has deleted anything past it:
	// the sweep lives inside Reconcile, which returns early when the target
	// listing fails, so a wedged controller would otherwise leave the waiver
	// unbounded. Past the window the read reports "stale" instead.
	//
	// Steady traffic here is the expected, healthy shape for recurring
	// ephemeral workloads. It is worth watching only against
	// --recommendation-retention: an identity whose gap between runs exceeds
	// that window has its object reaped in between, and its counts move to
	// "missing" instead.
	RecSourceRetained = "retained"
)

// RequestDuration tracks how long each webhook HTTP request takes, labelled
// by route and HTTP status text. PanicTotal counts panics caught by the
// httpx recovery middleware.
//
// RecommendationSourceTotal is the operator-facing signal for whether the
// WorkloadRecommendation pipeline the webhook now depends on exclusively is
// actually keeping up. Since Prometheus was removed from the admission path,
// a WLR read is the *only* way a pod can get rightsized resources at
// creation time — if the controller falls behind (stale) or a workload's WLR
// was never populated (missing) or the read itself fails (error), pods start
// on template resources with nothing in their own status to reveal it. A pod
// spec never says "I would have been rightsized but the recommendation
// pipeline was unhealthy" — this counter is the only place that fact is
// visible. A rising stale-or-missing rate (relative to hit) is the alerting
// signal an operator should watch for that condition.
//
// The collectors are declared at package scope but not auto-registered; the
// webhook serve command builds a dedicated prometheus.Registry and wires
// these (alongside the cert-expiry gauge) into it. Tests that exercise the
// handler in isolation can ignore them.
var (
	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_webhook_request_duration_seconds",
		Help:    "Webhook HTTP request duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path", "status"})

	PanicTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_webhook_panic_total",
		Help: "Number of panics recovered by the webhook middleware, by request path.",
	}, []string{"path"})

	RecommendationSourceTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_webhook_recommendation_source_total",
		Help: "Outcome of each admission's WorkloadRecommendation read, by source: hit, retained, stale, missing, nodata, error.",
	}, []string{"source"})
)

// RegisterMetrics attaches the webhook's request-level metrics to reg.
// Registration is idempotent across processes that share a registry: a
// second call with the same collector is silently absorbed.
func RegisterMetrics(reg prometheus.Registerer) {
	for _, c := range []prometheus.Collector{RequestDuration, PanicTotal, RecommendationSourceTotal} {
		if err := reg.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				// Surfaces only programming errors (e.g. duplicate name with
				// different help); panic so it shows up loudly in tests.
				panic(err)
			}
		}
	}
}
