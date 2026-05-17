package webhook

import "github.com/prometheus/client_golang/prometheus"

// RequestDuration tracks how long each webhook HTTP request takes, labelled
// by route and HTTP status text. PanicTotal counts panics caught by the
// httpx recovery middleware.
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
)

// RegisterMetrics attaches the webhook's request-level metrics to reg.
// Registration is idempotent across processes that share a registry: a
// second call with the same collector is silently absorbed.
func RegisterMetrics(reg prometheus.Registerer) {
	for _, c := range []prometheus.Collector{RequestDuration, PanicTotal} {
		if err := reg.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				// Surfaces only programming errors (e.g. duplicate name with
				// different help); panic so it shows up loudly in tests.
				panic(err)
			}
		}
	}
}
