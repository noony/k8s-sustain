#!/usr/bin/env bash
# Validates the rendered PrometheusRule manifest with promtool and asserts
# that all dashboard-required recording rules are present.
#
# This script always cd's to the worktree root (3 levels above this file)
# before running helm, so it can be invoked from any cwd.
set -euo pipefail

cd "$(dirname "$0")/../../.."

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

helm template k8s-sustain ./charts/k8s-sustain \
  --set controller.serviceMonitor.enabled=true \
  --show-only templates/prometheusrules.yaml \
  > "$WORKDIR/rules.yaml"

# Strip the kubernetes object envelope and keep just `groups:` for promtool.
yq '.spec' "$WORKDIR/rules.yaml" > "$WORKDIR/rules-only.yaml"

promtool check rules "$WORKDIR/rules-only.yaml"

required=(
  "k8s_sustain:pod_container_cpu_request:cores"
  "k8s_sustain:pod_container_memory_request:bytes"
  "k8s_sustain:cluster_cpu_savings_cores"
  "k8s_sustain:cluster_memory_savings_bytes"
  "k8s_sustain:cluster_cpu_savings_ratio"
  "k8s_sustain:cluster_memory_savings_ratio"
  "k8s_sustain:cluster_cpu_headroom_breakdown"
  "k8s_sustain:policy_cpu_savings_cores"
  "k8s_sustain:policy_memory_savings_bytes"
  "k8s_sustain:workload_oom_24h"
  "k8s_sustain:workload_drifted"
)
for rule in "${required[@]}"; do
  if ! grep -q "record: ${rule}" "$WORKDIR/rules-only.yaml"; then
    echo "missing recording rule: ${rule}" >&2
    exit 1
  fi
done

echo "OK: rules valid and contain required entries"
