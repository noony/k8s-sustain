#!/usr/bin/env bash
# Print a compact table of current vs. recommended resource requests for every
# active scenario. Sources:
#   - current requests:   the scenario's pods (targets_for picks the selector)
#   - recommendations:    the stored Recommendation in
#                         GET /api/workloads/<ns>/<kind>/<identity> on the
#                         k8s-sustain dashboard service (via kubectl proxy)
#   - recycled:           heuristic — the pod's CPU request differs from the
#                         value baked into hack/scenarios/<name>.yaml.
# Every scenario in Makefile.scenarios must be listed in SCENARIOS below.
set -euo pipefail

SCENARIOS=(steady overprovisioned underprovisioned stepped hpa hpa-coordinated hpa-replica-anchor init-containers oom-kill qos-change cronjob cronjob-long-running cronjob-overprovisioned job statefulset custom-name bare-pod recommend-only coldstart recurring namespace-optin)
DASHBOARD_SVC=k8s-sustain-dashboard
DASHBOARD_NS=k8s-sustain
DASHBOARD_PORT=8090

trap 'kill $PROXY_PID 2>/dev/null || true' EXIT
kubectl proxy --port=0 >/tmp/k8s-sustain-proxy.log 2>&1 &
PROXY_PID=$!

# Wait for the proxy to come up and capture the port it bound to.
for _ in $(seq 1 30); do
  if grep -q 'Starting to serve on' /tmp/k8s-sustain-proxy.log 2>/dev/null; then
    PORT=$(awk '/Starting to serve on/ {n=split($0,a,":"); print a[n]}' /tmp/k8s-sustain-proxy.log)
    break
  fi
  sleep 0.1
done
: "${PORT:?failed to start kubectl proxy}"

BASE="http://127.0.0.1:${PORT}/api/v1/namespaces/${DASHBOARD_NS}/services/${DASHBOARD_SVC}:${DASHBOARD_PORT}/proxy"

printf '%-28s %-22s %-9s %-9s %-9s %-9s %-8s\n' \
  NAMESPACE POD CPUreq CPUrec MEMreq MEMrec RECYCLED

# One line per workload: <kind> <identity name> <pod label selector> <latest|all>.
# "latest" shows only the newest pod (batch runs, bare pods); "all" every replica.
# The identity name is the owner-name override when the scenario sets one.
targets_for() {
  case "$1" in
    cronjob)                 echo "CronJob job app=stress latest" ;;
    cronjob-long-running)    echo "CronJob long-job app=stress latest" ;;
    cronjob-overprovisioned) echo "CronJob overprovisioned app=stress latest" ;;
    job)                     echo "Job oneshot app=stress latest" ;;
    coldstart)               echo "Job etl app=etl latest" ;;
    statefulset)             echo "StatefulSet web app=web all" ;;
    custom-name)             echo "Deployment renamed-app app=stress all" ;;
    bare-pod)                echo "Pod etl-daily app=toto latest" ;;
    recurring)               echo "Pod etl-recurring app=etl-recurring latest" ;;
    namespace-optin)         printf '%s\n' "Deployment web app=web all" "Deployment opted-out app=opted-out all" ;;
    *)                       echo "Deployment stress app=stress all" ;;
  esac
}

row() {
  printf '%-28s %-22s %-9s %-9s %-9s %-9s %-8s\n' "$@"
}

for name in "${SCENARIOS[@]}"; do
  ns="scenario-${name}"
  if ! kubectl get ns "${ns}" >/dev/null 2>&1; then continue; fi

  # Original CPU request baked into the scenario YAML; a running pod that
  # differs has already been resized, recycled or webhook-injected.
  orig=$(grep -A2 'requests:' "$(dirname "$0")/${name}.yaml" | awk '/cpu:/ {print $2; exit}')

  while read -r wlkind wlname selector mode; do
    rec_json=$(curl -fsS "${BASE}/api/workloads/${ns}/${wlkind}/${wlname}" 2>/dev/null || echo '{}')
    cpu_rec=$(echo "${rec_json}" | grep -o '"cpuRequest":"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)
    mem_rec=$(echo "${rec_json}" | grep -o '"memoryRequest":"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)
    cpu_rec=${cpu_rec:-?}
    mem_rec=${mem_rec:-?}

    if [ "${mode}" = latest ]; then
      pods=$(kubectl get pod -n "${ns}" -l "${selector}" \
        --sort-by=.metadata.creationTimestamp \
        -o jsonpath='{.items[-1].metadata.name}' 2>/dev/null || true)
    else
      pods=$(kubectl get pod -n "${ns}" -l "${selector}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    fi
    if [ -z "${pods}" ]; then
      row "${ns}" "${wlname} (${wlkind}, no pod)" "?" "${cpu_rec}" "?" "${mem_rec}" "no"
      continue
    fi

    while read -r pod; do
      [ -z "${pod}" ] && continue
      cpu_req=$(kubectl get pod -n "${ns}" "${pod}" \
        -o jsonpath='{.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo '?')
      mem_req=$(kubectl get pod -n "${ns}" "${pod}" \
        -o jsonpath='{.spec.containers[0].resources.requests.memory}' 2>/dev/null || echo '?')
      recycled=no
      if [ -n "${cpu_req}" ] && [ "${cpu_req}" != "${orig}" ]; then
        recycled=yes
      fi
      label="${pod}"
      [ "${wlkind}" != Deployment ] && label="${pod} (${wlkind})"
      row "${ns}" "${label}" "${cpu_req:-?}" "${cpu_rec}" "${mem_req:-?}" "${mem_rec}" "${recycled}"
    done <<<"${pods}"
  done < <(targets_for "${name}")
done
