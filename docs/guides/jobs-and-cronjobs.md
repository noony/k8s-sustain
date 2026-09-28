# Jobs and CronJobs

k8s-sustain right-sizes both standalone `Job`s and scheduled `CronJob`s. Every run creates a fresh pod, so the webhook injects the current recommendation at the start of each run.

## Policy

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

`job` applies to standalone Jobs; Jobs created by a CronJob are handled through their CronJob under `cronJob`.

## Opting in

For a CronJob, the pod-template annotation is two levels deep:

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

1. `spec.jobTemplate.spec.template.metadata.annotations`. The CronJob's own `metadata.annotations` and its Namespace also work — see [Resolution order](../reference/annotation.md#resolution-order).

A standalone Job is annotated on `spec.template.metadata.annotations`. The webhook resolves `Pod → Job` for a standalone Job and `Pod → Job → CronJob` for a scheduled one.

On the dashboard, standalone Jobs appear as kind `Job`; Jobs owned by a CronJob appear under their CronJob row.

## Ongoing mode

`Ongoing` additionally resizes **running** job pods in place through the `pods/resize` subresource. Job and CronJob pods are never evicted, and the Job/CronJob spec is never modified. In-place resize of `restartPolicy: Never`/`OnFailure` pods has its own Kubernetes version requirement — see [Version matrix](../concepts/in-place-updates.md#version-matrix) and [Kinds that are never evicted](../concepts/in-place-updates.md#kinds-that-are-never-evicted). When a resize is not possible, the running pod keeps its resources and the next run gets the recommendation from the webhook.

```yaml
spec:
  rightSizing:
    update:
      types:
        job: Ongoing
        cronJob: Ongoing
```

`Ongoing` is worthwhile for **long-running** runs (daily ETL, ML training, backfills, migrations), where it can correct a pod mid-run. For runs that finish within seconds it behaves like `OnCreate`, at the cost of one extra Job/Pod list per reconcile.

## Cold start

The first run of a new Job or CronJob is admitted with its template resources; the webhook records a stub `WorkloadRecommendation` so the controller starts computing the identity, and later runs are injected once a recommendation exists. See [Cold start](../concepts/workload-recommendations.md#cold-start-stub-recommendations) and keep `--recommendation-retention` above the gap between runs ([Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads)).

A Job whose **name changes every run** (a timestamp or hash suffix) is a new identity each time and never converges. Use a stable Job name, or run it under a CronJob. The signature is a `k8s_sustain_wlr_refresh_total{outcome="nodata"}` rate that never turns into `computed`, alongside a `WorkloadRecommendation` count that grows with every run.

## Collecting enough history

CronJobs that run infrequently (e.g. weekly) may not have enough data for a meaningful percentile. Use a longer window:

```yaml
resourcesConfigs:
  cpu:
    window: 720h   # 30 days
```

When the window holds no usable samples, no recommendation is written and resources stay unchanged until a later reconcile finds data.

## Guaranteed QoS for batch jobs

Setting `equalsToRequest: true` for both CPU and memory limits makes the pod [Guaranteed QoS](https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/#guaranteed), which makes the pod the last candidate for node-pressure eviction.

## OOM detection for one-shot pods

Job pods typically run with `restartPolicy: Never` (or `backoffLimit: 0`), so an OOM kill does not increment `kube_pod_container_status_restarts_total`. The `k8s_sustain:workload_oom_24h` rule therefore also flags any container whose `kube_pod_container_status_last_terminated_reason` was `OOMKilled` in the last 24h, so the OOM-driven memory floor and the dashboard's "OOM 24h" badge work for Jobs and CronJobs too.

This requires the failed pod to survive long enough for kube-state-metrics to scrape it. Pods removed by `failedJobsHistoryLimit` within ~30s of failing can slip through; keep `failedJobsHistoryLimit` above 0.
