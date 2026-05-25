package config

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

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
			name:     "dot and dash (webhook.prometheus-address)",
			envVar:   "K8SSUSTAIN_WEBHOOK_PROMETHEUS_ADDRESS",
			envVal:   "http://prom.example:9090",
			viperKey: "webhook.prometheus-address",
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

// TestLoadDashboardConfig_RoundTripsBoundFlags mirrors the webhook test for
// the dashboard subcommand.
func TestLoadDashboardConfig_RoundTripsBoundFlags(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()

	cmd := &cobra.Command{Use: "dashboard"}
	BindDashboardFlags(cmd)

	t.Setenv("K8SSUSTAIN_DASHBOARD_BIND_ADDRESS", ":7777")
	InitViper()

	cfg := LoadDashboardConfig()
	if cfg.BindAddress != ":7777" {
		t.Errorf("BindAddress = %q, want :7777", cfg.BindAddress)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info (default)", cfg.LogLevel)
	}
}
