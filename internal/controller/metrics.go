package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	reconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_reconcile_total",
		Help: "Total number of policy reconciliations by result.",
	}, []string{"policy", "result"})

	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_reconcile_duration_seconds",
		Help:    "Duration of a policy reconciliation in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"policy"})

	workloadPatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_workload_patch_total",
		Help: "Total number of workload patches by kind and result.",
	}, []string{"kind", "result"})
)

func init() {
	metrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDuration,
		workloadPatchTotal,
	)
}
