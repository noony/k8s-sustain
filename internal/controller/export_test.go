package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/noony/k8s-sustain/internal/recommender"
)

var registryForTest = metrics.Registry

// ApplyTuningDefaultsForTest exposes applyTuningDefaults to the external
// controller_test package, which can then import internal/config to compare the
// fallbacks against the CLI defaults without the production package gaining
// that dependency.
func ApplyTuningDefaultsForTest(r *PolicyReconciler) { r.applyTuningDefaults() }

// ReconcilerForTest is reconcilerOn for the external controller_test package,
// which can then drive Reconcile next to the dashboard it imports.
func ReconcilerForTest(c client.Client, inputs recommender.InputsFetcher) *PolicyReconciler {
	return reconcilerOn(c, c.Scheme(), inputs, false)
}
