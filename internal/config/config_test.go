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

// The webhook reads recommendations exclusively from the cached
// WorkloadRecommendation; a bound --prometheus-address would be a dead knob
// that looks like it does something.
func TestBindWebhookFlags_NoPrometheusAddress(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "webhook"}
	BindWebhookFlags(cmd)

	if f := cmd.Flags().Lookup("prometheus-address"); f != nil {
		t.Errorf("BindWebhookFlags registered --prometheus-address, want it gone")
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
			t.Cleanup(viper.Reset)
			viper.Reset()

			cmd := &cobra.Command{Use: "start"}
			BindControllerFlags(cmd)

			t.Setenv("K8SSUSTAIN_EXCLUDED_NAMESPACES", tc.envVal)
			InitViper()

			got := mustLoadController(t).ExcludedNamespaces
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExcludedNamespaces = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStringSlice_EnvOverride_SubcommandKey(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "webhook"}
	BindWebhookFlags(cmd)

	t.Setenv("K8SSUSTAIN_WEBHOOK_EXCLUDED_NAMESPACES", "kube-system,monitoring")
	InitViper()

	got := LoadWebhookConfig().ExcludedNamespaces
	want := []string{"kube-system", "monitoring"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExcludedNamespaces = %v, want %v", got, want)
	}
}

// The flag path already parses CSV via pflag; the env-side comma splitting must
// not regress it.
func TestStringSlice_FlagStillWorks(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "start"}
	BindControllerFlags(cmd)
	if err := cmd.Flags().Set("excluded-namespaces", "kube-system,monitoring"); err != nil {
		t.Fatalf("setting flag: %v", err)
	}
	InitViper()

	got := mustLoadController(t).ExcludedNamespaces
	want := []string{"kube-system", "monitoring"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExcludedNamespaces = %v, want %v", got, want)
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
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)

	t.Setenv("K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN_FILE", "/var/run/secrets/token")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_HEADERS", "X-Scope-OrgID=tenant-a,X-Extra=v")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_CA_FILE", "/etc/ca.pem")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_CERT_FILE", "/etc/tls.crt")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_KEY_FILE", "/etc/tls.key")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_SERVER_NAME", "prometheus.internal")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "true")
	InitViper()

	got := mustLoadController(t).PrometheusTransport
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
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)

	t.Setenv("K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN", "inline-token")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_USERNAME", "user")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_PASSWORD", "pass")
	t.Setenv("K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_PASSWORD_FILE", "/etc/password")
	InitViper()

	got := mustLoadController(t).PrometheusTransport
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
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)

	if got := mustLoadController(t).PrometheusTransport; !reflect.DeepEqual(got, promclient.TransportConfig{}) {
		t.Fatalf("PrometheusTransport = %+v, want the zero value", got)
	}
}

// A malformed entry must fail LoadControllerConfig itself, so no call site can
// start the process with the headers silently missing.
func TestControllerConfig_PrometheusHeadersParseError(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)

	t.Setenv("K8SSUSTAIN_PROMETHEUS_HEADERS", "not-a-header")
	InitViper()

	if _, err := LoadControllerConfig(); err == nil {
		t.Fatal("LoadControllerConfig succeeded, want a parse error")
	} else if !strings.Contains(err.Error(), "expected Key=Value") {
		t.Fatalf("error = %v, want it to explain the Key=Value grammar", err)
	}
}

// The dashboard binds the same flag names under "dashboard."-prefixed viper
// keys so the two subcommands are configurable independently; see
// bindPrometheusTransportFlags for why they cannot share one flat key.
func TestDashboardConfig_PrometheusTransportEnvOverrides(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{Use: "dashboard"}
	BindDashboardFlags(cmd)

	t.Setenv("K8SSUSTAIN_DASHBOARD_PROMETHEUS_BEARER_TOKEN_FILE", "/var/run/secrets/dash-token")
	t.Setenv("K8SSUSTAIN_DASHBOARD_PROMETHEUS_HEADERS", "X-Scope-OrgID=tenant-b")
	t.Setenv("K8SSUSTAIN_DASHBOARD_PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "true")
	InitViper()

	got := mustLoadDashboard(t).PrometheusTransport
	if got.BearerTokenFile != "/var/run/secrets/dash-token" {
		t.Errorf("BearerTokenFile = %q, want /var/run/secrets/dash-token", got.BearerTokenFile)
	}
	if !reflect.DeepEqual(got.Headers, map[string]string{"X-Scope-OrgID": "tenant-b"}) {
		t.Errorf("Headers = %v, want X-Scope-OrgID=tenant-b", got.Headers)
	}
	if !got.TLS.InsecureSkipVerify {
		t.Error("TLS.InsecureSkipVerify = false, want true")
	}
}

// Pins the exact flag names, so a rename is a deliberate act rather than a
// silent break of the Helm chart and docs that reference them.
func TestPrometheusTransportFlagsRegisteredOnBothCommands(t *testing.T) {
	want := []string{
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
	}

	for _, tc := range []struct {
		name string
		bind func(*cobra.Command)
	}{
		{"controller", BindControllerFlags},
		{"dashboard", BindDashboardFlags},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(viper.Reset)
			viper.Reset()
			cmd := &cobra.Command{Use: tc.name}
			tc.bind(cmd)
			for _, flagName := range want {
				if cmd.Flags().Lookup(flagName) == nil {
					t.Errorf("--%s is not registered on the %s command", flagName, tc.name)
				}
			}
		})
	}
}

// --prometheus-headers is a StringArray (one header per occurrence, verbatim),
// NOT a CSV StringSlice, so a header value may contain a comma. The chart
// renders one flag per header and relies on exactly this.
func TestPrometheusHeadersFlagIsRepeatableAndCommaSafe(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	if err := cmd.Flags().Parse([]string{
		"--prometheus-headers=Accept=application/json,text/plain",
		`--prometheus-headers=X-Quoted=say "hi"`,
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := mustLoadController(t).PrometheusTransport.Headers
	want := map[string]string{
		"Accept":   "application/json,text/plain",
		"X-Quoted": `say "hi"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Headers = %v, want %v", got, want)
	}
}
