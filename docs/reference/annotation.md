# Annotation Reference

## `k8s.sustain.io/policy`

This annotation is the **only** way to opt a workload into a policy. It must be set on the workload's **pod template** so that pods inherit it automatically.

```yaml
metadata:
  annotations:
    k8s.sustain.io/policy: <policy-name>
```

The value is the name of a cluster-scoped `Policy` object.

---

## Placement by workload kind

=== "Deployment"

    ```yaml
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-app
      namespace: production
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: production-rightsizing
        spec:
          containers:
            - name: app
              image: my-app:latest
    ```

=== "StatefulSet"

    ```yaml
    apiVersion: apps/v1
    kind: StatefulSet
    metadata:
      name: my-db
      namespace: production
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: production-rightsizing
        spec:
          containers:
            - name: db
              image: postgres:15
    ```

=== "DaemonSet"

    ```yaml
    apiVersion: apps/v1
    kind: DaemonSet
    metadata:
      name: my-agent
      namespace: monitoring
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: monitoring-rightsizing
        spec:
          containers:
            - name: agent
              image: my-agent:latest
    ```

=== "CronJob"

    ```yaml
    apiVersion: batch/v1
    kind: CronJob
    metadata:
      name: my-job
      namespace: production
    spec:
      schedule: "0 * * * *"
      jobTemplate:
        spec:
          template:
            metadata:
              annotations:
                k8s.sustain.io/policy: production-rightsizing  # (1)!
            spec:
              containers:
                - name: worker
                  image: my-worker:latest
    ```

    1. Note: the annotation is two levels deep — inside `jobTemplate.spec.template`.

---

## Adding the annotation imperatively

```bash
# Deployment
kubectl patch deployment my-app -n production \
  --type=merge \
  -p='{"spec":{"template":{"metadata":{"annotations":{"k8s.sustain.io/policy":"production-rightsizing"}}}}}'

# CronJob
kubectl patch cronjob my-job -n production \
  --type=merge \
  -p='{"spec":{"jobTemplate":{"spec":{"template":{"metadata":{"annotations":{"k8s.sustain.io/policy":"production-rightsizing"}}}}}}}'
```

---

## Removing a workload from a policy

Delete the annotation from the pod template. The controller will stop reconciling the workload on the next interval; existing resources are not reverted.

```bash
kubectl annotate deployment my-app -n production \
  k8s.sustain.io/policy- \
  --overwrite
```

!!! note
    The `-` suffix on the annotation key tells `kubectl annotate` to remove it.

---

## How the annotation is consumed

| Component | Where it reads the annotation |
|-----------|-------------------------------|
| **Controller** | `workload.spec.template.metadata.annotations` (for Deployment/StatefulSet/DaemonSet) or `cronJob.spec.jobTemplate.spec.template.metadata.annotations` |
| **Webhook** | `pod.metadata.annotations` — pods inherit it from the pod template automatically |
