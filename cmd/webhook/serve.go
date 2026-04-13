// Package webhook registers the "webhook" subcommand with the root cobra command.
// It starts a TLS HTTPS server that handles mutating admission requests for Pods,
// injecting resource recommendations from policies with OnCreate update mode.
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/cmd/manager"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	whhandler "github.com/noony/k8s-sustain/internal/webhook"
)

var webhookScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(webhookScheme))
	utilruntime.Must(sustainv1alpha1.AddToScheme(webhookScheme))

	serveCmd.Flags().String("tls-cert-file", "/tls/tls.crt", "Path to TLS certificate file")
	serveCmd.Flags().String("tls-key-file", "/tls/tls.key", "Path to TLS private key file")
	serveCmd.Flags().Int("port", 9443, "Port the webhook server listens on")
	serveCmd.Flags().String("prometheus-address", "http://localhost:9090", "Prometheus server address")
	serveCmd.Flags().String("zap-log-level", "info", "Log level (debug, info, warn, error)")
	serveCmd.Flags().String("health-probe-bind-address", ":8082", "Address the health probe endpoint binds to")

	_ = viper.BindPFlag("webhook.tls-cert-file", serveCmd.Flags().Lookup("tls-cert-file"))
	_ = viper.BindPFlag("webhook.tls-key-file", serveCmd.Flags().Lookup("tls-key-file"))
	_ = viper.BindPFlag("webhook.port", serveCmd.Flags().Lookup("port"))
	_ = viper.BindPFlag("webhook.prometheus-address", serveCmd.Flags().Lookup("prometheus-address"))
	_ = viper.BindPFlag("webhook.zap-log-level", serveCmd.Flags().Lookup("zap-log-level"))
	_ = viper.BindPFlag("webhook.health-probe-bind-address", serveCmd.Flags().Lookup("health-probe-bind-address"))

	manager.RootCmd().AddCommand(serveCmd)
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
	logLevel := zapcore.InfoLevel
	if err := logLevel.UnmarshalText([]byte(viper.GetString("webhook.zap-log-level"))); err != nil {
		logLevel = zapcore.InfoLevel
	}

	logger := ctrlzap.New(ctrlzap.UseDevMode(logLevel == zapcore.DebugLevel), ctrlzap.Level(logLevel))
	ctrl.SetLogger(logger)
	log := ctrl.Log.WithName("webhook")

	promClient, err := promclient.New(viper.GetString("webhook.prometheus-address"))
	if err != nil {
		log.Error(err, "Unable to create Prometheus client")
		return err
	}

	cfg := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(cfg, client.Options{Scheme: webhookScheme})
	if err != nil {
		log.Error(err, "Unable to create Kubernetes client")
		return err
	}

	handler := &whhandler.Handler{
		Client:           k8sClient,
		PrometheusClient: promClient,
	}

	mux := http.NewServeMux()
	mux.Handle("/mutate", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := fmt.Sprintf(":%d", viper.GetInt("webhook.port"))
	certFile := viper.GetString("webhook.tls-cert-file")
	keyFile := viper.GetString("webhook.tls-key-file")

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("Starting webhook server", "addr", addr, "certFile", certFile)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		log.Info("Shutting down webhook server")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}
