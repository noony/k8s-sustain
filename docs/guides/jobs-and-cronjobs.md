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

```text
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
              image: busybox:1.36
```

1. The annotation must be in `spec.jobTemplate.spec.template.metadata.annotations`.

### Ongoing mode for CronJobs

`Ongoing` mode resizes **currently running** job pods in place using the Kubernetes `pods/resize` subresource (requires `InPlacePodVerticalScaling` — GA on k8s ≥ 1.33; works for `restartPolicy: Never`/`OnFailure` on k8s ≥ 1.35). The CronJob spec itself is **never modified**, so GitOps tools (Argo CD, Flux) see no drift. Future runs continue to pick up the latest resources from the webhook at admission.

```yaml
spec:
  rightSizing:
    update:
      types:
        cronJob: Ongoing
```

Practical implications:

- For CronJobs whose pods finish within seconds (e.g. `* * * * *` health pings), `Ongoing` is essentially equivalent to `OnCreate` — pods complete before any reconcile pass would touch them. The cost of running `Ongoing` is one extra Job/Pod list per reconcile.
- For long-running runs (daily ETL, batch training, hour-long backfills), `Ongoing` can correct an under- or over-provisioned pod mid-run without restarting the container.
- On clusters where `InPlacePodVerticalScaling` is unavailable or the kubelet reports the resize as `Infeasible`, the running pod is left alone (it would be destructive to evict a Job pod). The new resources still land on the next scheduled run via the webhook.
- The controller never patches the `CronJob` or `Job` object, so RBAC for `batch/cronjobs` and `batch/jobs` is read-only.

### Collecting enough history

CronJobs that run infrequently (e.g. weekly) may not have enough data for a meaningful percentile. Use a longer window:

```yaml
resourcesConfigs:
  cpu:
    window: 720h   # 30 days
```

If fewer than ~10 data points exist in the window, the controller logs `no metrics yet, skipping` and leaves resources unchanged.

### Guaranteed QoS for batch jobs

Setting `equalsToRequest: true` for both CPU and memory limits makes the pod a [Guaranteed QoS class](https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/#guaranteed), which prevents throttling and OOM eviction under memory pressure. This is often desirable for batch workloads.

### OOM detection for one-shot pods

CronJob/Job pods typically run with `restartPolicy: Never` (or `backoffLimit: 0`), so the kubelet does not restart them after an OOM kill — `kube_pod_container_status_restarts_total` stays at 0. The `k8s_sustain:workload_oom_24h` rule combines two paths so OOMs are still detected: a "kill" path uses `max_over_time(kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}[24h])` to flag any (pod, container) that OOMed, regardless of whether it was restarted.

This means the OOM-driven memory floor (in `policy_controller.go`) and dashboard "OOM 24h" badge work for CronJobs too — provided the failed pod survives long enough for kube-state-metrics to scrape it. Pods garbage-collected by `failedJobsHistoryLimit` within ~30s of failing can still slip through; raising `failedJobsHistoryLimit` above 0 helps.
