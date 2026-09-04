package controller_test

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/controller"
)

// SetupWithManager's tuning fallbacks are hardcoded literals duplicating the
// CLI defaults in internal/config, because the reconciler deliberately does not
// import the config/Viper layer. Drift between the two would silently make the
// documented default stop matching an unconfigured reconciler.
//
// This lives in an EXTERNAL test package so the assertion can import
// internal/config without the production package gaining that dependency.
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
