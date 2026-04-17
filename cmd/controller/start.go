package controller

import (
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/controller"
	"github.com/noony/k8s-sustain/internal/logging"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

func init() {
	config.BindControllerFlags(startCmd)
	rootCmd.AddCommand(startCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the k8s-sustain controller",
	RunE:  runStart,
}

func runStart(_ *cobra.Command, _ []string) error {
	cfg := config.LoadControllerConfig()
	log := logging.Setup(cfg.LogLevel, "setup")

	log.Info("Starting k8s-sustain operator",
		"metricsAddr", cfg.MetricsBindAddress,
		"healthAddr", cfg.HealthProbeBindAddress,
		"leaderElect", cfg.LeaderElect,
		"prometheusAddr", cfg.PrometheusAddress,
		"reconcileInterval", cfg.ReconcileInterval,
		"recommendOnly", cfg.RecommendOnly,
	)

	promClient, err := promclient.New(cfg.PrometheusAddress)
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
		LeaderElectionID:       "k8s-sustain-leader-election",
	})
	if err != nil {
		log.Error(err, "Unable to create manager")
		return err
	}

	if err := (&controller.PolicyReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		PrometheusClient:   promClient,
		ReconcileInterval:  cfg.ReconcileInterval,
		InPlaceUpdates:     inPlaceUpdates,
		ExcludedNamespaces: cfg.ExcludedNamespaces,
		RecommendOnly:      cfg.RecommendOnly,
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

// detectInPlaceSupport returns true when the cluster is k8s ≥ 1.27, which is
// the first version to support InPlacePodVerticalScaling (alpha gate).
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
	return major > 1 || (major == 1 && minor >= 27)
}
