# Quick Start

This guide creates a policy that right-sizes opted-in Deployments using the p95 of the last 7 days of data. The Policy has no `selector`, so it can govern opted-in Deployments in any namespace.

## 1. Install k8s-sustain

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace
```

This assumes [cert-manager](https://cert-manager.io/) is installed. See [Installation](installation.md) for other options (existing Prometheus, manual TLS, recommend-only mode).

## 2. Create a Policy

```yaml title="staging-policy.yaml"
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: staging-rightsizing
spec:
  rightSizing:
    update:
      types:
        deployment: Ongoing     # controller recycles stale pods; webhook injects resources
    resourcesConfigs:
      cpu:
        window: 168h            # 7-day lookback
        requests:
          percentile: 95
          headroom: 10  # +10% safety buffer
        limits:
          keepLimitRequestRatio: true
      memory:
        window: 168h
        requests:
          percentile: 95
          headroom: 20
        limits:
          keepLimitRequestRatio: true
```

```bash
kubectl apply -f staging-policy.yaml
```

## 3. Opt in a Deployment

Add the annotation to the pod template of any Deployment you want right-sized. It is also honoured on the Deployment's own `metadata.annotations` or on its Namespace, and `k8s.sustain.io/opt-out: "true"` excludes a single workload — see [resolution order](../reference/annotation.md#resolution-order).

```bash
kubectl patch deployment my-app -n staging \
  --type=json \
  -p='[{"op":"add","path":"/spec/template/metadata/annotations","value":{"k8s.sustain.io/policy":"staging-rightsizing"}}]'
```

Or add it directly in the Deployment manifest:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: staging
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
      annotations:
        k8s.sustain.io/policy: staging-rightsizing  # (1)!
    spec:
      containers:
        - name: app
          image: nginx:1.27
```

1. This annotation tells k8s-sustain which policy governs this workload.

## 4. Wait for data

!!! note "Cold start"
    Recommendations need a workload that is at least 10 minutes old and a few hours of Prometheus history to be meaningful; until then its `WorkloadRecommendation` exists but its `status` stays empty or reads `outcome: TooYoung` or `outcome: NoData`. See [Cold start](../concepts/workload-recommendations.md#cold-start-stub-recommendations).

## 5. Check the Policy status

```bash
kubectl get policy staging-rightsizing -o yaml
```

Look for the `Ready` condition:

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: ReconciliationSucceeded
      message: All 3 workloads have been processed.
```

The count is the number of workload objects the policy processed this cycle,
plus any retained recommendation whose workload has since gone away.

## 6. Verify resource changes

The Deployment's pod template is never modified — recommendations are applied to pods. Check the cached recommendation and the running pods:

```bash
kubectl get wlrec -n staging
kubectl get pods -n staging -l app=my-app \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].resources}{"\n"}{end}'
```

The controller reconciles every `5m` by default (`--reconcile-interval`, Helm value `controller.reconcileInterval`). See [Verifying applied resources](../concepts/update-modes.md#verifying-applied-resources).

## Next steps

- Use **OnCreate** mode to inject resources at pod creation without restarting existing pods → [Update Modes](../concepts/update-modes.md)
- How running pods are resized **in place** (k8s ≥ 1.33) or evicted → [In-Place Updates](../concepts/in-place-updates.md)
- Right-size **Jobs and CronJobs** → [Jobs & CronJobs guide](../guides/jobs-and-cronjobs.md)
