export function workloadPath(w: { namespace: string; kind: string; name: string }): string {
  return `/workloads/${w.namespace}/${w.kind}/${w.name}`
}
