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
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// The bind* helpers register a pflag and bind it to a Viper key in one call.
// The key may differ from the flag name (e.g. flag "tls-cert-file" binds to key
// "webhook.tls-cert-file").

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

// bindStringSlice hardcodes a nil default: every caller wants "unset means
// empty list", and unparam flags a def parameter that is never anything else.
func bindStringSlice(flags *pflag.FlagSet, key, flagName, usage string) {
	flags.StringSlice(flagName, nil, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

// bindStringArray is bindStringSlice without CSV splitting: one element per
// occurrence, verbatim, so a value may contain a comma. Use it for values whose
// grammar the caller does not control (HTTP header values).
func bindStringArray(flags *pflag.FlagSet, key, flagName, usage string) {
	flags.StringArray(flagName, nil, usage)
	_ = viper.BindPFlag(key, flags.Lookup(flagName))
}

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

// getStringSlice splits comma-separated env values manually: viper hands env
// values back as one raw string and GetStringSlice splits on whitespace, so
// K8SSUSTAIN_EXCLUDED_NAMESPACES=kube-system,monitoring would surface as a
// single bogus element. Flag- and file-backed values already arrive as slices.
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

// bindPrometheusTransportFlags registers the Prometheus auth and TLS flags
// under keyPrefix ("" for the controller, "dashboard." for the dashboard).
//
// The two subcommands need identical flags under DIFFERENT viper keys:
// BindPFlag maps one key to exactly one pflag, so a shared flat key would leave
// whichever subcommand registered last owning it and the other reading an unset
// flagset. One registration function keeps the two sets from drifting.
func bindPrometheusTransportFlags(flags *pflag.FlagSet, keyPrefix string) {
	bindString(flags, keyPrefix+"prometheus-bearer-token", "prometheus-bearer-token", "",
		"Static bearer token sent as `Authorization: Bearer <token>` on every Prometheus request. Mutually exclusive with --prometheus-bearer-token-file; prefer the file form for Kubernetes service-account tokens, which rotate.")
	bindString(flags, keyPrefix+"prometheus-bearer-token-file", "prometheus-bearer-token-file", "",
		"Path to a file holding the Prometheus bearer token. Re-read on every request, so projected service-account tokens keep working across rotation.")
	bindString(flags, keyPrefix+"prometheus-basic-auth-username", "prometheus-basic-auth-username", "",
		"Username for HTTP basic auth against Prometheus. Required whenever a password or password file is set.")
	bindString(flags, keyPrefix+"prometheus-basic-auth-password", "prometheus-basic-auth-password", "",
		"Password for HTTP basic auth against Prometheus. Mutually exclusive with --prometheus-basic-auth-password-file.")
	bindString(flags, keyPrefix+"prometheus-basic-auth-password-file", "prometheus-basic-auth-password-file", "",
		"Path to a file holding the Prometheus basic-auth password. Re-read on every request.")
	bindStringArray(flags, keyPrefix+"prometheus-headers", "prometheus-headers",
		"Extra HTTP header sent on every Prometheus request, as Key=Value (e.g. X-Scope-OrgID=tenant-a for a multi-tenant Thanos/Mimir/Cortex gateway). Repeat the flag for several headers; a value may contain commas. The K8SSUSTAIN_PROMETHEUS_HEADERS env var form is comma-separated instead, so values with commas must use the flag.")
	bindString(flags, keyPrefix+"prometheus-tls-ca-file", "prometheus-tls-ca-file", "",
		"PEM CA bundle used to verify the Prometheus server certificate. Appended to the system trust store rather than replacing it.")
	bindString(flags, keyPrefix+"prometheus-tls-cert-file", "prometheus-tls-cert-file", "",
		"Client certificate for mutual TLS to Prometheus. Must be set together with --prometheus-tls-key-file.")
	bindString(flags, keyPrefix+"prometheus-tls-key-file", "prometheus-tls-key-file", "",
		"Client private key for mutual TLS to Prometheus. Must be set together with --prometheus-tls-cert-file.")
	bindString(flags, keyPrefix+"prometheus-tls-server-name", "prometheus-tls-server-name", "",
		"Overrides the SNI / certificate hostname verified against the Prometheus server certificate.")
	bindBool(flags, keyPrefix+"prometheus-tls-insecure-skip-verify", "prometheus-tls-insecure-skip-verify", false,
		"Disable verification of the Prometheus server certificate. Insecure: the connection can be intercepted. Logs a warning at startup.")
}

// parseHeaders turns a `Key=Value` list into a header map. Only the FIRST `=`
// splits, so header values may legitimately contain `=` (base64 tenant ids,
// for instance).
func parseHeaders(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("invalid prometheus header %q: expected Key=Value", entry)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("invalid prometheus header %q: empty header name", entry)
		}
		out[name] = strings.TrimSpace(value)
	}
	return out, nil
}

