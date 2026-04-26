# Development

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | ≥ 1.26 | Build and test |
| Docker | any | Build container image |
| kubectl | any | Cluster interaction |
| helm | ≥ 3.10 | Chart development |
| minikube / kind / k3d | any | Local cluster |

## Clone and build

```bash
git clone https://github.com/noony/k8s-sustain.git
cd k8s-sustain
go build ./...
```

## Run tests

```bash
go test ./...
```

## Project structure

```
k8s-sustain/
├── api/v1alpha1/          # CRD Go types and deepcopy
│   ├── policy_types.go
│   └── zz_generated.deepcopy.go
├── cmd/
│   ├── controller/        # Root cobra command + start subcommand
│   ├── webhook/           # webhook subcommand
│   └── dashboard/         # dashboard subcommand
├── internal/
│   ├── config/            # Centralized Viper config (flags, env, file)
│   ├── controller/        # Policy reconciler
│   ├── dashboard/         # Dashboard HTTP server
│   ├── logging/           # Shared zap logger setup
│   ├── prometheus/        # Prometheus HTTP API client
│   ├── recommender/       # Resource recommendation logic (pure functions)
│   ├── webhook/           # Admission webhook HTTP handler
│   └── workload/          # Pod recycler for Deployment/StatefulSet/DaemonSet
├── charts/k8s-sustain/    # Helm chart
├── docs/                  # This documentation
├── Dockerfile
├── Makefile
└── main.go
```

## Running locally against a cluster

### Start the controller

```bash
# Point KUBECONFIG at your cluster
export KUBECONFIG=~/.kube/config

# Forward Prometheus if needed
kubectl port-forward -n k8s-sustain svc/k8s-sustain-prometheus-server 9090:80 &

go run main.go start \
  --prometheus-address=http://localhost:9090 \  # port-forwarded from the cluster
  --reconcile-interval=1m \
  --log-level=debug
```

### Start the webhook (requires TLS)

The webhook must be reachable from the API server, which makes local development more involved. Use [telepresence](https://www.telepresence.io) or develop against a local kind cluster with a self-signed cert.

## Local end-to-end testing

The repository ships a `Makefile.scenarios` harness that brings up a kind
cluster, installs cert-manager and metrics-server, builds & loads the
image, helm-installs k8s-sustain, and runs a small library of synthetic
workload scenarios designed to exercise `Ongoing`-mode recycling
end-to-end. The scenario targets refuse to run unless the current kubectl
context is `kind-k8s-sustain`; bypass with `SKIP_CONTEXT_CHECK=1`.

```bash
make test-kind-up                       # one-shot cluster + helm install
make test-scenario-steady               # apply a scenario
make test-scenario-status               # current vs. recommended table
make test-scenario-clean                # delete every scenario namespace
make test-kind-down                     # delete the kind cluster
```

See [`docs/guides/local-testing.md`](guides/local-testing.md) for the
scenario catalog and the expected outcomes.

The remainder of this section ("Deploying on kind") describes the
manual equivalent, which is useful when you want to understand exactly
what the harness does or deviate from it.

## Deploying on kind

A full local deployment with Prometheus, the controller, webhook, and dashboard:

### 1. Create a kind cluster

```bash
kind create cluster --name k8s-sustain
```

### 2. Build and load the image

```bash
make docker-build IMG=k8s-sustain:dev
kind load docker-image k8s-sustain:dev --name k8s-sustain
```

### 3. Install with Helm

```bash
helm install k8s-sustain ./charts/k8s-sustain \
  --set image.repository=k8s-sustain \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --set dashboard.enabled=true
```

`image.pullPolicy=Never` ensures Kubernetes uses the locally loaded image. The `prometheusAddress` is auto-resolved to the bundled prometheus subchart service.

### 4. Verify pods are running

```bash
kubectl get pods -w
```

### 5. Access the dashboard

```bash
kubectl port-forward svc/k8s-sustain-dashboard 8090:8090
```

Open `http://localhost:8090`.

### 6. Create a test policy

```bash
kubectl apply -f - <<'EOF'
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: test-policy
spec:
  selector:
    namespaces: [default]
  rightSizing:
    update:
      types:
        deployment: Ongoing
    resourcesConfigs:
      cpu:
        window: 168h
        requests:
          percentile: 95
      memory:
        window: 168h
        requests:
          percentile: 95
EOF
```

### Rebuilding after changes

After modifying code, rebuild and reload:

```bash
make docker-build IMG=k8s-sustain:dev
kind load docker-image k8s-sustain:dev --name k8s-sustain
kubectl rollout restart deployment k8s-sustain
kubectl rollout restart deployment k8s-sustain-dashboard
```

### Cleanup

```bash
kind delete cluster --name k8s-sustain
```

## Regenerating code

If you modify types in `api/v1alpha1/`, regenerate the deepcopy methods:

```bash
make generate
```

This requires `controller-gen` to be installed:

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
```

## Building the container image

```bash
docker build -t ghcr.io/noony/k8s-sustain:dev .
```

The Dockerfile uses a two-stage build: `golang:1.26-alpine` → `gcr.io/distroless/static:nonroot`.

## Makefile targets

| Target | Description |
|--------|-------------|
| `make build` | Build the binary |
| `make test` | Run unit tests |
| `make generate` | Regenerate deepcopy code |
| `make docker-build` | Build the container image |
| `make helm-lint` | Lint the Helm chart |
| `make helm-template` | Render templates to stdout |

## Adding a new workload kind

To support a new workload kind (e.g. `Rollout` from Argo):

1. Add `ArgoRollout *UpdateMode` to `UpdateTypes` in `api/v1alpha1/policy_types.go` (already present as a placeholder)
2. Add the deepcopy block to `zz_generated.deepcopy.go`
3. Add `RecycleRolloutPods` to `internal/workload/patcher.go`
4. Add `reconcileRollouts` to `internal/controller/policy_controller.go`
5. Add the case to `modeForKind` and `resolveOwner` in `internal/webhook/handler.go`
6. Add RBAC markers (`+kubebuilder:rbac:...`) to the controller
7. Add the Helm RBAC rule in `charts/k8s-sustain/templates/rbac.yaml`

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/my-feature`
3. Commit with a clear message
4. Open a pull request against `main`

Please ensure `go build ./...` and `go test ./...` pass before opening a PR.
