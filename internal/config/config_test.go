package config

import (
	"testing"

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
		name    string
		envVar  string
		envVal  string
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
