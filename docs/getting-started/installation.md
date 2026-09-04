# Installation

Install k8s-sustain into a cluster with Helm, optionally using a bundled Prometheus or pointing at an existing one.

Charts are published as OCI artifacts to GitHub Container Registry — no `helm repo add` needed. Pick a version from the [releases page](https://github.com/noony/k8s-sustain/releases) and pin it with `--version`:

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace
```

!!! note "No version ranges"
    OCI registries don't support the caret/tilde ranges a classic Helm repo does — `--version` must be an exact chart version (e.g. `0.4.0`).

## Install with bundled Prometheus

The default installation deploys the controller, the admission webhook, and a [Prometheus](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus) instance with the required recording rules pre-configured.

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace
```

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
    When `prometheus.enabled=false`, you must install the recording rules manually.
    Copy the rule groups from `prometheus.server.serverFiles` in `values.yaml` into your existing Prometheus configuration.
    If you use the Prometheus Operator, enable `prometheusRule.enabled=true` to deploy the recording rules as a `PrometheusRule` resource, and `controller.serviceMonitor.enabled=true` for the controller metrics `ServiceMonitor`.

## Install without the admission webhook

If you only need `Ongoing` mode (no `OnCreate`), you can disable the webhook entirely. This removes the TLS certificate requirement.

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.enabled=false
```

## Install in recommend-only mode (dry-run)

Run k8s-sustain without applying any changes. Recommendations are logged as structured JSON but workloads and pods are never modified.

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace \
  --set recommendOnly=true
```

Once you are satisfied with the logged recommendations, disable recommend-only mode:

```bash
helm upgrade k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --reuse-values \
  --set recommendOnly=false
```

For a gradual rollout, you can also dry-run a single policy instead of the whole installation by setting `spec.rightSizing.recommendOnly: true` on that `Policy` — see the [Policy reference](../reference/policy.md#specrightsizingrecommendonly).

## Install with cert-manager (recommended for production)

The chart creates a self-signed Issuer and Certificate automatically — just enable cert-manager:

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.certManager.enabled=true
```

See the [cert-manager guide](../guides/cert-manager.md) for using your own Issuer.

## Verify the installation

```bash
kubectl get pods -n k8s-sustain
```

Expected output:

```text
NAME                                        READY   STATUS    RESTARTS   AGE
k8s-sustain-<hash>                          1/1     Running   0          1m
k8s-sustain-webhook-<hash>                  1/1     Running   0          1m
```

Check the controller logs:

```bash
kubectl logs -n k8s-sustain -l app.kubernetes.io/name=k8s-sustain -l app.kubernetes.io/component!=webhook
```

Check the webhook logs:

```bash
kubectl logs -n k8s-sustain -l app.kubernetes.io/component=webhook
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

!!! note "CRD retention"
    The `Policy` CRD is annotated with `helm.sh/resource-policy: keep` and will **not** be deleted on uninstall to protect existing Policy objects. Delete it manually if needed:
    ```bash
    kubectl delete crd policies.k8s.sustain.io
    ```
