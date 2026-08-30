# Quick Start

This guide creates a policy that right-sizes Deployments in a `staging` namespace using the p95 of the last 7 days of data.

## 1. Install k8s-sustain

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace
```

See [Installation](installation.md) for other install options (existing Prometheus, cert-manager, recommend-only mode).

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

Add the annotation to the pod template of any Deployment you want right-sized (it is also honoured on the Deployment's own `metadata.annotations` or its Namespace — see the [Annotation reference](../reference/annotation.md)):

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
  template:
    metadata:
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
    Recording rules need at least one evaluation cycle (~1 minute) before data is available.
    For meaningful percentile recommendations, allow data to accumulate for at least a few hours.
    A workload the controller has known for less than 10 minutes is held back by the
    workload-age gate, logged as `skipping recommendation: workload too young`. Its
    `WorkloadRecommendation` object still exists — discovery creates one per matched
    workload — but its `status` stays empty until there is something to put in it. An
    empty `status.containers` and an unset `status.source` are the expected early
    reading here, not a failure; the identity is recomputed on every reconcile
    interval and fills in once Prometheus has enough history.

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

```bash
kubectl get deployment my-app -n staging \
  -o jsonpath='{.spec.template.spec.containers[*].resources}'
```

The controller reconciles on a fixed `10m` interval by default. To see changes sooner during testing, run the controller locally with `--reconcile-interval=2m` (see [CLI Reference](../reference/cli.md)).

## Next steps

- Use **OnCreate** mode to inject resources at pod creation without restarting existing pods → [Update Modes](../concepts/update-modes.md)
- Enable **in-place updates** for zero-restart resource changes on k8s ≥ 1.33 → [In-Place Updates](../concepts/in-place-updates.md)
- Right-size **Jobs and CronJobs** → [Jobs & CronJobs guide](../guides/jobs-and-cronjobs.md)
