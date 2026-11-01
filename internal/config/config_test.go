package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// mustLoadController / mustLoadDashboard fail the test on a Load error, for
// the many tests that exercise something other than --prometheus-headers.
func mustLoadController(t *testing.T) ControllerConfig {
	t.Helper()
	cfg, err := LoadControllerConfig()
	if err != nil {
		t.Fatalf("LoadControllerConfig: %v", err)
	}
	return cfg
}

func mustLoadDashboard(t *testing.T) DashboardConfig {
	t.Helper()
	cfg, err := LoadDashboardConfig()
	if err != nil {
		t.Fatalf("LoadDashboardConfig: %v", err)
	}
	return cfg
}

// newRoot binds the flags the way the binary does: the global flags on the
// root, each subcommand's own on a command under it.
func newRoot(t *testing.T) *cobra.Command {
	t.Helper()
	t.Cleanup(viper.Reset)
	viper.Reset()
	root := &cobra.Command{Use: "k8s-sustain", SilenceUsage: true, SilenceErrors: true}
	BindGlobalFlags(root)
	for use, bind := range map[string]func(*cobra.Command){
		"start":     BindControllerFlags,
		"webhook":   BindWebhookFlags,
		"dashboard": BindDashboardFlags,
	} {
		cmd := &cobra.Command{Use: use, RunE: func(*cobra.Command, []string) error { return nil }}
		bind(cmd)
		root.AddCommand(cmd)
	}
	return root
}

// Without an env key replacer, viper upper-cases the key as-is, so
// `dashboard.bind-address` -> `K8SSUSTAIN_DASHBOARD.BIND-ADDRESS`, which is not
// a legal POSIX env var name and never matches what users set.
func TestInitViper_EnvBinding_DotAndDashKeys(t *testing.T) {
	tests := []struct {
		name     string
		envVar   string
		envVal   string
		viperKey string
	}{
		{
			name:     "dot and dash (dashboard.bind-address)",
			envVar:   "K8SSUSTAIN_DASHBOARD_BIND_ADDRESS",
			envVal:   ":9999",
			viperKey: "dashboard.bind-address",
		},
		{
			name:     "dash only (log-level)",
			envVar:   "K8SSUSTAIN_LOG_LEVEL",
			envVal:   "debug",
			viperKey: "log-level",
		},
		{
			name:     "dot and dash (webhook.tls-cert-file)",
			envVar:   "K8SSUSTAIN_WEBHOOK_TLS_CERT_FILE",
			envVal:   "/etc/webhook/tls.crt",
			viperKey: "webhook.tls-cert-file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// InitViper operates on the package-level viper singleton.
			t.Cleanup(viper.Reset)
			viper.Reset()

			t.Setenv(tc.envVar, tc.envVal)

			InitViper()

			if got := viper.GetString(tc.viperKey); got != tc.envVal {
				t.Fatalf("viper.GetString(%q) = %q, want %q (env %s=%s)",
					tc.viperKey, got, tc.envVal, tc.envVar, tc.envVal)
			}
		})
	}
}

func TestLoadWebhookConfig_RoundTripsBoundFlags(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "webhook"}
	BindWebhookFlags(cmd)

	t.Setenv("K8SSUSTAIN_WEBHOOK_TLS_CERT_FILE", "/etc/foo.crt")
	t.Setenv("K8SSUSTAIN_WEBHOOK_PORT", "9999")
	InitViper()

	cfg := LoadWebhookConfig()
	if cfg.TLSCertFile != "/etc/foo.crt" {
		t.Errorf("TLSCertFile = %q, want /etc/foo.crt", cfg.TLSCertFile)
	}
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.TLSKeyFile != "/tls/tls.key" {
		t.Errorf("TLSKeyFile = %q, want /tls/tls.key (default)", cfg.TLSKeyFile)
	}
}

// Viper hands env values back as a single raw string, and GetStringSlice splits
// on whitespace rather than commas, so getStringSlice splits them itself.
func TestStringSlice_EnvOverride(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		want   []string
	}{
		{name: "comma separated", envVal: "kube-system,monitoring", want: []string{"kube-system", "monitoring"}},
		{name: "comma separated with spaces", envVal: "kube-system, monitoring", want: []string{"kube-system", "monitoring"}},
		{name: "single value", envVal: "kube-system", want: []string{"kube-system"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newRoot(t)
			t.Setenv("K8SSUSTAIN_EXCLUDED_NAMESPACES", tc.envVal)
			InitViper()

			got := mustLoadController(t).ExcludedNamespaces
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExcludedNamespaces = %v, want %v", got, tc.want)
			}
		})
	}
}

