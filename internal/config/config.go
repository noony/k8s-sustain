// Package config centralizes all Viper-backed configuration for the k8s-sustain
// operator. Each subcommand has a Bind*Flags function (called from init()) and a
// corresponding typed struct returned by a Load* function (called at runtime).
package config

import (
	"fmt"
	"os"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// --- Global (persistent) flags, shared by every subcommand -----------------

// BindGlobalFlags registers global persistent flags on the root command.
func BindGlobalFlags(root *cobra.Command) {
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: $HOME/.k8s-sustain.yaml)")
	root.PersistentFlags().Bool("recommend-only", false,
		"Compute recommendations but never patch workloads or mutate pods (dry-run mode)")
	_ = viper.BindPFlag("recommend-only", root.PersistentFlags().Lookup("recommend-only"))
}

var cfgFile string

// InitViper sets up Viper's config-file search paths, env prefix, and reads
// the config file if present. Must be passed to cobra.OnInitialize().
func InitViper() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	}

	viper.SetEnvPrefix("K8SSUSTAIN")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}

// Scheme returns the shared runtime.Scheme with all k8s-sustain types registered.
// Safe to call from any subcommand.
func Scheme() *runtime.Scheme {
	return scheme
}

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(sustainv1alpha1.AddToScheme(s))
	utilruntime.Must(rolloutsv1alpha1.AddToScheme(s))
	return s
}()

// RecommendOnly returns true when the operator should only log recommendations
// without applying any changes to workloads or pods.
func RecommendOnly() bool {
	return viper.GetBool("recommend-only")
}

// --- Controller (start) flags ------------------------------------------------

// BindControllerFlags registers flags for the "start" subcommand.
func BindControllerFlags(cmd *cobra.Command) {
	cmd.Flags().String("metrics-bind-address", ":8080", "Address the metrics endpoint binds to")
	cmd.Flags().String("health-probe-bind-address", ":8081", "Address the health probe endpoint binds to")
	cmd.Flags().Bool("leader-elect", false, "Enable leader election for high availability")
	cmd.Flags().String("zap-log-level", "info", "Log level (debug, info, warn, error)")
	cmd.Flags().String("prometheus-address", "http://localhost:9090", "Address of the Prometheus server used for metric queries")
	cmd.Flags().Duration("reconcile-interval", 10*time.Minute, "How often policies are re-evaluated")
	cmd.Flags().StringSlice("excluded-namespaces", nil, "Namespaces the reconciler should never touch")
	cmd.Flags().Int("concurrency-limit", 5, "Maximum number of workloads processed in parallel per reconcile cycle")

	_ = viper.BindPFlag("metrics-bind-address", cmd.Flags().Lookup("metrics-bind-address"))
	_ = viper.BindPFlag("health-probe-bind-address", cmd.Flags().Lookup("health-probe-bind-address"))
	_ = viper.BindPFlag("leader-elect", cmd.Flags().Lookup("leader-elect"))
	_ = viper.BindPFlag("zap-log-level", cmd.Flags().Lookup("zap-log-level"))
	_ = viper.BindPFlag("prometheus-address", cmd.Flags().Lookup("prometheus-address"))
	_ = viper.BindPFlag("reconcile-interval", cmd.Flags().Lookup("reconcile-interval"))
	_ = viper.BindPFlag("excluded-namespaces", cmd.Flags().Lookup("excluded-namespaces"))
	_ = viper.BindPFlag("concurrency-limit", cmd.Flags().Lookup("concurrency-limit"))
}

// ControllerConfig holds resolved configuration for the controller.
type ControllerConfig struct {
	MetricsBindAddress     string
	HealthProbeBindAddress string
	LeaderElect            bool
	LogLevel               string
	PrometheusAddress      string
	ReconcileInterval      time.Duration
	ExcludedNamespaces     []string
	ConcurrencyLimit       int
	RecommendOnly          bool
}

// LoadControllerConfig reads the current Viper state and returns a ControllerConfig.
func LoadControllerConfig() ControllerConfig {
	return ControllerConfig{
		MetricsBindAddress:     viper.GetString("metrics-bind-address"),
		HealthProbeBindAddress: viper.GetString("health-probe-bind-address"),
		LeaderElect:            viper.GetBool("leader-elect"),
		LogLevel:               viper.GetString("zap-log-level"),
		PrometheusAddress:      viper.GetString("prometheus-address"),
		ReconcileInterval:      viper.GetDuration("reconcile-interval"),
		ExcludedNamespaces:     viper.GetStringSlice("excluded-namespaces"),
		ConcurrencyLimit:       viper.GetInt("concurrency-limit"),
		RecommendOnly:          RecommendOnly(),
	}
}

