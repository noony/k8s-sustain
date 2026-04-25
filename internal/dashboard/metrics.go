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

	promQueryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_dashboard_prometheus_query_duration_seconds",
		Help:    "Time spent running a Prometheus query from the dashboard.",
		Buckets: prometheus.DefBuckets,
	}, []string{"rule"})
)

func init() {
	metrics.Registry.MustRegister(requestDuration, promQueryDuration)
}