// A cluster-scope setting is one root flag, one key and one env var: set once,
// every subcommand reads it, so no component can run with its own copy.
func TestClusterScopeSettingsAreSetOnceForEverySubcommand(t *testing.T) {
	newRoot(t)
	t.Setenv("K8SSUSTAIN_RECOMMEND_ONLY", "true")
	t.Setenv("K8SSUSTAIN_EXCLUDED_NAMESPACES", "kube-system,monitoring")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_ADDRESS", "https://thanos.example:10902")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN_FILE", "/var/run/secrets/token")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_HEADERS", "X-Scope-OrgID=tenant-a")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "true")
	InitViper()

	controllerCfg, webhookCfg, dashboardCfg := mustLoadController(t), LoadWebhookConfig(), mustLoadDashboard(t)

	wantScope := ClusterScope{RecommendOnly: true, ExcludedNamespaces: []string{"kube-system", "monitoring"}}
	for name, got := range map[string]ClusterScope{
		"start":     controllerCfg.ClusterScope,
		"webhook":   webhookCfg.ClusterScope,
		"dashboard": dashboardCfg.ClusterScope,
	} {
		if !reflect.DeepEqual(got, wantScope) {
			t.Errorf("%s: cluster scope = %+v, want %+v", name, got, wantScope)
		}
	}
	wantPrometheus := PrometheusConnection{
		Address: "https://thanos.example:10902",
		Transport: promclient.TransportConfig{
			BearerTokenFile: "/var/run/secrets/token",
			Headers:         map[string]string{"X-Scope-OrgID": "tenant-a"},
			TLS:             promclient.TLSConfig{InsecureSkipVerify: true},
		},
	}
	for name, got := range map[string]PrometheusConnection{
		"start":     controllerCfg.Prometheus,
		"dashboard": dashboardCfg.Prometheus,
	} {
		if !reflect.DeepEqual(got, wantPrometheus) {
			t.Errorf("%s: Prometheus = %+v, want %+v", name, got, wantPrometheus)
		}
	}
}

// The flag form is the root's persistent flag, given after any subcommand;
// the webhook accepts the Prometheus ones too, and ignores them.
func TestClusterScopeFlagsParseAfterEverySubcommand(t *testing.T) {
	for subcommand, load := range map[string]func(*testing.T) []string{
		"start":     func(t *testing.T) []string { return mustLoadController(t).ExcludedNamespaces },
		"webhook":   func(*testing.T) []string { return LoadWebhookConfig().ExcludedNamespaces },
		"dashboard": func(t *testing.T) []string { return mustLoadDashboard(t).ExcludedNamespaces },
	} {
		t.Run(subcommand, func(t *testing.T) {
			root := newRoot(t)
			root.SetArgs([]string{subcommand, "--excluded-namespaces=kube-system,monitoring", "--prometheus-address=http://prometheus.example"})
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got, want := load(t), []string{"kube-system", "monitoring"}; !reflect.DeepEqual(got, want) {
				t.Errorf("ExcludedNamespaces = %v, want %v", got, want)
			}
		})
	}
}

func TestLoadDashboardConfig_RoundTripsBoundFlags(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "dashboard"}
	BindDashboardFlags(cmd)

	t.Setenv("K8SSUSTAIN_DASHBOARD_BIND_ADDRESS", ":7777")
	InitViper()

	cfg := mustLoadDashboard(t)
	if cfg.BindAddress != ":7777" {
		t.Errorf("BindAddress = %q, want :7777", cfg.BindAddress)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info (default)", cfg.LogLevel)
	}
}

// The 7d default is not arbitrary: the WorkloadRecommendation is the webhook's
// only recommendation source, so an object reaped between two runs of a
// recurring ephemeral identity makes every run cold start. 7d clears the weekly
// batch cycle that 72h did not.
func TestControllerConfig_RecommendationRetentionDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	if got := mustLoadController(t).RecommendationRetention; got != 168*time.Hour {
		t.Errorf("RecommendationRetention = %v, want 168h", got)
	}
}

// Every Recommendation is stamped servable for a window derived from the
// reconcile interval or the retention window, so either at zero or less would
// have the webhook inject nothing: the controller refuses to start instead.
func TestControllerConfig_RejectsANonPositiveIntervalOrRetention(t *testing.T) {
	for _, tc := range []struct{ env, value, flag string }{
		{"K8SSUSTAIN_RECONCILE_INTERVAL", "0s", "--reconcile-interval"},
		{"K8SSUSTAIN_RECONCILE_INTERVAL", "-5m", "--reconcile-interval"},
		{"K8SSUSTAIN_RECOMMENDATION_RETENTION", "0s", "--recommendation-retention"},
		{"K8SSUSTAIN_RECOMMENDATION_RETENTION", "-1h", "--recommendation-retention"},
	} {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			t.Cleanup(viper.Reset)
			viper.Reset()
			cmd := &cobra.Command{}
			BindControllerFlags(cmd)
			t.Setenv(tc.env, tc.value)
			InitViper()

			if _, err := LoadControllerConfig(); err == nil || !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("err = %v, want one naming %s", err, tc.flag)
			}
		})
	}
}