// loadPrometheusTransport reads the Prometheus transport keys under keyPrefix.
// Only the --prometheus-headers grammar can fail, and it is returned as an
// error so no call site can start the process with headers silently missing —
// i.e. querying the wrong tenant.
func loadPrometheusTransport(keyPrefix string) (promclient.TransportConfig, error) {
	headers, err := parseHeaders(getStringSlice(keyPrefix + "prometheus-headers"))
	if err != nil {
		return promclient.TransportConfig{}, err
	}
	return promclient.TransportConfig{
		BearerToken:           viper.GetString(keyPrefix + "prometheus-bearer-token"),
		BearerTokenFile:       viper.GetString(keyPrefix + "prometheus-bearer-token-file"),
		BasicAuthUsername:     viper.GetString(keyPrefix + "prometheus-basic-auth-username"),
		BasicAuthPassword:     viper.GetString(keyPrefix + "prometheus-basic-auth-password"),
		BasicAuthPasswordFile: viper.GetString(keyPrefix + "prometheus-basic-auth-password-file"),
		Headers:               headers,
		TLS: promclient.TLSConfig{
			CAFile:             viper.GetString(keyPrefix + "prometheus-tls-ca-file"),
			CertFile:           viper.GetString(keyPrefix + "prometheus-tls-cert-file"),
			KeyFile:            viper.GetString(keyPrefix + "prometheus-tls-key-file"),
			ServerName:         viper.GetString(keyPrefix + "prometheus-tls-server-name"),
			InsecureSkipVerify: viper.GetBool(keyPrefix + "prometheus-tls-insecure-skip-verify"),
		},
	}, nil
}

// DefaultQueryShardMaxSamples is the default --query-shard-max-samples value:
// the projected Prometheus sample budget BuildShards packs a batched query
// against. Prometheus's own --query.max-samples (default 50_000_000) REJECTS an
// over-budget query outright, failing every workload in the shard, so this
// leaves a 5x margin. Exported so internal/prometheus/shardscale_test.go can
// assert against the shipped value rather than a second literal.
const DefaultQueryShardMaxSamples = 10_000_000

