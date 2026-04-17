// Package dashboard registers the "dashboard" subcommand with the root cobra command.
// It starts an HTTP server that serves the k8s-sustain web UI for exploring policies,
// viewing workload metrics, and simulating policy changes.
package dashboard

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/noony/k8s-sustain/cmd/controller"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/dashboard"
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

	restCfg := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(restCfg, client.Options{Scheme: config.Scheme()})
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

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		log.Info("Shutting down dashboard server")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}
