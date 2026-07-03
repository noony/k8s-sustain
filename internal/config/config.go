// Package config centralizes all Viper-backed configuration for the k8s-sustain
// operator. Each subcommand has a Bind*Flags function (called from init()) and a
// corresponding typed struct returned by a Load* function (called at runtime).
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// --- Flag registration helpers ---------------------------------------------
//
// Each helper registers a pflag on the given flagset under flagName and binds
// it to the given Viper key in a single call, so the flag-name string is
// written once instead of being repeated (and hand-aligned) across a separate
// pflag declaration and BindPFlag call. The Viper key may differ from the flag
// name (e.g. the "tls-cert-file" flag binds to the "webhook.tls-cert-file"
// key).

func bindString(flags *pflag.FlagSet, key, flagName, def, usage string) {
	flags.String(flagName, def, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

func bindBool(flags *pflag.FlagSet, key, flagName string, def bool, usage string) {
	flags.Bool(flagName, def, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

func bindInt(flags *pflag.FlagSet, key, flagName string, def int, usage string) {
	flags.Int(flagName, def, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

func bindDuration(flags *pflag.FlagSet, key, flagName string, def time.Duration, usage string) {
	flags.Duration(flagName, def, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

func bindStringSlice(flags *pflag.FlagSet, key, flagName string, def []string, usage string) {
	flags.StringSlice(flagName, def, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

// --- Global (persistent) flags, shared by every subcommand -----------------

// BindGlobalFlags registers global persistent flags on the root command.
func BindGlobalFlags(root *cobra.Command) {
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: $HOME/.k8s-sustain.yaml)")
	bindBool(root.PersistentFlags(), "recommend-only", "recommend-only", false,
		"Compute recommendations but never patch workloads or mutate pods (dry-run mode)")
}

var cfgFile string

// InitViper sets up Viper's config-file search paths, env prefix, and reads
// the config file if present. Must be passed to cobra.OnInitialize().
func InitViper() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	}

	viper.SetEnvPrefix("K8SSUSTAIN")
	// Map `.` (viper subkey separator) and `-` (kebab-case flag names) to `_`
	// so env vars like K8SSUSTAIN_DASHBOARD_BIND_ADDRESS bind to keys such as
	// `dashboard.bind-address`.
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
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

// getStringSlice reads a string-slice key, accepting comma-separated values
// from environment variables. Viper hands env values back as a single raw
// string, and GetStringSlice splits strings on whitespace — so
// K8SSUSTAIN_EXCLUDED_NAMESPACES=kube-system,monitoring would surface as the
// single bogus element "kube-system,monitoring". Detect the raw-string case
// and split on commas (trimming spaces) so env overrides behave exactly like
// --flag=a,b. Flag- and config-file-backed values arrive as slices and pass
// through to GetStringSlice unchanged.
func getStringSlice(key string) []string {
	raw, ok := viper.Get(key).(string)
	if !ok {
		return viper.GetStringSlice(key)
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// --- Controller (start) flags ------------------------------------------------

// BindControllerFlags registers flags for the "start" subcommand.
func BindControllerFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	bindString(flags, "metrics-bind-address", "metrics-bind-address", ":8080", "Address the metrics endpoint binds to")
	bindString(flags, "health-probe-bind-address", "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to")
	bindBool(flags, "leader-elect", "leader-elect", false, "Enable leader election for high availability")
	bindString(flags, "leader-election-id", "leader-election-id", "k8s-sustain-leader-election", "Lease name for leader election. Override when running multiple operator installs in the same cluster.")
	bindString(flags, "log-level", "log-level", "info", "Log level (debug, info, warn, error)")
	bindString(flags, "prometheus-address", "prometheus-address", "http://localhost:9090", "Address of the Prometheus server used for metric queries")
	bindDuration(flags, "reconcile-interval", "reconcile-interval", 5*time.Minute, "How often policies are re-evaluated")
	bindStringSlice(flags, "excluded-namespaces", "excluded-namespaces", nil, "Namespaces the reconciler should never touch")
	bindInt(flags, "concurrency-limit", "concurrency-limit", 5, "Maximum number of workloads processed in parallel per reconcile cycle")
	bindDuration(flags, "recycle-replacement-timeout", "recycle-replacement-timeout", 5*time.Minute,
		"In the eviction-fallback recycle path, how long to wait for a replacement pod to become Ready before aborting the loop. Increase on clusters where node autoscaling (Karpenter / cluster-autoscaler) takes several minutes.")
	bindDuration(flags, "recommendation-retention", "recommendation-retention", 72*time.Hour,
		"How long a WorkloadRecommendation is kept after its workload object disappears (ephemeral bare pods, deleted or terminal Jobs). The dashboard shows these as inactive workloads. 0 sweeps them on the next reconcile.")
}

// ControllerConfig holds resolved configuration for the controller.
type ControllerConfig struct {
	MetricsBindAddress        string
	HealthProbeBindAddress    string
	LeaderElect               bool
	LeaderElectionID          string
	LogLevel                  string
	PrometheusAddress         string
	ReconcileInterval         time.Duration
	ExcludedNamespaces        []string
	ConcurrencyLimit          int
	RecommendOnly             bool
	RecycleReplacementTimeout time.Duration
	RecommendationRetention   time.Duration
}

// LoadControllerConfig reads the current Viper state and returns a ControllerConfig.
func LoadControllerConfig() ControllerConfig {
	return ControllerConfig{
		MetricsBindAddress:        viper.GetString("metrics-bind-address"),
		HealthProbeBindAddress:    viper.GetString("health-probe-bind-address"),
		LeaderElect:               viper.GetBool("leader-elect"),
		LeaderElectionID:          viper.GetString("leader-election-id"),
		LogLevel:                  viper.GetString("log-level"),
		PrometheusAddress:         viper.GetString("prometheus-address"),
		ReconcileInterval:         viper.GetDuration("reconcile-interval"),
		ExcludedNamespaces:        getStringSlice("excluded-namespaces"),
		ConcurrencyLimit:          viper.GetInt("concurrency-limit"),
		RecommendOnly:             RecommendOnly(),
		RecycleReplacementTimeout: viper.GetDuration("recycle-replacement-timeout"),
		RecommendationRetention:   viper.GetDuration("recommendation-retention"),
	}
}

// --- Webhook flags ---------------------------------------------------------

// BindWebhookFlags registers flags for the "webhook" subcommand.
func BindWebhookFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	bindString(flags, "webhook.tls-cert-file", "tls-cert-file", "/tls/tls.crt", "Path to TLS certificate file")
	bindString(flags, "webhook.tls-key-file", "tls-key-file", "/tls/tls.key", "Path to TLS private key file")
	bindInt(flags, "webhook.port", "port", 9443, "Port the webhook server listens on")
	bindString(flags, "webhook.prometheus-address", "prometheus-address", "http://localhost:9090", "Prometheus server address")
	bindString(flags, "webhook.log-level", "log-level", "info", "Log level (debug, info, warn, error)")
	bindStringSlice(flags, "webhook.excluded-namespaces", "excluded-namespaces", nil, "Namespaces the webhook should never mutate (mirrors the controller flag)")
}

// WebhookConfig holds resolved configuration for the webhook server.
type WebhookConfig struct {
	TLSCertFile        string
	TLSKeyFile         string
	Port               int
	PrometheusAddress  string
	LogLevel           string
	RecommendOnly      bool
	ExcludedNamespaces []string
}

// LoadWebhookConfig reads the current Viper state and returns a WebhookConfig.
// viper.UnmarshalKey would be tidier but does not see BindPFlag-bound nested
// keys (viper.Sub("webhook") returns nil even though AllSettings exposes the
// subtree). Explicit Get calls are uglier but reliable.
func LoadWebhookConfig() WebhookConfig {
	return WebhookConfig{
		TLSCertFile:        viper.GetString("webhook.tls-cert-file"),
		TLSKeyFile:         viper.GetString("webhook.tls-key-file"),
		Port:               viper.GetInt("webhook.port"),
		PrometheusAddress:  viper.GetString("webhook.prometheus-address"),
		LogLevel:           viper.GetString("webhook.log-level"),
		RecommendOnly:      RecommendOnly(),
		ExcludedNamespaces: getStringSlice("webhook.excluded-namespaces"),
	}
}

// --- Dashboard flags -------------------------------------------------------

// BindDashboardFlags registers flags for the "dashboard" subcommand.
func BindDashboardFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	bindString(flags, "dashboard.bind-address", "bind-address", ":8090", "Address the dashboard server listens on")
	bindString(flags, "dashboard.prometheus-address", "prometheus-address", "http://localhost:9090", "Prometheus server address")
	bindString(flags, "dashboard.log-level", "log-level", "info", "Log level (debug, info, warn, error)")
	bindStringSlice(flags, "dashboard.cors-allowed-origins", "cors-allowed-origins", nil, "Allowed CORS origins (e.g. http://localhost:3000). Empty (default) means same-origin only. Use * to allow all.")
}

// DashboardConfig holds resolved configuration for the dashboard server.
type DashboardConfig struct {
	BindAddress        string
	PrometheusAddress  string
	LogLevel           string
	CORSAllowedOrigins []string
}

// LoadDashboardConfig reads the current Viper state and returns a DashboardConfig.
// See LoadWebhookConfig for why we avoid viper.UnmarshalKey here.
func LoadDashboardConfig() DashboardConfig {
	return DashboardConfig{
		BindAddress:        viper.GetString("dashboard.bind-address"),
		PrometheusAddress:  viper.GetString("dashboard.prometheus-address"),
		LogLevel:           viper.GetString("dashboard.log-level"),
		CORSAllowedOrigins: getStringSlice("dashboard.cors-allowed-origins"),
	}
}