// DefaultRecommendationRetention is shared by the controller and webhook
// bindings on purpose: the controller decides how long a departed
// WorkloadRecommendation is kept and the webhook refuses to inject from one
// older than that, so two literals could drift into a window where the webhook
// serves what the controller considers expired.
const DefaultRecommendationRetention = 168 * time.Hour

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
	bindStringSlice(flags, "excluded-namespaces", "excluded-namespaces", "Namespaces the reconciler should never touch")
	bindInt(flags, "workload-concurrency-limit", "workload-concurrency-limit", 5, "Maximum number of workloads processed in parallel per reconcile cycle")
	bindInt(flags, "policy-concurrency-limit", "policy-concurrency-limit", 10, "Maximum number of Policy objects reconciled in parallel")
	bindInt(flags, "prometheus-max-inflight", "prometheus-max-inflight", 8,
		"Maximum concurrent Prometheus queries across the whole controller. Kept below Prometheus's own --query.max-concurrency (default 20) so k8s-sustain does not starve dashboards and alerting")
	bindDuration(flags, "recycle-replacement-timeout", "recycle-replacement-timeout", 5*time.Minute,
		"In the eviction-fallback recycle path, how long to wait for a replacement pod to become Ready before aborting the loop. Increase on clusters where node autoscaling (Karpenter / cluster-autoscaler) takes several minutes.")
	bindDuration(flags, "recommendation-retention", "recommendation-retention", DefaultRecommendationRetention,
		"How long a WorkloadRecommendation is kept after its workload object disappears (ephemeral bare pods, deleted or terminal Jobs). Also decides whether a RECURRING ephemeral identity is rightsized at admission on its next run: the webhook's only recommendation source is this object, so an identity whose gap between runs exceeds this window cold-starts every time. Set it above the longest expected inter-run gap (the 7d default covers weekly batch). The dashboard shows retained entries as inactive workloads. 0 sweeps them on the next reconcile.")
	bindInt(flags, "query-shard-max-samples", "query-shard-max-samples", DefaultQueryShardMaxSamples,
		"Projected Prometheus sample budget (containers x window-minutes, summed across a shard's workloads) a single batched CPU/memory/OOM shard query is allowed to reach before a new shard is started. Keep this under Prometheus's own --query.max-samples (default 50,000,000): that server-side limit REJECTS an over-budget query outright, failing every workload sharing the shard, not just the excess ones. The default here leaves a 5x margin.")
	bindPrometheusTransportFlags(flags, "")
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
	WorkloadConcurrencyLimit  int
	PolicyConcurrencyLimit    int
	PrometheusMaxInflight     int
	RecommendOnly             bool
	RecycleReplacementTimeout time.Duration
	RecommendationRetention   time.Duration
	QueryShardMaxSamples      int
	// PrometheusTransport is the auth/TLS config handed to promclient.New.
	PrometheusTransport promclient.TransportConfig
}

// LoadControllerConfig reads the current Viper state and returns a
// ControllerConfig. It fails only on a malformed --prometheus-headers entry.
func LoadControllerConfig() (ControllerConfig, error) {
	transport, err := loadPrometheusTransport("")
	if err != nil {
		return ControllerConfig{}, fmt.Errorf("invalid --prometheus-headers: %w", err)
	}
	return ControllerConfig{
		MetricsBindAddress:        viper.GetString("metrics-bind-address"),
		HealthProbeBindAddress:    viper.GetString("health-probe-bind-address"),
		LeaderElect:               viper.GetBool("leader-elect"),
		LeaderElectionID:          viper.GetString("leader-election-id"),
		LogLevel:                  viper.GetString("log-level"),
		PrometheusAddress:         viper.GetString("prometheus-address"),
		ReconcileInterval:         viper.GetDuration("reconcile-interval"),
		ExcludedNamespaces:        getStringSlice("excluded-namespaces"),
		WorkloadConcurrencyLimit:  viper.GetInt("workload-concurrency-limit"),
		PolicyConcurrencyLimit:    viper.GetInt("policy-concurrency-limit"),
		PrometheusMaxInflight:     viper.GetInt("prometheus-max-inflight"),
		RecommendOnly:             RecommendOnly(),
		RecycleReplacementTimeout: viper.GetDuration("recycle-replacement-timeout"),
		RecommendationRetention:   viper.GetDuration("recommendation-retention"),
		QueryShardMaxSamples:      viper.GetInt("query-shard-max-samples"),
		PrometheusTransport:       transport,
	}, nil
}

// BindWebhookFlags registers flags for the "webhook" subcommand.
func BindWebhookFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	bindString(flags, "webhook.tls-cert-file", "tls-cert-file", "/tls/tls.crt", "Path to TLS certificate file")
	bindString(flags, "webhook.tls-key-file", "tls-key-file", "/tls/tls.key", "Path to TLS private key file")
	bindInt(flags, "webhook.port", "port", 9443, "Port the webhook server listens on")
	bindString(flags, "webhook.log-level", "log-level", "info", "Log level (debug, info, warn, error)")
	bindStringSlice(flags, "webhook.excluded-namespaces", "excluded-namespaces", "Namespaces the webhook should never mutate (mirrors the controller flag)")
	// Prefixed key despite the shared flag name: BindPFlag maps a key to exactly
	// one pflag, so a flat key would leave the webhook reading the controller's
	// unset flagset. The DEFAULT is shared, which is what must not drift.
	bindDuration(flags, "webhook.recommendation-retention", "recommendation-retention", DefaultRecommendationRetention,
		"Must match the controller's --recommendation-retention. It bounds the one case where the webhook injects from a WorkloadRecommendation older than the staleness budget: an identity the controller marked departed, whose ObservedAt is frozen by design. Past this window the object is one the controller's sweep should already have deleted, so the webhook treats it as stale instead of injecting it forever. The chart renders both flags from the single controller.recommendationRetention value.")
}

