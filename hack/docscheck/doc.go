// Package docscheck fails when docs/reference drifts from the flags (per command, with their defaults), metrics,
// recording rules and Helm values (with their defaults) it documents, and when a flag the chart renders from a value
// disagrees with the binary's default.
package docscheck
