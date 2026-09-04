package controller_test

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/controller"
)

// PolicyReconciler.SetupWithManager applies its own fallback defaults for the
// tuning knobs, so a reconciler constructed without them (in tests, or by any
// future embedder) still behaves sanely. Those fallbacks are hardcoded
// literals, duplicating the CLI defaults in internal/config — the reconciler
// deliberately does not import the config/Viper layer in production code.
//
// Duplication is fine as long as the two agree. Drift is not: if someone tunes
// a CLI default and misses the reconciler fallback, the operator's documented
// default silently stops matching what an unconfigured reconciler actually
// does, and the discrepancy shows up only as a behaviour difference nobody
// traces back here.
//
// This lives in an EXTERNAL test package (controller_test) on purpose: it lets
// the assertion import internal/config without the production internal/controller
// package gaining any dependency on the CLI layer. The dependency is
// test-only, verifiable with `go list -deps` versus `go list -test -deps`.
func TestSetupDefaultsAgreeWithConfigDefaults(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	config.BindControllerFlags(cmd)
	cfg, err := config.LoadControllerConfig()
	if err != nil {
		t.Fatalf("LoadControllerConfig: %v", err)
	}

	// A reconciler with every knob left at its zero value: SetupWithManager's
	// fallback branches are exactly what a zero value triggers.
	r := &controller.PolicyReconciler{}
	controller.ApplyTuningDefaultsForTest(r)

	cases := []struct {
		name       string
		fromSetup  int
		fromConfig int
	}{
		{"WorkloadConcurrencyLimit", r.WorkloadConcurrencyLimit, cfg.WorkloadConcurrencyLimit},
		{"PolicyConcurrencyLimit", r.PolicyConcurrencyLimit, cfg.PolicyConcurrencyLimit},
		{"QueryShardMaxSamples", r.QueryShardMaxSamples, cfg.QueryShardMaxSamples},
	}
	for _, tc := range cases {
		if tc.fromSetup != tc.fromConfig {
			t.Errorf("%s: SetupWithManager fallback is %d but the CLI default is %d — "+
				"the two must agree, or an unconfigured reconciler behaves differently "+
				"from the documented default",
				tc.name, tc.fromSetup, tc.fromConfig)
		}
	}
}