// --- Webhook flags ---------------------------------------------------------

// BindWebhookFlags registers flags for the "webhook" subcommand.
func BindWebhookFlags(cmd *cobra.Command) {
	cmd.Flags().String("tls-cert-file", "/tls/tls.crt", "Path to TLS certificate file")
	cmd.Flags().String("tls-key-file", "/tls/tls.key", "Path to TLS private key file")
	cmd.Flags().Int("port", 9443, "Port the webhook server listens on")
	cmd.Flags().String("prometheus-address", "http://localhost:9090", "Prometheus server address")
	cmd.Flags().String("zap-log-level", "info", "Log level (debug, info, warn, error)")
	_ = viper.BindPFlag("webhook.tls-cert-file", cmd.Flags().Lookup("tls-cert-file"))
	_ = viper.BindPFlag("webhook.tls-key-file", cmd.Flags().Lookup("tls-key-file"))
	_ = viper.BindPFlag("webhook.port", cmd.Flags().Lookup("port"))
	_ = viper.BindPFlag("webhook.prometheus-address", cmd.Flags().Lookup("prometheus-address"))
	_ = viper.BindPFlag("webhook.zap-log-level", cmd.Flags().Lookup("zap-log-level"))
}

// WebhookConfig holds resolved configuration for the webhook server.
type WebhookConfig struct {
	TLSCertFile       string
	TLSKeyFile        string
	Port              int
	PrometheusAddress string
	LogLevel          string
	RecommendOnly     bool
}

// LoadWebhookConfig reads the current Viper state and returns a WebhookConfig.
func LoadWebhookConfig() WebhookConfig {
	return WebhookConfig{
		TLSCertFile:       viper.GetString("webhook.tls-cert-file"),
		TLSKeyFile:        viper.GetString("webhook.tls-key-file"),
		Port:              viper.GetInt("webhook.port"),
		PrometheusAddress: viper.GetString("webhook.prometheus-address"),
		LogLevel:          viper.GetString("webhook.zap-log-level"),
		RecommendOnly:     RecommendOnly(),
	}
}

// --- Dashboard flags -------------------------------------------------------

// BindDashboardFlags registers flags for the "dashboard" subcommand.
func BindDashboardFlags(cmd *cobra.Command) {
	cmd.Flags().String("bind-address", ":8090", "Address the dashboard server listens on")
	cmd.Flags().String("prometheus-address", "http://localhost:9090", "Prometheus server address")
	cmd.Flags().String("zap-log-level", "info", "Log level (debug, info, warn, error)")
	cmd.Flags().StringSlice("cors-allowed-origins", []string{"*"}, "Allowed CORS origins (e.g. http://localhost:3000). Use * to allow all.")

	_ = viper.BindPFlag("dashboard.bind-address", cmd.Flags().Lookup("bind-address"))
	_ = viper.BindPFlag("dashboard.prometheus-address", cmd.Flags().Lookup("prometheus-address"))
	_ = viper.BindPFlag("dashboard.zap-log-level", cmd.Flags().Lookup("zap-log-level"))
	_ = viper.BindPFlag("dashboard.cors-allowed-origins", cmd.Flags().Lookup("cors-allowed-origins"))
}

// DashboardConfig holds resolved configuration for the dashboard server.
type DashboardConfig struct {
	BindAddress        string
	PrometheusAddress  string
	LogLevel           string
	CORSAllowedOrigins []string
}

// LoadDashboardConfig reads the current Viper state and returns a DashboardConfig.
func LoadDashboardConfig() DashboardConfig {
	return DashboardConfig{
		BindAddress:        viper.GetString("dashboard.bind-address"),
		PrometheusAddress:  viper.GetString("dashboard.prometheus-address"),
		LogLevel:           viper.GetString("dashboard.zap-log-level"),
		CORSAllowedOrigins: viper.GetStringSlice("dashboard.cors-allowed-origins"),
	}
}
