// Package version exposes the build-time version string.
// Release builds set Version via -ldflags "-X .../internal/version.Version=...".
// Dev builds leave it as "dev".
package version

// Version is the build-time version. Overridden by the release workflow.
var Version = "dev"
