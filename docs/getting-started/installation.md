# Installation

Install k8s-sustain into a cluster with Helm, optionally using a bundled Prometheus or pointing at an existing one. Check the [prerequisites](prerequisites.md) first.

Charts are published as OCI artifacts to GitHub Container Registry — no `helm repo add` needed. Pick a version from the [releases page](https://github.com/noony/k8s-sustain/releases) and pin it with `--version`.

!!! note "No version ranges"
    OCI registries don't support the caret/tilde ranges a classic Helm repo does — `--version` must be an exact chart version (e.g. `0.4.0`).

!!! tip "Verify before you install"
    Images, charts and release binaries are signed with cosign in keyless mode, and images carry an SPDX SBOM and SLSA build provenance. See [Security](../security.md) for copy-pasteable `cosign verify` and `gh attestation verify` commands, and for pinning the image by digest.

## Install with bundled Prometheus

The default installation deploys the controller, the admission webhook, the dashboard, and a [Prometheus](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus) instance (with kube-state-metrics) with the required recording rules pre-configured.

The webhook needs a TLS certificate. By default the chart has [cert-manager](https://cert-manager.io/) issue it from a self-signed Issuer, so cert-manager must be installed in the cluster first:

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace
```

Without cert-manager, set `webhook.certManager.enabled=false`, create the TLS Secret yourself and pass `webhook.tlsSecretName` and `webhook.caBundle` instead — see [TLS certificate](prerequisites.md#tls-certificate) and the [cert-manager guide](../guides/cert-manager.md) (which also covers using your own Issuer).

## Install with an existing Prometheus

If you already have Prometheus running, disable the bundled instance and point k8s-sustain at yours:

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace \
  --set prometheus.enabled=false \
  --set prometheusAddress=http://prometheus.monitoring.svc:80
```

If that Prometheus requires authentication — a bearer token, basic auth, a
private CA, or a Thanos/Mimir/Cortex tenant header — configure the top-level
`prometheusAuth` block as well. See the
[Authenticated Prometheus guide](../guides/authenticated-prometheus.md) for
copy-pasteable values, and the
[Helm values reference](../reference/helm-values.md#prometheus-authentication)
for the full schema.

!!! warning "Recording rules required"
    When `prometheus.enabled=false`, you must install the recording rules yourself. They are defined once, under `prometheusRule.groups` in the chart's `values.yaml`.
    If you use the Prometheus Operator, set `prometheusRule.enabled=true` to deploy them as a `PrometheusRule` resource, and `controller.serviceMonitor.enabled=true` for the controller metrics `ServiceMonitor`. Otherwise copy the groups into your Prometheus rule files. Your Prometheus must also scrape the metrics listed in [Prerequisites](prerequisites.md#prometheus).

## Install in recommend-only mode (dry-run)

Run k8s-sustain without applying any changes: recommendations are computed and cached in `WorkloadRecommendation` objects, but pods are never resized, evicted or injected.

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace \
  --set recommendOnly=true
```

Review the results with `kubectl get wlrec -A` or the dashboard. The controller also logs each recommendation at `info` level, which the chart's default `controller.logLevel: error` suppresses — add `--set controller.logLevel=info` to see them.

Once you are satisfied, disable recommend-only mode:

```bash
helm upgrade k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --reuse-values \
  --set recommendOnly=false
```

To dry-run a single policy instead of the whole installation, see [Recommend-only mode](../concepts/update-modes.md#recommend-only-mode).

## The admission webhook is required

`webhook.enabled=false` exists, but k8s-sustain does not work correctly without the webhook, in either mode:

- `OnCreate` does nothing at all.
- `Ongoing` still needs it: pods the controller evicts (always on k8s < 1.33, and as a fallback on newer clusters) are recreated from the unchanged template, so the replacement starts on the old resources and is evicted again on the next reconcile.
- Jobs, CronJob runs and bare pods only get the recommendation at creation, so they are never sized.

## Verify the installation

```bash
kubectl get pods -n k8s-sustain
```

With the default values you should see:

```text
NAME                                             READY   STATUS    RESTARTS   AGE
k8s-sustain-<hash>                               1/1     Running   0          1m
k8s-sustain-webhook-<hash>                       1/1     Running   0          1m
k8s-sustain-dashboard-<hash>                     1/1     Running   0          1m
k8s-sustain-prometheus-server-<hash>             2/2     Running   0          1m
k8s-sustain-kube-state-metrics-<hash>            1/1     Running   0          1m
```

The last two are absent with `prometheus.enabled=false`, and the dashboard with `dashboard.enabled=false`.

Check the controller and webhook logs:

```bash
kubectl logs -n k8s-sustain deploy/k8s-sustain
kubectl logs -n k8s-sustain deploy/k8s-sustain-webhook
```

## Upgrading

Bump `--version` to the new release and re-run `helm upgrade`:

```bash
helm upgrade k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --reuse-values
```

## Uninstalling

```bash
helm uninstall k8s-sustain -n k8s-sustain
```

!!! warning "Uninstall deletes the CRDs and every Policy"
    The chart installs the `Policy` and `WorkloadRecommendation` CRDs as regular templates (`installCRDs: true`), so `helm uninstall` deletes both CRDs — and with them every `Policy` and `WorkloadRecommendation` in the cluster. Back up your Policies first (`kubectl get policies -o yaml > policies.yaml`), or manage them with the [policies chart](../guides/managing-policies-with-helm.md) or GitOps.
