# Argo CD Integration

k8s-sustain **never patches workload specs** (Deployments, StatefulSets, etc.) — it only mutates Pods at admission time via the webhook and recycles stale pods via eviction or in-place patching. Because the workload spec in the cluster always matches what is stored in Git, **no `ignoreDifferences` configuration is needed**.

## Why no special configuration?

- The **webhook** intercepts `Pod CREATE` requests and injects resources via an admission response patch. This does not modify the workload spec that Argo CD tracks.
- The **controller** (Ongoing mode) recycles pods by evicting them or patching them in-place. It does not modify the Deployment, StatefulSet, DaemonSet, or CronJob objects.

Since Argo CD tracks workload specs (not running Pod specs), there is no drift to detect or ignore.

## Self-heal and sync policy

With this architecture, `selfHeal: true` works without issues — Argo CD will never detect a diff caused by k8s-sustain, so there is nothing to revert.
