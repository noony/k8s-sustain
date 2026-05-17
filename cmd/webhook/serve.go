// Package webhook registers the "webhook" subcommand with the root cobra command.
// It starts a TLS HTTPS server that handles mutating admission requests for Pods,
// injecting resource recommendations from policies with OnCreate update mode.
package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/noony/k8s-sustain/cmd/controller"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/httpx"
	k8sclient "github.com/noony/k8s-sustain/internal/k8s"
	"github.com/noony/k8s-sustain/internal/logging"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	whhandler "github.com/noony/k8s-sustain/internal/webhook"
)

func init() {
	config.BindWebhookFlags(serveCmd)
	controller.RootCmd().AddCommand(serveCmd)
}

var serveCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Start the k8s-sustain mutating admission webhook server",
	Long: `Starts an HTTPS server that intercepts Pod CREATE requests and injects
resource requests/limits based on matching policies with OnCreate update mode.

Requires a TLS certificate and key (--tls-cert-file / --tls-key-file).
Use cert-manager or provide a pre-existing Secret mounted at /tls.`,
	RunE: runWebhook,
}

func runWebhook(_ *cobra.Command, _ []string) error {
	cfg := config.LoadWebhookConfig()
	log := logging.Setup(cfg.LogLevel, "webhook")

	// Tight per-query timeout: the webhook must fit within the apiserver's
	// MutatingWebhookConfiguration timeout (5s by default). Background queries
	// from the controller use the longer default.
	promClient, err := promclient.New(cfg.PrometheusAddress, promclient.WithQueryTimeout(2*time.Second))
	if err != nil {
		log.Error(err, "Unable to create Prometheus client")
		return err
	}

	k8sClient, err := k8sclient.New(config.Scheme())
	if err != nil {
		log.Error(err, "Unable to create Kubernetes client")
		return err
	}

	// Validate TLS files exist before starting the server.
	if _, err := os.Stat(cfg.TLSCertFile); err != nil {
		return fmt.Errorf("tls cert file %q: %w", cfg.TLSCertFile, err)
	}
	if _, err := os.Stat(cfg.TLSKeyFile); err != nil {
		return fmt.Errorf("tls key file %q: %w", cfg.TLSKeyFile, err)
	}

	handler := &whhandler.Handler{
		Client:             k8sClient,
		PrometheusClient:   promClient,
		RecommendOnly:      cfg.RecommendOnly,
		ExcludedNamespaces: cfg.ExcludedNamespaces,
	}

	registry := prometheus.NewRegistry()
	whhandler.RegisterMetrics(registry)
	certWatcher, err := whhandler.NewCertExpiry(cfg.TLSCertFile, cfg.TLSKeyFile, log, registry)
	if err != nil {
		log.Error(err, "Unable to register cert expiry gauge; continuing without it")
	} else {
		if err := certWatcher.Refresh(); err != nil {
			// Refresh failed: we have no keypair loaded, so the server cannot
			// serve TLS. Bail out rather than starting a broken listener.
			return fmt.Errorf("initial TLS cert load: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/mutate", handler)
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Shared HTTP stack: request-ID correlation, panic recovery, telemetry,
	// matching what the dashboard exposes. Order matters — telemetry needs
	// to wrap recovery so a recovered request is still observed.
	//
	// Route labels are derived via DefaultRouteLabeler so the histogram and
	// panic counter only ever see the registered patterns (/mutate, /metrics,
	// /healthz). Without that, an attacker hitting bogus URLs would blow up
	// Prometheus label cardinality on the webhook the same way it would on
	// the dashboard.
	wrapped := httpx.WithTelemetry(
		httpx.WithRecovery(
			httpx.WithRequestID(mux),
			log,
			func(path string) { whhandler.PanicTotal.WithLabelValues(path).Inc() },
			httpx.DefaultRouteLabeler,
		),
		log,
		func(path, status string, dur time.Duration) {
			whhandler.RequestDuration.WithLabelValues(path, status).Observe(dur.Seconds())
		},
		httpx.DefaultRouteLabeler,
	)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           wrapped,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if certWatcher != nil {
		// Hot-reload path: GetCertificate is consulted on every TLS handshake,
		// so cert-manager rotations are picked up at the next Refresh tick
		// without a process restart.
		srv.TLSConfig = &tls.Config{
			GetCertificate: certWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	log.Info("Starting webhook server", "addr", addr, "certFile", cfg.TLSCertFile)

	watcherCtx, stopWatcher := context.WithCancel(context.Background())
	defer stopWatcher()
	if certWatcher != nil {
		go certWatcher.Run(watcherCtx, time.Hour)
	}

	return httpx.ListenAndServeWithShutdown(srv, log, "webhook", 10*time.Second, func() error {
		// Empty cert/key paths: the keypair comes from TLSConfig.GetCertificate.
		// Falls back to disk-loading paths when GetCertificate isn't wired
		// (e.g. cert watcher init failed).
		certPath, keyPath := "", ""
		if certWatcher == nil {
			certPath, keyPath = cfg.TLSCertFile, cfg.TLSKeyFile
		}
		return srv.ListenAndServeTLS(certPath, keyPath)
	})
}
