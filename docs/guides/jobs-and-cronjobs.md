# Jobs and CronJobs

k8s-sustain right-sizes both standalone `Job`s and scheduled `CronJob`s. Because each run creates a fresh pod, `OnCreate` mode is the natural fit for both kinds.

## Standalone Jobs

A standalone `Job` (not created by a CronJob) runs once. The webhook resolves `Pod → Job` directly and injects resources at admission. `OnCreate` is the typical mode; `Ongoing` is allowed but rarely useful for short-lived jobs.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: example-batch
  namespace: example
spec:
  template:
    metadata:
      annotations:
        k8s.sustain.io/policy: batch-rightsizing
    spec:
      restartPolicy: Never
      containers:
        - name: worker
          image: busybox:1.36
          command: ["sh", "-c", "sleep 30"]
          resources:
            requests: { cpu: 100m, memory: 64Mi }
```

The matching policy snippet:

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: batch-rightsizing
spec:
  rightSizing:
    update:
      types:
        job: OnCreate
        cronJob: OnCreate
```

Right-sizing a standalone Job is only meaningful when the same job type runs repeatedly enough to build a percentile history.

## CronJobs

CronJobs spawn ephemeral pods on a schedule. Because each run creates a fresh pod, `OnCreate` mode is a natural fit — the webhook injects recommendations at the start of every run.

### Owner chain

The webhook resolves the full owner chain:

```
Pod → Job → CronJob
```

When a pod annotated with `k8s.sustain.io/policy` is created by a Job owned by a CronJob, the webhook looks up the CronJob and checks its mode.

### OnCreate mode (recommended for CronJobs)

Each job pod receives the current recommendation at creation time. No restarts, no rollouts — just fresh pods with accurate resources on every schedule tick.

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: batch-rightsizing
spec:
  rightSizing:
    update:
      types:
        cronJob: OnCreate
    resourcesConfigs:
      cpu:
        window: 336h          # 14 days — more history for irregular jobs
        requests:
          percentile: 90
          headroom: 10
        limits:
          equalsToRequest: true   # Guaranteed QoS for batch jobs
      memory:
        window: 336h
        requests:
          percentile: 95
          headroom: 15
        limits:
          equalsToRequest: true
```

Opt in your CronJob:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: nightly-report
  namespace: production
spec:
  schedule: "0 2 * * *"
  jobTemplate:
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: batch-rightsizing  # (1)!
        spec:
          restartPolicy: OnFailure
          containers:
            - name: report
              image: my-report:latest
```

1. The annotation must be in `spec.jobTemplate.spec.template.metadata.annotations`.

### Ongoing mode for CronJobs

`Ongoing` mode updates the CronJob's job template so **future runs** use updated resources. It does not affect currently running job pods (those are ephemeral and will finish normally).

This is useful when you want the controller to keep the template current without relying on the webhook:

```yaml
spec:
  rightSizing:
    update:
      types:
        cronJob: Ongoing
```

### Collecting enough history

CronJobs that run infrequently (e.g. weekly) may not have enough data for a meaningful percentile. Use a longer window:

```yaml
resourcesConfigs:
  cpu:
    window: 720h   # 30 days
```

If fewer than ~10 data points exist in the window, the operator logs `no metrics yet, skipping` and leaves resources unchanged.

### Guaranteed QoS for batch jobs

Setting `equalsToRequest: true` for both CPU and memory limits makes the pod a [Guaranteed QoS class](https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/#guaranteed), which prevents throttling and OOM eviction under memory pressure. This is often desirable for batch workloads.
