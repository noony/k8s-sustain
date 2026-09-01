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

// TestInitViper_EnvBinding_DotAndDashKeys verifies that environment variables
// with the K8SSUSTAIN_ prefix bind to viper keys that contain dots (subkey
// separators) and dashes (kebab-case), as used throughout this package.
//
// Without an env key replacer, viper looks up env vars by upper-casing the key
// as-is, so `dashboard.bind-address` -> `K8SSUSTAIN_DASHBOARD.BIND-ADDRESS`,
// which is not a legal POSIX env var name and never matches what users set.
// The replacer maps `.` and `-` to `_` so users can sensibly export
// `K8SSUSTAIN_DASHBOARD_BIND_ADDRESS`.
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
			// Use a clean global viper instance and restore it after the test
			// since InitViper operates on the package-level singleton.
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

// TestLoadWebhookConfig_RoundTripsBoundFlags verifies BindWebhookFlags +
// LoadWebhookConfig surface bound-flag defaults and env overrides correctly.
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

// TestBindWebhookFlags_NoPrometheusAddress verifies the webhook subcommand no
// longer registers --prometheus-address. The webhook's Prometheus client was
// removed (it now reads recommendations exclusively from the cached
// WorkloadRecommendation); leaving the flag bound would be a dead knob that
// looks like it does something and does nothing.
func TestBindWebhookFlags_NoPrometheusAddress(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "webhook"}
	BindWebhookFlags(cmd)

	if f := cmd.Flags().Lookup("prometheus-address"); f != nil {
		t.Errorf("BindWebhookFlags registered --prometheus-address, want it gone")
	}
}

// TestStringSlice_EnvOverride verifies that string-slice flags can be
// overridden via environment variable using the same comma-separated syntax
// as --flag=a,b. Viper hands env values back as a single raw string (and
// GetStringSlice splits strings on whitespace, not commas), so getStringSlice
// has to do the comma splitting itself.
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

// TestStringSlice_EnvOverride_SubcommandKey covers the dotted-key variant
// (webhook.excluded-namespaces) of the comma-separated env override.
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

// TestStringSlice_FlagStillWorks ensures the comma-splitting env fix does not
// regress the flag path, which already parses CSV via pflag.
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

// TestLoadDashboardConfig_RoundTripsBoundFlags mirrors the webhook test for
// the dashboard subcommand.
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

// TestControllerConfig_RecommendationRetentionDefault verifies the flag
// registers with its 168h (7d) default and threads into ControllerConfig.
//
// The value is not arbitrary. Since the webhook's only recommendation source
// is the WorkloadRecommendation object, this window decides whether a
// recurring ephemeral identity is rightsized at admission on its next run: if
// the object is reaped between two runs, every run cold starts. 7d clears the
// weekly batch cycle that 72h did not.
func TestControllerConfig_RecommendationRetentionDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	if got := mustLoadController(t).RecommendationRetention; got != 168*time.Hour {
		t.Errorf("RecommendationRetention = %v, want 168h", got)
	}
}

// TestControllerConfig_PolicyConcurrencyLimitDefault verifies the flag
// registers with its 10 default and threads into ControllerConfig.
func TestControllerConfig_PolicyConcurrencyLimitDefault(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)
	if got := mustLoadController(t).PolicyConcurrencyLimit; got != 10 {
		t.Errorf("PolicyConcurrencyLimit = %d, want 10", got)
	}
}

// TestControllerConfig_PolicyConcurrencyLimitEnvOverride verifies the flag can
// be overridden via K8SSUSTAIN_POLICY_CONCURRENCY_LIMIT.
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

// TestControllerConfig_PrometheusMaxInflightDefault verifies the
// --prometheus-max-inflight flag registers with its default of 8 and threads
// into ControllerConfig. The default is kept well under Prometheus's own
// --query.max-concurrency (20) so the controller does not starve dashboards
// and alerting sharing the same Prometheus server.
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

// TestControllerConfig_QueryShardMaxSamplesDefault verifies the
// --query-shard-max-samples flag registers with the shared
// DefaultQueryShardMaxSamples constant and threads into ControllerConfig.
// internal/prometheus/shardscale_test.go asserts its shard-collapsing scale
// property against this same exported constant, so the two can never drift
// apart silently -- see that test's doc comment.
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

// TestControllerConfig_QueryShardMaxSamplesEnvOverride verifies the flag can
// be overridden via K8SSUSTAIN_QUERY_SHARD_MAX_SAMPLES.
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

// --- Prometheus transport (auth / TLS) -------------------------------------

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

// TestControllerConfig_PrometheusTransportEnvOverrides verifies every
// Prometheus auth/TLS flag binds to its K8SSUSTAIN_-prefixed env var and lands
// in the promclient.TransportConfig the call site passes to New.
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

// TestControllerConfig_PrometheusBasicAuthEnvOverrides covers the basic-auth
// trio, which the bearer-token test above deliberately leaves unset (the two
// are mutually exclusive in the client).
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

// TestControllerConfig_PrometheusTransportDefaultIsEmpty pins that an install
// that sets none of these flags produces the zero TransportConfig, which
// internal/prometheus resolves to "no RoundTripper" — i.e. the pre-auth
// behaviour is unchanged by default.
func TestControllerConfig_PrometheusTransportDefaultIsEmpty(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	cmd := &cobra.Command{}
	BindControllerFlags(cmd)

	if got := mustLoadController(t).PrometheusTransport; !reflect.DeepEqual(got, promclient.TransportConfig{}) {
		t.Fatalf("PrometheusTransport = %+v, want the zero value", got)
	}
}

// TestControllerConfig_PrometheusHeadersParseError verifies a malformed entry
// fails LoadControllerConfig itself rather than being silently dropped, so no
// call site can start the process with the headers missing.
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

// TestDashboardConfig_PrometheusTransportEnvOverrides verifies the dashboard
// binds the same flag names under "dashboard."-prefixed viper keys, so the two
// subcommands can be configured independently (see bindPrometheusTransportFlags
// for why they cannot share one flat key).
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

// TestPrometheusTransportFlagsRegisteredOnBothCommands pins the exact flag
// names, so a rename here is a deliberate act rather than a silent break of
// the Helm chart and docs that reference them.
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

// TestPrometheusHeadersFlagIsRepeatableAndCommaSafe pins the flag's grammar:
// --prometheus-headers is a StringArray (one header per occurrence, verbatim),
// NOT a CSV StringSlice, so a header value may itself contain a comma. The
// chart renders one flag per header and relies on exactly this.
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
