# 6. The owner-name override is read from the annotation

- **Status:** Accepted
- **Date:** 2026-10-30
- **Refines:** [ADR 0004](0004-governance-is-decided-on-the-pod-template.md) — `k8s.sustain.io/owner-name` is no longer a label k8s-sustain adds to pods, so a Policy's selector may name it

## Context

A pod declares the identity it reports into with the `k8s.sustain.io/owner-name` annotation, on its pod template or, for a bare pod, on itself. Two parties read it, and they must agree or an identity's Recommendation is computed from usage that is not its own: `governance.Decide`, through `workload.ApplyOwnerNameOverride`, which groups the pod for the controller, the dashboard, the webhook and the OOM watcher; and the `k8s_sustain:pod_workload` recording rules, which aggregate the pod's usage under the same identity.

The recording rules could not read an annotation: the bundled kube-state-metrics exported allowlisted labels only. So the webhook copied a valid annotation onto a pod label of the same name on every admission, governed or not, and the rules read it from `kube_pod_labels{label_k8s_sustain_io_owner_name}`. Nothing else read the label. That made the grouping depend on the webhook, which it was never meant to:

- The webhook fails open. A pod admitted during a webhook timeout or outage, or created before k8s-sustain was installed, carried the annotation but not the label. The controller grouped it under the override identity while Prometheus mapped it to its real owner, or not at all for a bare pod, so its usage silently dropped out of the identity's Recommendation, and nothing reported it.
- The webhook mutated pods it does not govern: pods that opt out, that no Policy selects, and pods in `--excluded-namespaces` whenever the `MutatingWebhookConfiguration` still sent them.
- It forced workarounds. The label was on pods but never on pod templates, so the controller and the webhook saw different labels, and the Policy CRD had to reject `k8s.sustain.io/owner-name` as a selector key along with the labels controllers add to pods.
- It copied metadata into the selection space: a grouping request became a label any selector could match.

kube-state-metrics exports allowlisted annotations as `kube_pod_annotations{annotation_<key>}` (`--metric-annotations-allowlist`, since v2.2; the bundled subchart runs 2.18).

## Decision

**The recording rules read the owner-name override from the annotation, through `kube_pod_annotations`, and the webhook no longer writes it to a label.**

- The bundled kube-state-metrics exports `pods=[k8s.sustain.io/owner-name]` (`metricAnnotationsAllowList`), and the bundled Prometheus keeps `kube_pod_annotations` instead of `kube_pod_labels`, which nothing reads any more.
- The rules apply the override only to a value that is a valid, non-empty Kubernetes label value, the rule `workload.ApplyOwnerNameOverride` applies: every rule selects the annotation with `annotation_k8s_sustain_io_owner_name=~"[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?"`, which PromQL anchors at both ends. A pod with any other value keeps its real owner on both sides. `hack/signalcontract` fails when the rules' regexes differ, when they stop matching exactly the values the controller accepts, or when the bundled kube-state-metrics stops exporting the annotation.
- The rules give an overridden pod the kind the controller gives it: the kind `workload.ResolveControllerOwner` resolves its controller to (the Deployment or Rollout of its ReplicaSet, the CronJob of its Job, otherwise the controller's own kind, whatever it is, such as an orphan ReplicaSet, a ReplicationController or a custom controller), and `Pod` only for a pod without a controller. A pod of any other kind mapped as `Pod` would add its usage to a bare-pod identity of the same name. `hack/signalcontract` fails when the rules give a pod without a controller another kind than the controller does; a promtool fixture pins the kind of each other case.
- The webhook patches a pod only when it injects resources, and then only container resources. It still allows every pod and still recovers a panic past decoding, so that pod is admitted unmutated rather than left to the `failurePolicy`.
- The Policy CRD accepts `k8s.sustain.io/owner-name` as a selector key: it is a label only where the user sets one, on the pod template and its pods alike.

Two alternatives were rejected:

- The controller exporting its own pod → identity series from the inventory. The mapping would be lost while the controller is down, it needs a pod watch the controller does not otherwise keep, and only the leader may export it, so the rules would read a gap at every failover.
- Grouping by an existing label the Policy names. The kube-state-metrics allowlist would have to follow whatever label any Policy names, which the operator does not control with a bring-your-own Prometheus, and a missing entry would fail as silently as the missing mirror did.

## Consequences

- The grouping no longer depends on the webhook having seen a pod: a pod admitted while the webhook was unreachable, or before k8s-sustain was installed, is aggregated under the identity the controller groups it under.
- The webhook mutates only the pods it injects. A pod that opts out, that no Policy governs or that sits in an excluded namespace is admitted exactly as submitted.
- Breaking, for a bring-your-own Prometheus: kube-state-metrics must run with `--metric-annotations-allowlist=pods=[k8s.sustain.io/owner-name]` and `kube_pod_annotations` must be scraped. Without them, a pod using the override is mapped to its real owner and a bare pod not at all, so the identity gets partial or no usage data. The `kube_pod_labels` allowlist and keep-list entries can go. The bundled Prometheus is configured by the chart.
- Breaking: pods admitted by an earlier release keep their `k8s.sustain.io/owner-name` label until they are recreated, and new pods never get one. Nothing in k8s-sustain reads it; a selector written against it stops matching new pods.
- The regex is written six times in the chart's recording rules (four guards and the two branches of the override rule); the signal contract test keeps them identical and equal to the controller's rule.
