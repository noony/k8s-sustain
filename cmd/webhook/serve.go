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

// newCachedClient is the seam cmd/webhook builds its Kubernetes client
// through. Production is k8sclient.NewCached; tests replace it, because the
// real one reaches for a kubeconfig via ctrl.GetConfigOrDie (which exits the
// process when there is none) and then opens watches against an apiserver.
var newCachedClient = k8sclient.NewCached

func runWebhook(_ *cobra.Command, _ []string) error {
	cfg := config.LoadWebhookConfig()
	log := logging.Setup(cfg.LogLevel, "webhook")

	// One signal-handler context for the whole process. It is the ONLY signal
	// source: httpx.ListenAndServeWithShutdown takes this ctx rather than
	// registering a handler of its own, so shutdown ordering in serve() is an
	// explicit choice rather than a race between two handlers.
	//
	// ctrl.SetupSignalHandler panics if called a second time in the same
	// process, so this must be the only call site -- the "start" subcommand
	// (cmd/controller/start.go) has its own, but that only runs when the
	// process is invoked as "k8s-sustain start", never in the same run as
	// "k8s-sustain webhook".
	//
	// It is also, from the moment it is installed, the process's ONLY response
	// to SIGTERM: SetupSignalHandler replaces Go's default "terminate" with
	// "cancel this context". Everything that can block from here on has to
	// honour it, or the signal is silently swallowed for as long as that block
	// lasts -- see serve().
	return serve(ctrl.SetupSignalHandler(), cfg, log)
}

// serve runs the webhook server until ctx is cancelled (SIGTERM/SIGINT) or the
// listener fails. Split out from runWebhook so tests can drive it with a
// context they control instead of a real signal.
func serve(ctx context.Context, cfg config.WebhookConfig, log logr.Logger) error {
	// The informer cache and cert watcher run on a SEPARATE context that
	// outlives ctx on purpose. They are cancelled only after the HTTP server
	// has finished draining (see the deferred stopDeps and the ordering at the
	// end of this function).
	//
	// Deriving them from ctx instead would stop the cache the instant SIGTERM
	// arrives, while the server is still serving admissions for up to its
	// shutdown timeout — those requests would read a store that had stopped
	// receiving updates. Serving is the thing that must stop first; everything
	// admission depends on has to still be there while it does.
	depsCtx, stopDeps := context.WithCancel(context.Background())
	defer stopDeps()

	// Cached rather than direct-to-apiserver: with Prometheus out of the
	// admission path (see internal/webhook/recommendations.go), the
	// apiserver is the only remaining source of per-pod latency, and admit()
	// does a Get for the Policy, the owner chain, and the
	// WorkloadRecommendation on every single Pod CREATE. At thousands of
	// pods that is a per-pod apiserver round trip the cluster does not need.
	// See k8sclient.NewCached's doc comment for the startup-ordering and
	// memory-cost tradeoffs this brings.
	//
	// TWO contexts, and the second one is not decoration. This call BLOCKS —
	// up to crdWaitTimeout (2m) waiting for the CRDs to be servable, plus the
	// informer sync — and the HTTPS listener does not exist yet, so nothing
	// answers /healthz and nothing is watching for a shutdown signal. depsCtx
	// deliberately does not track ctx (see above), so handing it the startup
	// phase as well would mean the process ignores SIGTERM for that entire
	// window and is removed only by the SIGKILL at the end of
	// terminationGracePeriodSeconds — a `kubectl rollout restart` on a cluster
	// where the CRDs are absent (installCRDs=false) hangs for the full grace
	// period. So: depsCtx for the cache's lifetime, ctx for the wait.
	k8sClient, err := newCachedClient(depsCtx, ctx, config.Scheme())
	if err != nil {
		// A shutdown signal that arrived mid-startup is not a failure. Exiting
		// non-zero here would have the container restart-loop on what is simply
		// a rollout: nothing has been served, so there is nothing to report.
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Info("Shutdown signal received during startup; exiting before the server started")
			return nil
		}
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
		RecommendOnly:      cfg.RecommendOnly,
		ExcludedNamespaces: cfg.ExcludedNamespaces,
		// Bounds the departed-identity path, which waives the staleness gate.
		// Must be the controller's own retention window — the chart renders both
		// flags from one value; see config.BindWebhookFlags.
		RecommendationRetention: cfg.RecommendationRetention,
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
	// matching what the dashboard exposes. Order matters — request-ID sits
	// outermost so telemetry/recovery log a populated requestId, and
	// telemetry wraps recovery so a recovered request is still observed.
	//
	// Route labels are derived via DefaultRouteLabeler so the histogram and
	// panic counter only ever see the registered patterns (/mutate, /metrics,
	// /healthz). Without that, an attacker hitting bogus URLs would blow up
	// Prometheus label cardinality on the webhook the same way it would on
	// the dashboard.
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
	// Hardened timeouts come from httpx.NewServer's shared defaults
	// (ReadHeaderTimeout 5s, Read/WriteTimeout 15s, IdleTimeout 60s). This
	// widens the webhook's old 10s Read/WriteTimeout to the shared 15s; the
	// webhook's effective deadline is still enforced upstream by the
	// apiserver's MutatingWebhookConfiguration timeout, so this is safe.
	srv := httpx.NewServer(addr, wrapped)
	if certWatcher != nil {
		// Hot-reload path: GetCertificate is consulted on every TLS handshake,
		// so cert-manager rotations are picked up at the next Refresh tick
		// without a process restart.
		srv.TLSConfig = &tls.Config{
			GetCertificate: certWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	log.Info("Starting webhook server", "version", version.Version, "addr", addr, "certFile", cfg.TLSCertFile)

	// Shares depsCtx with the informer cache: the cert watcher supplies the
	// keypair for every TLS handshake, so it too must outlive the drain — a
	// watcher stopped at SIGTERM would leave in-flight handshakes during
	// shutdown without a certificate.
	if certWatcher != nil {
		go certWatcher.Run(depsCtx, time.Hour)
	}

	err = httpx.ListenAndServeWithShutdown(ctx, srv, log, "webhook", 10*time.Second, func() error {
		// Empty cert/key paths: the keypair comes from TLSConfig.GetCertificate.
		// Falls back to disk-loading paths when GetCertificate isn't wired
		// (e.g. cert watcher init failed).
		certPath, keyPath := "", ""
		if certWatcher == nil {
			certPath, keyPath = cfg.TLSCertFile, cfg.TLSKeyFile
		}
		return srv.ListenAndServeTLS(certPath, keyPath)
	})

	// Only now, with the drain complete and no request able to arrive, is it
	// safe to stop the informer cache and cert watcher. The deferred stopDeps
	// covers the early-return paths above; this call makes the ordering
	// explicit on the path that actually serves traffic.
	stopDeps()
	return err
}
