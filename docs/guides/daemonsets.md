# DaemonSets

DaemonSets run one pod per node and are commonly used for monitoring agents, log shippers, and CNI plugins. Right-sizing them can yield significant cluster-wide savings since every node is affected.

## Policy example

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: monitoring-rightsizing
spec:
  update:
    types:
      daemonSet: Ongoing
  rightSizing:
    resourcesConfigs:
      cpu:
        window: 168h
        requests:
          percentile: 99   # use p99 for node-critical agents
          headroom: 15
        limits:
          keepLimitRequestRatio: true
      memory:
        window: 168h
        requests:
          percentile: 99
          headroom: 25
        limits:
          keepLimitRequestRatio: true
```

Opt in your DaemonSet:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: fluentd
  namespace: logging
spec:
  template:
    metadata:
      annotations:
        k8s.sustain.io/policy: monitoring-rightsizing
```

## Behaviour with Ongoing mode

DaemonSet `Ongoing` mode updates the pod template and triggers a DaemonSet rolling update (`updateStrategy.type: RollingUpdate`). This is DaemonSet's normal update path — one node at a time.

!!! warning "OnDelete strategy"
    If your DaemonSet uses `updateStrategy.type: OnDelete`, the template is updated but pods are only replaced when deleted manually. In-place updates (k8s ≥ 1.31) bypass this restriction and patch running pods directly.

## Node-critical agents: use higher percentiles

For agents that must not be OOM-killed, use **p99** and a generous headroom. A terminated log shipper or CNI plugin can cause node-level issues.

## OnCreate mode for DaemonSets

OnCreate mode for DaemonSets is less common since DaemonSet pods are typically long-running. However, it is useful during initial cluster setup to ensure new nodes get properly-sized agent pods.
