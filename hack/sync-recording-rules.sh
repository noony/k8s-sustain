#!/usr/bin/env bash
# sync-recording-rules.sh — write the canonical recording rules into
# the bundled prometheus subchart's values.yaml.
#
# Single source of truth: charts/k8s-sustain/files/recording-rules.yaml.
# That file is consumed directly by templates/prometheusrules.yaml via
# `.Files.Get`. The bundled prometheus subchart cannot read that file at
# install time (it expects rules under prometheus.serverFiles in the
# parent values), so we mirror the rules into values.yaml from the
# canonical copy.
#
# Strategy: marker-based block replacement so the rest of values.yaml —
# comments, formatting, blank lines — stays untouched. yq's full-file
# rewrite would normalize whitespace and shift unrelated comments.
#
# Run:
#   make sync-rules     # write rules into values.yaml
#   make verify-rules   # fail if values.yaml is not in sync (CI)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/k8s-sustain"
RULES="${CHART_DIR}/files/recording-rules.yaml"
VALUES="${CHART_DIR}/values.yaml"

readonly BEGIN_MARKER="# BEGIN recording-rules"
readonly END_MARKER="# END recording-rules"

if [[ ! -f "${RULES}" ]]; then
  echo "ERROR: ${RULES} not found" >&2
  exit 2
fi
if ! grep -q "${BEGIN_MARKER}" "${VALUES}"; then
  echo "ERROR: ${BEGIN_MARKER} not found in ${VALUES}" >&2
  exit 2
fi
if ! grep -q "${END_MARKER}" "${VALUES}"; then
  echo "ERROR: ${END_MARKER} not found in ${VALUES}" >&2
  exit 2
fi

tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT

awk -v begin="${BEGIN_MARKER}" -v end="${END_MARKER}" -v rules="${RULES}" '
  BEGIN { state = "before" }
  state == "before" {
    print
    if (index($0, begin)) {
      state = "skip"
      # Re-emit the canonical block, indented to match the existing nesting
      # under prometheus.serverFiles."recording_rules.yml" (groups: at 6,
      # items at 8). Header lines (# Source... # Run...) are preserved as
      # part of the BEGIN marker block we just printed.
      print "      # Source of truth: charts/k8s-sustain/files/recording-rules.yaml"
      print "      # Run `make sync-rules` after editing the canonical file."
      print "      groups:"
      while ((getline line < rules) > 0) {
        if (line == "") { print ""; continue }
        print "        " line
      }
      close(rules)
    }
    next
  }
  state == "skip" {
    if (index($0, end)) {
      state = "after"
      print
    }
    next
  }
  { print }
' "${VALUES}" > "${tmp}"

mv "${tmp}" "${VALUES}"

echo "Synced recording rules from ${RULES#"${REPO_ROOT}"/} into ${VALUES#"${REPO_ROOT}"/}."
