package webhook_test

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/webhook"
)

// webhook.DefaultRecommendationRetention duplicates the CLI default rather than
// importing the flag/Viper layer, so drift has to be pinned here: too small and
// the webhook rejects recommendations the controller still retains; too large
// and it serves objects the sweep should already have deleted.
//
// External test package so the assertion can import internal/config without the
// production package gaining that dependency.
func TestRetentionDefaultAgreesWithConfigDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{}
	config.BindWebhookFlags(cmd)
	cfg := config.LoadWebhookConfig()

	if cfg.RecommendationRetention != webhook.DefaultRecommendationRetention {
		t.Errorf("webhook CLI default is %s but Handler's fallback is %s — the two must agree, "+
			"or a handler built without an explicit retention bounds the departed path differently "+
			"from the shipped binary", cfg.RecommendationRetention, webhook.DefaultRecommendationRetention)
	}

	// The controller's value is what actually decides when the object is swept.
	ctrlCmd := &cobra.Command{}
	config.BindControllerFlags(ctrlCmd)
	ctrlCfg, err := config.LoadControllerConfig()
	if err != nil {
		t.Fatalf("LoadControllerConfig: %v", err)
	}
	if got := ctrlCfg.RecommendationRetention; got != cfg.RecommendationRetention {
		t.Errorf("controller --recommendation-retention default is %s but the webhook's is %s — "+
			"the webhook's bound must be the controller's retention window", got, cfg.RecommendationRetention)
	}
}
