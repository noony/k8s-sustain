package dashboard

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_dashboard_request_duration_seconds",
		Help:    "Dashboard HTTP request duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path", "status"})

	panicTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_dashboard_panic_total",
		Help: "Number of panics recovered by the dashboard middleware, by request path.",
	}, []string{"path"})
)

func init() {
	metrics.Registry.MustRegister(requestDuration, panicTotal)
}
