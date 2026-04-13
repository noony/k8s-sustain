package manager

import (
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/controller"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sustainv1alpha1.AddToScheme(scheme))

	startCmd.Flags().String("metrics-bind-address", ":8080", "Address the metrics endpoint binds to")
	startCmd.Flags().String("health-probe-bind-address", ":8081", "Address the health probe endpoint binds to")
	startCmd.Flags().Bool("leader-elect", false, "Enable leader election for high availability")
	startCmd.Flags().String("zap-log-level", "info", "Log level (debug, info, warn, error)")
	startCmd.Flags().String("prometheus-address", "http://localhost:9090", "Address of the Prometheus server used for metric queries")
	startCmd.Flags().Duration("reconcile-interval", time.Hour, "How often policies are re-evaluated")
	startCmd.Flags().StringSlice("excluded-namespaces", nil, "Namespaces the reconciler should never touch")

	_ = viper.BindPFlag("metrics-bind-address", startCmd.Flags().Lookup("metrics-bind-address"))
	_ = viper.BindPFlag("health-probe-bind-address", startCmd.Flags().Lookup("health-probe-bind-address"))
	_ = viper.BindPFlag("leader-elect", startCmd.Flags().Lookup("leader-elect"))
	_ = viper.BindPFlag("zap-log-level", startCmd.Flags().Lookup("zap-log-level"))
	_ = viper.BindPFlag("prometheus-address", startCmd.Flags().Lookup("prometheus-address"))
	_ = viper.BindPFlag("reconcile-interval", startCmd.Flags().Lookup("reconcile-interval"))
	_ = viper.BindPFlag("excluded-namespaces", startCmd.Flags().Lookup("excluded-namespaces"))

	rootCmd.AddCommand(startCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the k8s-sustain operator manager",
	RunE:  runStart,
}

func runStart(_ *cobra.Command, _ []string) error {
	logLevel := zapcore.InfoLevel
	if err := logLevel.UnmarshalText([]byte(viper.GetString("zap-log-level"))); err != nil {
		logLevel = zapcore.InfoLevel
	}

	logger := ctrlzap.New(ctrlzap.UseDevMode(logLevel == zapcore.DebugLevel), ctrlzap.Level(logLevel))
	ctrl.SetLogger(logger)

	log := ctrl.Log.WithName("setup")
	log.Info("Starting k8s-sustain operator",
		"metricsAddr", viper.GetString("metrics-bind-address"),
		"healthAddr", viper.GetString("health-probe-bind-address"),
		"leaderElect", viper.GetBool("leader-elect"),
		"prometheusAddr", viper.GetString("prometheus-address"),
		"reconcileInterval", viper.GetDuration("reconcile-interval"),
	)

	promClient, err := promclient.New(viper.GetString("prometheus-address"))
	if err != nil {
		log.Error(err, "Unable to create Prometheus client")
		return err
	}

	restCfg := ctrl.GetConfigOrDie()
	inPlaceUpdates := detectInPlaceSupport(restCfg, log)
	log.Info("InPlacePodVerticalScaling support", "enabled", inPlaceUpdates)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: viper.GetString("metrics-bind-address"),
		},
		HealthProbeBindAddress: viper.GetString("health-probe-bind-address"),
		LeaderElection:         viper.GetBool("leader-elect"),
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
		ReconcileInterval:  viper.GetDuration("reconcile-interval"),
		InPlaceUpdates:     inPlaceUpdates,
		ExcludedNamespaces: viper.GetStringSlice("excluded-namespaces"),
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

	log.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Problem running manager")
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
