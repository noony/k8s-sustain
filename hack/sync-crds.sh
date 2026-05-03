#!/usr/bin/env bash
# Wraps the controller-gen CRDs with Helm template directives and writes them
# into the chart. Run via "make manifests".
set -euo pipefail

sync_one() {
  local src="$1"
  local dst="$2"
  local crd_name="$3"

  if [ ! -f "$src" ]; then
    echo "ERROR: $src not found — run controller-gen first" >&2
    exit 1
  fi

  cat > "$dst" <<HELM_HEADER
{{- if .Values.installCRDs }}
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ${crd_name}
  labels:
    {{- include "k8s-sustain.labels" . | nindent 4 }}
  annotations:
    helm.sh/resource-policy: keep
HELM_HEADER

  # Append everything from "spec:" onward in the generated CRD.
  sed -n '/^spec:/,$ p' "$src" >> "$dst"
  echo '{{- end }}' >> "$dst"

  echo "Synced $src -> $dst"
}

sync_one "config/crd/bases/k8s.sustain.io_policies.yaml" \
         "charts/k8s-sustain/templates/crd-policy.yaml" \
         "policies.k8s.sustain.io"

sync_one "config/crd/bases/k8s.sustain.io_workloadrecommendations.yaml" \
         "charts/k8s-sustain/templates/crd-workloadrecommendation.yaml" \
         "workloadrecommendations.k8s.sustain.io"
