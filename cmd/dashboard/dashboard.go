// Package dashboard registers the "dashboard" subcommand with the root cobra command.
// It starts an HTTP server that serves the k8s-sustain web UI for exploring policies,
// viewing workload metrics, and simulating policy changes.
package dashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/noony/k8s-sustain/cmd/controller"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/dashboard"
	"github.com/noony/k8s-sustain/internal/httpx"
	k8sclient "github.com/noony/k8s-sustain/internal/k8s"
	"github.com/noony/k8s-sustain/internal/logging"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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
	cfg := config.LoadDashboardConfig()
	log := logging.Setup(cfg.LogLevel, "dashboard")

	promClient, err := promclient.New(cfg.PrometheusAddress)
	if err != nil {
		return fmt.Errorf("creating prometheus client: %w", err)
	}

	// Validate Prometheus connectivity at startup
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
		K8sClient:   k8sClient,
		PromClient:  promClient,
		Logger:      log,
		CORSOrigins: cfg.CORSAllowedOrigins,
	}

	httpSrv := srv.NewHTTPServer(cfg.BindAddress)
	log.Info("Starting dashboard server", "addr", cfg.BindAddress)
	return httpx.ListenAndServeWithShutdown(httpSrv, log, "dashboard", 10*time.Second, httpSrv.ListenAndServe)
}
