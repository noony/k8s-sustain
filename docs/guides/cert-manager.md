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
  --set installCRDs=true
```

## Using a ClusterIssuer

### Self-signed (development)

```bash
kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
EOF
```

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.certManager.enabled=true \
  --set webhook.certManager.issuerRef.name=selfsigned-issuer \
  --set webhook.certManager.issuerRef.kind=ClusterIssuer
```

### Let's Encrypt (production)

```bash
kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: your-email@example.com
    privateKeySecretRef:
      name: letsencrypt-prod
    solvers:
      - http01:
          ingress:
            class: nginx
EOF
```

```bash
helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.certManager.enabled=true \
  --set webhook.certManager.issuerRef.name=letsencrypt-prod \
  --set webhook.certManager.issuerRef.kind=ClusterIssuer
```

## How it works

When `webhook.certManager.enabled=true`, the chart creates:

1. A `Certificate` resource targeting the webhook service DNS names:
   ```
   k8s-sustain-webhook.<namespace>.svc
   k8s-sustain-webhook.<namespace>.svc.cluster.local
   ```
2. cert-manager issues the certificate and stores it in `webhook.tlsSecretName` (default: `k8s-sustain-webhook-tls`)
3. The `MutatingWebhookConfiguration` is annotated with `cert-manager.io/inject-ca-from`, so cert-manager automatically updates the `caBundle` when the certificate is renewed

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

# Base64-encode the CA cert
CA_BUNDLE=$(base64 -w0 tls.crt)

helm install k8s-sustain k8s-sustain/k8s-sustain \
  --namespace k8s-sustain \
  --create-namespace \
  --set webhook.caBundle="${CA_BUNDLE}"
```

!!! warning "Certificate rotation"
    Without cert-manager, you are responsible for rotating the certificate before it expires and updating `webhook.caBundle` via `helm upgrade`.
