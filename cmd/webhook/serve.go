// Package webhook registers the "webhook" subcommand, a TLS mutating-admission
// server that injects resource recommendations on Pod CREATE.
package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/noony/k8s-sustain/cmd/controller"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/httpx"
	k8sclient "github.com/noony/k8s-sustain/internal/k8s"
	"github.com/noony/k8s-sustain/internal/logging"
	"github.com/noony/k8s-sustain/internal/version"
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
	Args: cobra.NoArgs,
	RunE: runWebhook,
}

// Test seam: the real NewCached calls ctrl.GetConfigOrDie (which exits the
// process without a kubeconfig) and opens watches against an apiserver.
var newCachedClient = k8sclient.NewCached

func runWebhook(_ *cobra.Command, _ []string) error {
	cfg := config.LoadWebhookConfig()
	log := logging.Setup(cfg.LogLevel, "webhook")

	// The process's only signal source: SetupSignalHandler panics if called
	// twice, and it replaces Go's default SIGTERM handling with "cancel this
	// context", so everything that can block downstream must honour ctx or the
	// signal is swallowed for as long as the block lasts.
	return serve(ctrl.SetupSignalHandler(), cfg, log)
}

// serve runs the webhook server until ctx is cancelled (SIGTERM/SIGINT) or the
// listener fails. Split out from runWebhook so tests can drive it with a
// context they control instead of a real signal.
func serve(ctx context.Context, cfg config.WebhookConfig, log logr.Logger) error {
	// The informer cache and cert watcher run on a context that outlives ctx on
	// purpose: deriving them from ctx would stop the cache the instant SIGTERM
	// arrives, while admissions are still being served for up to the shutdown
	// timeout, and those requests would read a store that stopped updating.
	depsCtx, stopDeps := context.WithCancel(context.Background())
	defer stopDeps()

	// Cached because admit() Gets the Policy, the owner chain and the
	// WorkloadRecommendation on every Pod CREATE.
	//
	// The two contexts are not decoration: this call BLOCKS for up to
	// crdWaitTimeout (2m) plus the informer sync, before the HTTPS listener
	// exists. depsCtx deliberately does not track ctx, so the startup wait gets
	// ctx instead — otherwise SIGTERM is ignored for that whole window and the
	// pod only dies at the end of terminationGracePeriodSeconds.
	k8sClient, err := newCachedClient(depsCtx, ctx, config.Scheme())
	if err != nil {
		// A shutdown signal mid-startup is not a failure: exiting non-zero would
		// restart-loop the container on what is simply a rollout.
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Info("Shutdown signal received during startup; exiting before the server started")
			return nil
		}
		log.Error(err, "Unable to create Kubernetes client")
		return err
	}

	if _, err := os.Stat(cfg.TLSCertFile); err != nil {
		return fmt.Errorf("tls cert file %q: %w", cfg.TLSCertFile, err)
	}
	if _, err := os.Stat(cfg.TLSKeyFile); err != nil {
		return fmt.Errorf("tls key file %q: %w", cfg.TLSKeyFile, err)
	}

	handler := &whhandler.Handler{
		Client:             k8sClient,
		RecommendOnly:      cfg.RecommendOnly,
		ExcludedNamespaces: cfg.ExcludedNamespaces,
		// Bounds the departed-identity path, which waives the staleness gate.
		// Must match the controller's retention window; see config.BindWebhookFlags.
		RecommendationRetention: cfg.RecommendationRetention,
	}

	registry := prometheus.NewRegistry()
	whhandler.RegisterMetrics(registry)
	certWatcher, err := whhandler.NewCertExpiry(cfg.TLSCertFile, cfg.TLSKeyFile, log, registry)
	if err != nil {
		log.Error(err, "Unable to register cert expiry gauge; continuing without it")
	} else {
		if err := certWatcher.Refresh(); err != nil {
			// No keypair loaded means the listener could not serve TLS at all.
			return fmt.Errorf("initial TLS cert load: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/mutate", handler)
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Order matters: request-ID outermost so telemetry/recovery log a populated
	// requestId, and telemetry wraps recovery so a recovered request is still
	// observed. DefaultRouteLabeler keeps the metric labels to registered
	// patterns — the raw path is attacker-controlled cardinality.
	wrapped := httpx.WithRequestID(httpx.WithTelemetry(
		httpx.WithRecovery(
			mux,
			log,
			func(path string) { whhandler.PanicTotal.WithLabelValues(path).Inc() },
			httpx.DefaultRouteLabeler,
		),
		log,
		func(path, status string, dur time.Duration) {
			whhandler.RequestDuration.WithLabelValues(path, status).Observe(dur.Seconds())
		},
		httpx.DefaultRouteLabeler,
	))

	addr := fmt.Sprintf(":%d", cfg.Port)
	// The shared httpx timeouts are wider than the webhook needs; its effective
	// deadline is enforced upstream by the MutatingWebhookConfiguration timeout.
	srv := httpx.NewServer(addr, wrapped)
	if certWatcher != nil {
		// GetCertificate is consulted per handshake, so cert-manager rotations
		// land at the next Refresh tick without a restart.
		srv.TLSConfig = &tls.Config{
			GetCertificate: certWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	log.Info("Starting webhook server", "version", version.Version, "addr", addr, "certFile", cfg.TLSCertFile)

	// depsCtx, not ctx: a watcher stopped at SIGTERM would leave handshakes
	// during the drain without a certificate.
	if certWatcher != nil {
		go certWatcher.Run(depsCtx, time.Hour)
	}

	err = httpx.ListenAndServeWithShutdown(ctx, srv, log, "webhook", 10*time.Second, func() error {
		// Empty paths mean the keypair comes from TLSConfig.GetCertificate;
		// fall back to disk when the cert watcher failed to initialize.
		certPath, keyPath := "", ""
		if certWatcher == nil {
			certPath, keyPath = cfg.TLSCertFile, cfg.TLSKeyFile
		}
		return srv.ListenAndServeTLS(certPath, keyPath)
	})

	// The HTTP drain ends the requests, not the goroutines they started: a stub
	// create (internal/webhook/stub.go) outlives its AdmissionResponse and may
	// still be reading through the informer cache. Drain the handler first, then
	// stop the cache and cert watcher. ctx is already cancelled here (that is
	// what ended the serve), so the drain needs a context of its own. An
	// abandoned stub write is not lost — the next admission requests it again.
	if drainErr := handler.Shutdown(context.Background()); drainErr != nil {
		log.Info("Gave up waiting for in-flight recommendation-stub writes", "err", drainErr)
	}
	stopDeps()
	return err
}
