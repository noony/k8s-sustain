# TLS with cert-manager

The admission webhook requires a valid TLS certificate. The recommended approach for production is to use [cert-manager](https://cert-manager.io) to issue and automatically rotate the certificate.

## Prerequisites

cert-manager must be installed in the cluster:

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set crds.enabled=true
```

## Default setup (self-signed)

The chart creates a self-signed `Issuer` and `Certificate` automatically. No external Issuer is required:

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.certManager.enabled=true
```

This is the simplest approach and works for both development and production. The webhook only needs to be trusted by the Kubernetes API server, and cert-manager handles CA bundle injection automatically.

## Using your own Issuer

If you already have an Issuer or ClusterIssuer in the cluster (e.g. a corporate CA), disable the built-in one and point to yours:

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.certManager.enabled=true \
  --set webhook.certManager.createIssuer=false \
  --set webhook.certManager.issuerRef.name=my-ca-issuer \
  --set webhook.certManager.issuerRef.kind=ClusterIssuer
```

!!! note "ACME / Let's Encrypt issuers"
    ACME issuers (e.g. Let's Encrypt) cannot issue certificates for internal Kubernetes service DNS names like `*.svc.cluster.local`. Use a self-signed or CA issuer instead.

## How it works

When `webhook.certManager.enabled=true`, the chart creates:

1. A self-signed `Issuer` (unless `createIssuer=false`)
2. A `Certificate` resource targeting the webhook service DNS names:
   ```
   k8s-sustain-webhook.<namespace>.svc
   k8s-sustain-webhook.<namespace>.svc.cluster.local
   ```
3. cert-manager issues the certificate and stores it in `webhook.tlsSecretName` (default: `k8s-sustain-webhook-tls`)
4. The `MutatingWebhookConfiguration` is annotated with `cert-manager.io/inject-ca-from`, so cert-manager automatically updates the `caBundle` when the certificate is renewed

## Manual certificate (without cert-manager)

Create a TLS secret manually and provide the base64-encoded CA certificate:

```bash
# Generate a self-signed cert (example only)
openssl req -x509 -newkey rsa:4096 -keyout tls.key -out tls.crt -days 365 -nodes \
  -subj "/CN=k8s-sustain-webhook.k8s-sustain.svc"

# Create the secret
kubectl create secret tls k8s-sustain-webhook-tls \
  --cert=tls.crt \
  --key=tls.key \
  -n k8s-sustain

# Base64-encode the CA cert (use -b 0 on macOS instead of -w0)
CA_BUNDLE=$(base64 -w0 tls.crt)

helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.caBundle="${CA_BUNDLE}"
```

!!! warning "Certificate rotation"
    Without cert-manager, you are responsible for rotating the certificate before it expires and updating `webhook.caBundle` via `helm upgrade`.
