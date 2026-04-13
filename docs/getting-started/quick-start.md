# Quick Start

This guide creates a policy that right-sizes Deployments in a `staging` namespace using the p95 of the last 7 days of data.

## 1. Install k8s-sustain

```bash
helm repo add k8s-sustain https://noony.github.io/k8s-sustain
helm repo update
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace
```

## 2. Create a Policy

```yaml title="staging-policy.yaml"
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: staging-rightsizing
spec:
  update:
    types:
      deployment: Ongoing       # controller patches workload templates periodically
  rightSizing:
    resourcesConfigs:
      cpu:
        window: 168h            # 7-day lookback
        requests:
          percentilePercentage: 95
          headroomPercentage: 10  # +10% safety buffer
        limits:
          keepLimitRequestRatio: true
      memory:
        window: 168h
        requests:
          percentilePercentage: 95
          headroomPercentage: 20
        limits:
          keepLimitRequestRatio: true
```

```bash
kubectl apply -f staging-policy.yaml
```

## 3. Opt in a Deployment

Add the annotation to the pod template of any Deployment you want right-sized:

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
          image: my-app:latest
```

1. This annotation tells k8s-sustain which policy governs this workload.

## 4. Wait for data

!!! info "Cold start"
    Recording rules need at least one evaluation cycle (~1 minute) before data is available.
    For meaningful percentile recommendations, allow data to accumulate for at least a few hours.
    The operator logs `no metrics yet, skipping` for workloads with no data yet.

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
      message: All targeted workloads have been processed.
```

## 6. Verify resource changes

```bash
kubectl get deployment my-app -n staging \
  -o jsonpath='{.spec.template.spec.containers[*].resources}'
```

The controller reconciles on a fixed `1h` interval by default. To see changes sooner during testing, run the controller locally with `--reconcile-interval=5m` (see [CLI Reference](../reference/cli.md)).

## Next steps

- Use **OnCreate** mode to inject resources at pod creation without restarting existing pods → [Update Modes](../concepts/update-modes.md)
- Enable **in-place updates** for zero-restart resource changes on k8s ≥ 1.27 → [In-Place Updates](../concepts/in-place-updates.md)
- Right-size **CronJobs** → [CronJob guide](../guides/cronjobs.md)
