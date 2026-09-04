// Package dashboard registers the "dashboard" subcommand, an HTTP server for
// the k8s-sustain web UI.
package dashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/noony/k8s-sustain/cmd/controller"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/dashboard"
	"github.com/noony/k8s-sustain/internal/httpx"
	k8sclient "github.com/noony/k8s-sustain/internal/k8s"
	"github.com/noony/k8s-sustain/internal/logging"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/version"
)

func init() {
	config.BindDashboardFlags(dashCmd)
	controller.RootCmd().AddCommand(dashCmd)
}

var dashCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Start the k8s-sustain web dashboard",
	Long: `Starts an HTTP server that serves the k8s-sustain dashboard UI.

The dashboard provides:
  - Policy overview and exploration
  - Per-workload CPU and memory usage graphs
  - Policy simulator for testing parameter changes against historical data`,
	Args: cobra.NoArgs,
	RunE: runDashboard,
}

func runDashboard(_ *cobra.Command, _ []string) error {
	cfg, err := config.LoadDashboardConfig()
	if err != nil {
		return err
	}
	log := logging.Setup(cfg.LogLevel, "dashboard")

	promClient, err := promclient.New(cfg.PrometheusAddress,
		promclient.WithTransportConfig(cfg.PrometheusTransport))
	if err != nil {
		return fmt.Errorf("creating prometheus client: %w", err)
	}

	if err := promClient.Ping(context.Background()); err != nil {
		log.Error(err, "Prometheus is not reachable at startup — metrics queries will fail until it becomes available", "address", cfg.PrometheusAddress)
	} else {
		log.Info("Prometheus connectivity verified", "address", cfg.PrometheusAddress)
	}

	k8sClient, err := k8sclient.New(config.Scheme())
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	srv := &dashboard.Server{
		K8sClient:          k8sClient,
		PromClient:         promClient,
		Logger:             log,
		CORSOrigins:        cfg.CORSAllowedOrigins,
		ExcludedNamespaces: cfg.ExcludedNamespaces,
	}

	httpSrv := srv.NewHTTPServer(cfg.BindAddress)
	log.Info("Starting dashboard server", "version", version.Version, "addr", cfg.BindAddress)

	// The subcommand owns the process's only signal source: SetupSignalHandler
	// may be called once per process, and httpx deliberately registers none.
	return httpx.ListenAndServeWithShutdown(
		ctrl.SetupSignalHandler(), httpSrv, log, "dashboard", 10*time.Second, httpSrv.ListenAndServe)
}
