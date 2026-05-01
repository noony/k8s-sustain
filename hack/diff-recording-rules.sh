#!/usr/bin/env bash
# diff-recording-rules.sh — verify recording rules are identical between the
# bundled prometheus subchart values block and the standalone PrometheusRule CRD.
#
# Both copies must stay in lockstep:
#   - charts/k8s-sustain/values.yaml         → prometheus.serverFiles."recording_rules.yml".groups
#   - charts/k8s-sustain/templates/prometheusrules.yaml → .spec.groups (rendered)
#
# Drift means users who pick the bundled prometheus get different rules than
# users who deploy with serviceMonitor.enabled=true. Run via `make verify-rules`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/k8s-sustain"

if ! command -v yq >/dev/null 2>&1; then
  echo "ERROR: yq is required (https://github.com/mikefarah/yq)" >&2
  exit 2
fi
if ! command -v helm >/dev/null 2>&1; then
  echo "ERROR: helm is required" >&2
  exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Source A: rules embedded in values.yaml (used by bundled prometheus subchart).
yq eval '.prometheus.serverFiles."recording_rules.yml".groups' \
  "${CHART_DIR}/values.yaml" > "${tmp}/from-values.yaml"

# Source B: rules in PrometheusRule template, rendered with serviceMonitor enabled.
helm template tmp "${CHART_DIR}" \
  --set controller.serviceMonitor.enabled=true \
  --show-only templates/prometheusrules.yaml 2>/dev/null \
  | yq eval '.spec.groups' - > "${tmp}/from-template.yaml"

if ! diff -u "${tmp}/from-values.yaml" "${tmp}/from-template.yaml"; then
  echo
  echo "ERROR: recording rules drift detected between values.yaml and prometheusrules.yaml." >&2
  echo "Edit BOTH files together and re-run 'make verify-rules'." >&2
  exit 1
fi

echo "Recording rules are in sync."
