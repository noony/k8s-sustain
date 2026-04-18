#!/usr/bin/env bash
# Wraps the controller-gen CRD with Helm template directives and writes it into
# the chart.  Run via "make manifests".
set -euo pipefail

SRC="config/crd/bases/k8s.sustain.io_policies.yaml"
DST="charts/k8s-sustain/templates/crd-policy.yaml"

if [ ! -f "$SRC" ]; then
  echo "ERROR: $SRC not found — run controller-gen first" >&2
  exit 1
fi

cat > "$DST" <<'HELM_HEADER'
{{- if .Values.installCRDs }}
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: policies.k8s.sustain.io
  labels:
    {{- include "k8s-sustain.labels" . | nindent 4 }}
  annotations:
    helm.sh/resource-policy: keep
HELM_HEADER

# Append everything after the "metadata:" block from the generated CRD (i.e. from "spec:" onward).
sed -n '/^spec:/,$ p' "$SRC" >> "$DST"

echo '{{- end }}' >> "$DST"

echo "Synced $SRC -> $DST"