func TestControllerConfig_PolicyConcurrencyLimitDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	if got := mustLoadController(t).PolicyConcurrencyLimit; got != 10 {
		t.Errorf("PolicyConcurrencyLimit = %d, want 10", got)
	}
}

func TestControllerConfig_PolicyConcurrencyLimitEnvOverride(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	t.Setenv("K8SSUSTAIN_POLICY_CONCURRENCY_LIMIT", "3")
	InitViper()
	if got := mustLoadController(t).PolicyConcurrencyLimit; got != 3 {
		t.Errorf("PolicyConcurrencyLimit = %d, want 3 (env override)", got)
	}
}

// The default of 8 is kept well under Prometheus's own --query.max-concurrency
// (20) so the controller does not starve dashboards and alerting sharing the
// same server.
func TestControllerConfig_PrometheusMaxInflightDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	cfg := mustLoadController(t)
	if cfg.PrometheusMaxInflight != 8 {
		t.Fatalf("PrometheusMaxInflight: got %d want 8", cfg.PrometheusMaxInflight)
	}
}

// internal/prometheus/shardscale_test.go asserts its shard-collapsing scale
// property against this same exported constant, so the two cannot drift.
func TestControllerConfig_QueryShardMaxSamplesDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	cfg := mustLoadController(t)
	if cfg.QueryShardMaxSamples != DefaultQueryShardMaxSamples {
		t.Fatalf("QueryShardMaxSamples: got %d want %d", cfg.QueryShardMaxSamples, DefaultQueryShardMaxSamples)
	}
	if DefaultQueryShardMaxSamples != 10_000_000 {
		t.Fatalf("DefaultQueryShardMaxSamples changed to %d -- this is a 5x safety margin under Prometheus's own --query.max-samples default of 50_000_000; changing it is a deliberate decision, not accidental drift", DefaultQueryShardMaxSamples)
	}
}

func TestControllerConfig_QueryShardMaxSamplesEnvOverride(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	t.Setenv("K8SSUSTAIN_QUERY_SHARD_MAX_SAMPLES", "1000000")
	InitViper()
	if got := mustLoadController(t).QueryShardMaxSamples; got != 1_000_000 {
		t.Errorf("QueryShardMaxSamples = %d, want 1000000 (env override)", got)
	}
}

// TestParseHeaders covers the --prometheus-headers Key=Value grammar,
// including the values a Thanos/Mimir tenant id can legitimately take.
func TestParseHeaders(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    map[string]string
		wantErr string
	}{
		{
			name:    "empty list yields no map",
			entries: nil,
			want:    nil,
		},
		{
			name:    "single header",
			entries: []string{"X-Scope-OrgID=tenant-a"},
			want:    map[string]string{"X-Scope-OrgID": "tenant-a"},
		},
		{
			name:    "several headers, surrounding spaces trimmed",
			entries: []string{" X-Scope-OrgID = tenant-a ", "X-Extra=v"},
			want:    map[string]string{"X-Scope-OrgID": "tenant-a", "X-Extra": "v"},
		},
		{
			name: "only the first = splits, so values may contain =",
			// A base64 tenant id or an opaque proxy token routinely ends in '='.
			entries: []string{"X-Token=abc=="},
			want:    map[string]string{"X-Token": "abc=="},
		},
		{
			name:    "empty value is allowed",
			entries: []string{"X-Empty="},
			want:    map[string]string{"X-Empty": ""},
		},
		{
			name:    "missing =",
			entries: []string{"X-Scope-OrgID"},
			wantErr: "expected Key=Value",
		},
		{
			name:    "empty key",
			entries: []string{"  =tenant-a"},
			wantErr: "empty header name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHeaders(tc.entries)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseHeaders(%q) = %v, want error containing %q", tc.entries, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHeaders(%q): %v", tc.entries, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseHeaders(%q) = %v, want %v", tc.entries, got, tc.want)
			}
		})
	}
}

