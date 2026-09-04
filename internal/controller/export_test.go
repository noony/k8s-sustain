package controller

import "sigs.k8s.io/controller-runtime/pkg/metrics"

var registryForTest = metrics.Registry

// ApplyTuningDefaultsForTest exposes applyTuningDefaults to the external
// controller_test package, which can then import internal/config to compare the
// fallbacks against the CLI defaults without the production package gaining
// that dependency.
func ApplyTuningDefaultsForTest(r *PolicyReconciler) { r.applyTuningDefaults() }
