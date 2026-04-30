# DaemonSets

k8s-sustain right-sizes DaemonSets the same way it handles Deployments. Because every node runs one pod, getting their resources right yields cluster-wide savings.

## Goal

Right-size a DaemonSet in `Ongoing` mode while respecting the DaemonSet's update strategy.

## Prerequisites

- A DaemonSet with a `k8s.sustain.io/policy` annotation on its pod template.
- A k8s-sustain `Policy` matching the workload.
- A Prometheus instance reachable from the controller.

## Walkthrough

### 1. Annotate the DaemonSet

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: fluentd
  namespace: logging
spec:
  selector: { matchLabels: { app: fluentd } }
  template:
    metadata:
      labels: { app: fluentd }
      annotations:
        k8s.sustain.io/policy: monitoring-rightsizing
    spec:
      tolerations:
        - operator: Exists
      containers:
        - name: fluentd
          image: fluent/fluentd:v1.16
          resources:
            requests: { cpu: 100m, memory: 200Mi }
            limits:   { memory: 400Mi }
```

### 2. Apply the Policy

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: monitoring-rightsizing
spec:
  rightSizing:
    update:
      types:
        daemonSet: Ongoing
    resourcesConfigs:
      cpu:
        window: 168h
        requests: { percentile: 99, headroom: 15 }
        limits:   { keepLimitRequestRatio: true }
      memory:
        window: 168h
        requests: { percentile: 99, headroom: 25 }
        limits:   { keepLimitRequestRatio: true }
```

## Verification

```bash
kubectl get pods -n logging -l app=fluentd \
  -o yaml | yq '.items[].spec.containers[].resources'
```

All node pods carry the recommended values; the DaemonSet's pod template is unchanged.

## Notes

- **Higher percentile for node-critical agents.** Log shippers, CNI plugins, and node exporters cannot be OOM-killed without disturbing the node. Use p99 with generous memory headroom.
- **`updateStrategy: OnDelete`.** When a DaemonSet uses `OnDelete`, the controller cannot create replacement pods on its own. On Kubernetes 1.31+ k8s-sustain patches running pods in place and bypasses this constraint; on older versions, plan to delete pods manually after a recommendation lands so the DaemonSet controller creates fresh ones.
- **Tolerations.** A DaemonSet typically tolerates control-plane taints. The eviction fallback respects these tolerations so a recycle does not strand control-plane node pods.
- **`OnCreate` mode.** Less common for DaemonSets since their pods are long-lived. Useful during cluster bootstrap to size newly-added nodes' agents before the controller reconciles.
