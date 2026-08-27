package webhook_test

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/webhook"
)

// The webhook refuses to inject from a departed WorkloadRecommendation older
// than the controller's retention window. Handler.RecommendationRetention
// carries that window, and a handler built without one falls back to
// webhook.DefaultRecommendationRetention — a literal duplicating the CLI
// default, because the webhook deliberately does not depend on the flag/Viper
// layer (same reasoning as internal/controller's tuning fallbacks).
//
// Drift between the two is not cosmetic: too small and the webhook rejects
// recommendations the controller is still retaining, so every run of a
// recurring ephemeral workload cold-starts on template resources; too large and
// it keeps serving objects the controller's sweep should already have deleted,
// which is the unbounded waiver this bound was added to close.
//
// External test package (webhook_test) so the assertion can import
// internal/config without the production package gaining that dependency.
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

	// And the controller's, which is the value that actually decides when the
	// object is swept: the webhook's whole bound is "do not serve what the
	// controller would already have deleted".
	ctrlCmd := &cobra.Command{}
	config.BindControllerFlags(ctrlCmd)
	if got := config.LoadControllerConfig().RecommendationRetention; got != cfg.RecommendationRetention {
		t.Errorf("controller --recommendation-retention default is %s but the webhook's is %s — "+
			"the webhook's bound must be the controller's retention window", got, cfg.RecommendationRetention)
	}
}
