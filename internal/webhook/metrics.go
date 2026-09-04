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
	// RecSourceNoData means a WorkloadRecommendation exists but the identity
	// produced nothing recommendable (too young, no metrics, workload gone).
	// Distinct from "missing" because no stub is created — the object is
	// already in the controller's work-list. A sustained rate here means the
	// identity is not accumulating history, e.g. a Job renamed every run.
	RecSourceNoData = "nodata"
	// RecSourceError means the WorkloadRecommendation read itself failed
	// (apiserver error other than NotFound) — distinct from "missing"
	// because it points at an apiserver/RBAC/cache problem rather than an
	// unreconciled workload.
	RecSourceError = "error"
	// RecSourceRetained means the injection came from an object the controller
	// is deliberately keeping for a departed identity — a completed Job, a
	// bare-pod group between runs. A success like "hit", but counted separately
	// because the data is last-known-good rather than fresh: ObservedAt is
	// frozen, so its age is bounded by --recommendation-retention (168h) rather
	// than by the staleness budget, and past that window the read reports
	// "stale". Steady traffic here is the healthy shape for recurring ephemeral
	// workloads; an identity whose gap between runs exceeds the retention window
	// is reaped in between and moves to "missing".
	RecSourceRetained = "retained"
)

// Panic label values for PanicTotal that are not HTTP routes. The singleflight
// leaders in optin.go run off the request goroutine, so their panics are
// recovered by sfPanicSafe rather than the httpx middleware — still webhook
// panics an operator alerting on k8s_sustain_webhook_panic_total must see.
// Constants, so they add exactly two bounded series.
const (
	panicLabelOwnerRef         = "singleflight/ownerRef"
	panicLabelOwnerAnnotations = "singleflight/ownerAnnotations"
)

// RecommendationSourceTotal is the only place the health of the
// WorkloadRecommendation pipeline is visible: a WLR read is the sole way a pod
// gets rightsized at creation time, and a pod that missed out carries nothing
// in its own spec to say so. A rising stale-or-missing rate relative to hit is
// the signal to alert on.
//
// The collectors are declared at package scope but not auto-registered; the
// webhook serve command wires them into its own registry.
var (
	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_webhook_request_duration_seconds",
		Help:    "Webhook HTTP request duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path", "status"})

	PanicTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_webhook_panic_total",
		Help: "Number of panics the webhook recovered, by request route or by the internal operation that panicked.",
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
