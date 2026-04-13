# Installation

## Add the Helm repository

```bash
helm repo add k8s-sustain https://noony.github.io/k8s-sustain
helm repo update
```

## Install with bundled Prometheus

The default installation deploys the operator, the admission webhook, and a standalone Prometheus with the required recording rules pre-configured.

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace
```

## Install with an existing Prometheus

If you already have Prometheus running, disable the bundled instance and point the operator at yours:

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set prometheus.enabled=false \
  --set prometheus-node-exporter.enabled=false \
  --set manager.prometheusAddress=http://prometheus.monitoring.svc:9090 \
  --set webhook.prometheusAddress=http://prometheus.monitoring.svc:9090
```

!!! warning "Recording rules required"
    You must install the recording rules into your Prometheus manually when `prometheus.enabled=false`.
    Copy the rules from `charts/k8s-sustain/values.yaml` under `prometheus.serverFiles.recording_rules.yml`
    into a `PrometheusRule` CRD or your Prometheus configuration.

## Install without the admission webhook

If you only need `Ongoing` mode (no `OnCreate`), you can disable the webhook entirely. This removes the TLS certificate requirement.

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.enabled=false
```

## Install with cert-manager (recommended for production)

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.certManager.enabled=true \
  --set webhook.certManager.issuerRef.name=letsencrypt-prod \
  --set webhook.certManager.issuerRef.kind=ClusterIssuer
```

## Verify the installation

```bash
kubectl get pods -n k8s-sustain
```

Expected output:

```
NAME                                        READY   STATUS    RESTARTS   AGE
k8s-sustain-<hash>                          1/1     Running   0          1m
k8s-sustain-webhook-<hash>                  1/1     Running   0          1m
k8s-sustain-prometheus-server-<hash>        1/1     Running   0          1m
k8s-sustain-prometheus-node-exporter-<hash> 1/1     Running   0          1m
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

```bash
helm repo update
helm upgrade k8s-sustain k8s-sustain/k8s-sustain \
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
