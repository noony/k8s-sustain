// Package dashboard implements the read-only HTTP API that powers the
// k8s-sustain dashboard UI.
//
// Handlers are split by topic for readability:
//   - handlers_policies.go        — /api/policies, /api/policies/{name}
//   - handlers_workloads.go       — /api/policies/{name}/workloads, /api/workloads
//   - handlers_workload_metrics.go — /api/workloads/{ns}/{kind}/{name}/(metrics|recommendations)
//   - handlers_workload_detail.go — /api/workloads/{ns}/{kind}/{name}
//   - handlers_simulate.go        — /api/simulate
//
// Cross-cutting helpers (workload-kind switching, Prometheus signal fan-out,
// shared types) live in handlers_workload_kinds.go.
package dashboard
