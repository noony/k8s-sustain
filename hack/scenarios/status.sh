#!/usr/bin/env bash
# Print a compact table of current vs. recommended resource requests for every
# active scenario. Sources:
#   - current requests:   kubectl get deploy <name> -n scenario-<name>
#   - recommendations:    GET /api/workloads/<ns>/Deployment/<name>/recommendations
#                         on the k8s-sustain dashboard service (via kubectl proxy)
#   - recycled:           heuristic — pod creationTimestamp > scenario apply time
#                         AND current cpu request differs from the value baked
#                         into hack/scenarios/<name>.yaml (i.e. the request has
#                         already been changed by the controller).
set -euo pipefail

SCENARIOS=(steady overprovisioned underprovisioned stepped hpa hpa-coordinated hpa-replica-anchor cronjob cronjob-long-running cronjob-overprovisioned job)
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

for name in "${SCENARIOS[@]}"; do
  ns="scenario-${name}"
  if ! kubectl get ns "${ns}" >/dev/null 2>&1; then continue; fi

  # CronJob and standalone Job scenarios: read from the latest run's pod.
  # The CronJob/Job spec is intentionally never modified — the "applied"
  # column tells you whether the webhook (and, for cronjob-long-running, the
  # in-place resize path) has already moved the pod off its template defaults.
  case "${name}" in
    cronjob)                  wlkind=CronJob; wlname=job ;;
    cronjob-long-running)     wlkind=CronJob; wlname=long-job ;;
    cronjob-overprovisioned)  wlkind=CronJob; wlname=overprovisioned ;;
    job)                      wlkind=Job;     wlname=oneshot ;;
    *)                        wlkind=""; wlname="" ;;
  esac
  if [ -n "${wlkind}" ]; then
    pod=$(kubectl get pod -n "${ns}" -l app=stress \
      --sort-by=.metadata.creationTimestamp \
      -o jsonpath='{.items[-1].metadata.name}' 2>/dev/null || true)
    if [ -z "${pod}" ]; then
      printf '%-28s %-22s %-9s %-9s %-9s %-9s %-8s\n' \
        "${ns}" "${wlname} (${wlkind} — no pod yet)" "?" "?" "?" "?" "no"
      continue
    fi
    cpu_req=$(kubectl get pod -n "${ns}" "${pod}" \
      -o jsonpath='{.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo '?')
    mem_req=$(kubectl get pod -n "${ns}" "${pod}" \
      -o jsonpath='{.spec.containers[0].resources.requests.memory}' 2>/dev/null || echo '?')
    rec_json=$(curl -fsS "${BASE}/api/workloads/${ns}/${wlkind}/${wlname}/recommendations" 2>/dev/null || echo '{}')
    cpu_rec=$(echo "${rec_json}" | grep -o '"cpuRequest":"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)
    mem_rec=$(echo "${rec_json}" | grep -o '"memoryRequest":"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)
    cpu_rec=${cpu_rec:-?}
    mem_rec=${mem_rec:-?}
    orig=$(grep -A2 'requests:' "$(dirname "$0")/${name}.yaml" | awk '/cpu:/ {print $2; exit}')
    applied=no
    if [ "${cpu_req}" != "${orig}" ] && [ -n "${cpu_req}" ]; then
      applied=yes
    fi
    printf '%-28s %-22s %-9s %-9s %-9s %-9s %-8s\n' \
      "${ns}" "${pod} (${wlkind})" "${cpu_req:-?}" "${cpu_rec}" "${mem_req:-?}" "${mem_rec}" "${applied}"
    continue
  fi

  pods=$(kubectl get pod -n "${ns}" -l app=stress \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  if [ -z "${pods}" ]; then continue; fi

  rec_json=$(curl -fsS "${BASE}/api/workloads/${ns}/Deployment/stress/recommendations" 2>/dev/null || echo '{}')
  cpu_rec=$(echo "${rec_json}" | grep -o '"cpuRequest":"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)
  mem_rec=$(echo "${rec_json}" | grep -o '"memoryRequest":"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)
  cpu_rec=${cpu_rec:-?}
  mem_rec=${mem_rec:-?}

  while read -r pod; do
    [ -z "${pod}" ] && continue
    cpu_req=$(kubectl get pod -n "${ns}" "${pod}" \
      -o jsonpath='{.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo '?')
    mem_req=$(kubectl get pod -n "${ns}" "${pod}" \
      -o jsonpath='{.spec.containers[0].resources.requests.memory}' 2>/dev/null || echo '?')

    # Recycled heuristic: original CPU request comes from the scenario YAML;
    # if the running pod's request differs, the controller has already acted.
    orig=$(grep -A2 'requests:' "$(dirname "$0")/${name}.yaml" | \
      awk '/cpu:/ {print $2; exit}')
    recycled=no
    if [ "${cpu_req}" != "${orig}" ] && [ -n "${cpu_req}" ]; then
      recycled=yes
    fi

    printf '%-28s %-22s %-9s %-9s %-9s %-9s %-8s\n' \
      "${ns}" "${pod}" "${cpu_req:-?}" "${cpu_rec}" "${mem_req:-?}" "${mem_rec}" "${recycled}"
  done <<<"${pods}"
done