// WebhookConfig holds resolved configuration for the webhook server.
type WebhookConfig struct {
	TLSCertFile        string
	TLSKeyFile         string
	Port               int
	LogLevel           string
	RecommendOnly      bool
	ExcludedNamespaces []string
	// RecommendationRetention mirrors the controller flag of the same name;
	// see BindWebhookFlags for why the webhook needs it.
	RecommendationRetention time.Duration
}

// LoadWebhookConfig reads the current Viper state and returns a WebhookConfig.
// viper.UnmarshalKey would be tidier but does not see BindPFlag-bound nested
// keys (viper.Sub("webhook") returns nil), hence the explicit Get calls.
func LoadWebhookConfig() WebhookConfig {
	return WebhookConfig{
		TLSCertFile:        viper.GetString("webhook.tls-cert-file"),
		TLSKeyFile:         viper.GetString("webhook.tls-key-file"),
		Port:               viper.GetInt("webhook.port"),
		LogLevel:           viper.GetString("webhook.log-level"),
		RecommendOnly:      RecommendOnly(),
		ExcludedNamespaces: getStringSlice("webhook.excluded-namespaces"),

		RecommendationRetention: viper.GetDuration("webhook.recommendation-retention"),
	}
}

// BindDashboardFlags registers flags for the "dashboard" subcommand.
func BindDashboardFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	bindString(flags, "dashboard.bind-address", "bind-address", ":8090", "Address the dashboard server listens on")
	bindString(flags, "dashboard.prometheus-address", "prometheus-address", "http://localhost:9090", "Prometheus server address")
	bindString(flags, "dashboard.log-level", "log-level", "info", "Log level (debug, info, warn, error)")
	bindStringSlice(flags, "dashboard.cors-allowed-origins", "cors-allowed-origins", "Allowed CORS origins (e.g. http://localhost:3000). Empty (default) means same-origin only. Use * to allow all.")
	bindStringSlice(flags, "dashboard.excluded-namespaces", "excluded-namespaces", "Namespaces the controller/webhook never manage (mirrors their --excluded-namespaces flag). The dashboard uses this to keep its policy-scoped workload views consistent with what the controller and webhook actually manage.")
	bindPrometheusTransportFlags(flags, "dashboard.")
}

// DashboardConfig holds resolved configuration for the dashboard server.
type DashboardConfig struct {
	BindAddress        string
	PrometheusAddress  string
	LogLevel           string
	CORSAllowedOrigins []string
	ExcludedNamespaces []string
	// PrometheusTransport is the auth/TLS config handed to promclient.New.
	PrometheusTransport promclient.TransportConfig
}

// LoadDashboardConfig reads the current Viper state and returns a
// DashboardConfig. It fails only on a malformed --prometheus-headers entry.
// See LoadWebhookConfig for why we avoid viper.UnmarshalKey here.
func LoadDashboardConfig() (DashboardConfig, error) {
	transport, err := loadPrometheusTransport("dashboard.")
	if err != nil {
		return DashboardConfig{}, fmt.Errorf("invalid --prometheus-headers: %w", err)
	}
	return DashboardConfig{
		BindAddress:         viper.GetString("dashboard.bind-address"),
		PrometheusAddress:   viper.GetString("dashboard.prometheus-address"),
		LogLevel:            viper.GetString("dashboard.log-level"),
		CORSAllowedOrigins:  getStringSlice("dashboard.cors-allowed-origins"),
		ExcludedNamespaces:  getStringSlice("dashboard.excluded-namespaces"),
		PrometheusTransport: transport,
	}, nil
}
