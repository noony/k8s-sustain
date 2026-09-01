package controller

import (
	"context"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/controller"
	"github.com/noony/k8s-sustain/internal/logging"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/version"
)

func init() {
	config.BindControllerFlags(startCmd)
	rootCmd.AddCommand(startCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the k8s-sustain controller",
	Args:  cobra.NoArgs,
	RunE:  runStart,
}

func runStart(_ *cobra.Command, _ []string) error {
	cfg, err := config.LoadControllerConfig()
	if err != nil {
		return err
	}
	log := logging.Setup(cfg.LogLevel, "setup")

	log.Info(
		"Starting k8s-sustain operator",
		"version", version.Version,
		"metricsAddr", cfg.MetricsBindAddress,
		"healthAddr", cfg.HealthProbeBindAddress,
		"leaderElect", cfg.LeaderElect,
		"leaderElectionID", cfg.LeaderElectionID,
		"prometheusAddr", cfg.PrometheusAddress,
		"reconcileInterval", cfg.ReconcileInterval,
		"workloadConcurrencyLimit", cfg.WorkloadConcurrencyLimit,
		"policyConcurrencyLimit", cfg.PolicyConcurrencyLimit,
		"prometheusMaxInflight", cfg.PrometheusMaxInflight,
		"recommendOnly", cfg.RecommendOnly,
		"recommendationRetention", cfg.RecommendationRetention,
		"queryShardMaxSamples", cfg.QueryShardMaxSamples,
	)

	promClient, err := promclient.New(cfg.PrometheusAddress,
		promclient.WithMaxInflight(cfg.PrometheusMaxInflight),
		promclient.WithTransportConfig(cfg.PrometheusTransport))
	if err != nil {
		log.Error(err, "Unable to create Prometheus client")
		return err
	}

	restCfg := ctrl.GetConfigOrDie()
	inPlaceUpdates := detectInPlaceSupport(restCfg, log)
	log.Info("InPlacePodVerticalScaling support", "enabled", inPlaceUpdates)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: config.Scheme(),
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsBindAddress,
		},
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		LeaderElection:         cfg.LeaderElect,
		LeaderElectionID:       cfg.LeaderElectionID,
	})
	if err != nil {
		log.Error(err, "Unable to create manager")
		return err
	}

	oomCache := oomwatch.NewCache(oomwatch.DefaultRecentMaxAge)
	oomCache.SizeObserver = controller.SetOOMCacheEntries
	// Big enough to absorb a rolling-restart burst across a few hundred pods;
	// small enough that a stuck reconciler does not pile up unbounded events.
	// Drops on overflow are safe — the next ReconcileInterval tick catches up.
	const oomTriggerBuffer = 256
	triggerCh := make(chan event.GenericEvent, oomTriggerBuffer)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		oomCache.Run(ctx)
		return nil
	})); err != nil {
		log.Error(err, "Unable to start OOM cache sweeper")
		return err
	}
	oomHandler := oomwatch.EventHandlerFunc(func(ctx context.Context, key oomwatch.Key, rec oomwatch.OOMRecord) {
		if rec.PolicyName == "" {
			return
		}
		controller.EmitOOMObserved(key.Namespace, key.OwnerKind, key.OwnerName, key.Container)
		select {
		case triggerCh <- event.GenericEvent{Object: &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: rec.PolicyName}}}:
		default:
			// Channel full: the next reconcile interval will catch up via
			// Prometheus. Better than blocking the watcher.
		}
	})
	watcher := &oomwatch.Watcher{
		Client:  mgr.GetClient(),
		Sink:    oomCache,
		Handler: oomHandler,
	}
	if err := watcher.SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create OOM watcher")
		return err
	}

	if err := (&controller.PolicyReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		PrometheusClient:          promClient,
		ReconcileInterval:         cfg.ReconcileInterval,
		InPlaceUpdates:            inPlaceUpdates,
		ExcludedNamespaces:        cfg.ExcludedNamespaces,
		RecommendOnly:             cfg.RecommendOnly,
		WorkloadConcurrencyLimit:  cfg.WorkloadConcurrencyLimit,
		PolicyConcurrencyLimit:    cfg.PolicyConcurrencyLimit,
		RecycleReplacementTimeout: cfg.RecycleReplacementTimeout,
		RecommendationRetention:   cfg.RecommendationRetention,
		QueryShardMaxSamples:      cfg.QueryShardMaxSamples,
		LiveOOM: controller.LiveOOMConfig{
			Source:    oomCache,
			TriggerCh: triggerCh,
		},
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create Policy controller")
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "Unable to set up health check")
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "Unable to set up ready check")
		return err
	}

	log.Info("Starting controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Problem running controller")
		return err
	}

	return nil
}

// detectInPlaceSupport returns true when the cluster is k8s >= 1.33, where the
// InPlacePodVerticalScaling feature gate is beta and enabled by default and
// the pods/resize subresource is served. Earlier versions had the gate as
// alpha (disabled by default) and no /resize subresource, so we don't enable
// in-place updates there to avoid silent patch rejections.
// On any error it logs a warning and returns false (safe default).
func detectInPlaceSupport(cfg *rest.Config, log logr.Logger) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		log.Error(err, "Unable to create discovery client; in-place updates disabled")
		return false
	}
	sv, err := dc.ServerVersion()
	if err != nil {
		log.Error(err, "Unable to fetch server version; in-place updates disabled")
		return false
	}
	major, err1 := strconv.Atoi(sv.Major)
	minor, err2 := strconv.Atoi(strings.TrimRight(sv.Minor, "+"))
	if err1 != nil || err2 != nil {
		log.Info("Unable to parse server version; in-place updates disabled", "major", sv.Major, "minor", sv.Minor)
		return false
	}
	return major > 1 || (major == 1 && minor >= 33)
}
