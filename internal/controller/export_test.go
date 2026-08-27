package controller

import "sigs.k8s.io/controller-runtime/pkg/metrics"

var registryForTest = metrics.Registry

// ApplyTuningDefaultsForTest exposes applyTuningDefaults so the external
// controller_test package can assert the reconciler's fallback defaults agree
// with the CLI defaults in internal/config. Keeping the assertion in an
// external test package is what lets it import internal/config without the
// production controller package gaining that dependency.
func ApplyTuningDefaultsForTest(r *PolicyReconciler) { r.applyTuningDefaults() }