func TestControllerConfig_PrometheusTransportEnvOverrides(t *testing.T) {
	newRoot(t)

	t.Setenv("K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN_FILE", "/var/run/secrets/token")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_HEADERS", "X-Scope-OrgID=tenant-a,X-Extra=v")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_CA_FILE", "/etc/ca.pem")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_CERT_FILE", "/etc/tls.crt")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_KEY_FILE", "/etc/tls.key")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_SERVER_NAME", "prometheus.internal")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "true")
	InitViper()

	got := mustLoadController(t).Prometheus.Transport
	want := promclient.TransportConfig{
		BearerTokenFile: "/var/run/secrets/token",
		Headers:         map[string]string{"X-Scope-OrgID": "tenant-a", "X-Extra": "v"},
		TLS: promclient.TLSConfig{
			CAFile:             "/etc/ca.pem",
			CertFile:           "/etc/tls.crt",
			KeyFile:            "/etc/tls.key",
			ServerName:         "prometheus.internal",
			InsecureSkipVerify: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PrometheusTransport = %+v, want %+v", got, want)
	}
}

// The bearer-token test above leaves these unset: the two are mutually
// exclusive in the client.
func TestControllerConfig_PrometheusBasicAuthEnvOverrides(t *testing.T) {
	newRoot(t)

	t.Setenv("K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN", "inline-token")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_USERNAME", "user")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_PASSWORD", "pass")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_PASSWORD_FILE", "/etc/password")
	InitViper()

	got := mustLoadController(t).Prometheus.Transport
	if got.BearerToken != "inline-token" {
		t.Errorf("BearerToken = %q, want inline-token", got.BearerToken)
	}
	if got.BasicAuthUsername != "user" || got.BasicAuthPassword != "pass" || got.BasicAuthPasswordFile != "/etc/password" {
		t.Errorf("basic auth = (%q, %q, %q), want (user, pass, /etc/password)",
			got.BasicAuthUsername, got.BasicAuthPassword, got.BasicAuthPasswordFile)
	}
}

// An install setting none of these flags must produce the zero TransportConfig,
// which internal/prometheus resolves to "no RoundTripper".
func TestControllerConfig_PrometheusTransportDefaultIsEmpty(t *testing.T) {
	newRoot(t)

	if got := mustLoadController(t).Prometheus.Transport; !reflect.DeepEqual(got, promclient.TransportConfig{}) {
		t.Fatalf("PrometheusTransport = %+v, want the zero value", got)
	}
}

// A malformed entry must fail every Load that reads it, so no call site can
// start the process with the headers silently missing.
func TestPrometheusHeadersParseErrorFailsTheLoad(t *testing.T) {
	newRoot(t)
	t.Setenv("K8SSUSTAIN_PROMETHEUS_HEADERS", "not-a-header")
	InitViper()

	_, controllerErr := LoadControllerConfig()
	_, dashboardErr := LoadDashboardConfig()
	for name, err := range map[string]error{"start": controllerErr, "dashboard": dashboardErr} {
		if err == nil || !strings.Contains(err.Error(), "expected Key=Value") {
			t.Errorf("%s: err = %v, want one explaining the Key=Value grammar", name, err)
		}
	}
}

// Pins the exact flag names, so a rename is a deliberate act rather than a
// silent break of the Helm chart and docs that reference them, and keeps each
// a root persistent flag no subcommand shadows with a flag of its own.
func TestClusterScopeFlagsAreRootPersistentFlags(t *testing.T) {
	root := newRoot(t)
	for _, name := range []string{
		"recommend-only",
		"excluded-namespaces",
		"prometheus-address",
		"prometheus-bearer-token",
		"prometheus-bearer-token-file",
		"prometheus-basic-auth-username",
		"prometheus-basic-auth-password",
		"prometheus-basic-auth-password-file",
		"prometheus-headers",
		"prometheus-tls-ca-file",
		"prometheus-tls-cert-file",
		"prometheus-tls-key-file",
		"prometheus-tls-server-name",
		"prometheus-tls-insecure-skip-verify",
	} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("--%s is not a root persistent flag", name)
		}
		for _, sub := range root.Commands() {
			if sub.LocalNonPersistentFlags().Lookup(name) != nil {
				t.Errorf("%s registers its own --%s", sub.Name(), name)
			}
		}
	}
}

// --prometheus-headers is a StringArray (one header per occurrence, verbatim),
// NOT a CSV StringSlice, so a header value may contain a comma. The chart
// renders one flag per header and relies on exactly this.
func TestPrometheusHeadersFlagIsRepeatableAndCommaSafe(t *testing.T) {
	root := newRoot(t)
	if err := root.PersistentFlags().Parse([]string{
		"--prometheus-headers=Accept=application/json,text/plain",
		`--prometheus-headers=X-Quoted=say "hi"`,
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := mustLoadController(t).Prometheus.Transport.Headers
	want := map[string]string{
		"Accept":   "application/json,text/plain",
		"X-Quoted": `say "hi"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Headers = %v, want %v", got, want)
	}
}
