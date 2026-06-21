BINARY ?= k8s-sustain
CONTROLLER_GEN ?= go tool controller-gen
IMG ?= ghcr.io/noony/k8s-sustain:dev
PLATFORMS ?= linux/amd64,linux/arm64

include Makefile.scenarios

.PHONY: help build test test-race lint generate manifests generate-crds verify-crds tidy fmt vet coverage docker-build docker-buildx docker-push helm-deps helm-lint helm-template helm-unittest port-forward port-forward-stop

NAMESPACE ?= k8s-sustain
DASHBOARD_PORT ?= 8090
PROMETHEUS_PORT ?= 9090

.DEFAULT_GOAL := help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build-ui: ## Build dashboard frontend
	cd internal/dashboard/ui/frontend && npm ci && npm run build

build: build-ui ## Build binary to bin/k8s-sustain
	go build -o bin/$(BINARY) ./

test: ## Run all tests
	go test -shuffle=on ./...

test-race: ## Run all tests with the race detector and randomized order
	go test -race -shuffle=on ./...

coverage: ## Run tests with coverage report (race detector + shuffled order)
	go test -race -shuffle=on -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint: ## Run golangci-lint
	golangci-lint run

tidy: ## Run go mod tidy
	go mod tidy

generate: ## Generate DeepCopy methods
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./..."

manifests: generate-crds ## Generate CRD manifests into the Helm chart

generate-crds:
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=charts/k8s-sustain/files/crds

verify-crds: generate-crds ## Verify generated CRDs are in sync with the Go types
	@git diff --exit-code charts/k8s-sustain/files/crds || \
		(echo "ERROR: CRDs in Helm chart are out of sync with Go types. Run 'make manifests' and commit." && exit 1)

docker-build: ## Build Docker image for the host's native platform
	DOCKER_BUILDKIT=1 docker build -t $(IMG) .

docker-buildx: ## Build and push a multi-arch image for $(PLATFORMS) (requires buildx)
	docker buildx build --platform $(PLATFORMS) -t $(IMG) .

docker-push: ## Push Docker image
	docker push $(IMG)

helm-deps: ## Fetch Helm chart dependencies
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
	helm dependency build charts/k8s-sustain

helm-lint: helm-deps ## Lint Helm charts
	helm lint charts/k8s-sustain
	helm lint charts/k8s-sustain-policies

helm-template: helm-deps ## Render Helm chart templates
	helm template k8s-sustain charts/k8s-sustain
	helm template k8s-sustain-policies charts/k8s-sustain-policies

helm-unittest: helm-deps ## Run Helm chart unit tests (requires the helm-unittest plugin)
	helm unittest charts/k8s-sustain
	helm unittest charts/k8s-sustain-policies

port-forward: port-forward-stop ## Port-forward dashboard ($(DASHBOARD_PORT)) and Prometheus ($(PROMETHEUS_PORT)) in the background
	@mkdir -p .port-forward
	@nohup kubectl port-forward -n $(NAMESPACE) svc/k8s-sustain-dashboard $(DASHBOARD_PORT):8090 \
		>.port-forward/dashboard.log 2>&1 & echo $$! > .port-forward/dashboard.pid
	@nohup kubectl port-forward -n $(NAMESPACE) svc/k8s-sustain-prometheus-server $(PROMETHEUS_PORT):80 \
		>.port-forward/prometheus.log 2>&1 & echo $$! > .port-forward/prometheus.pid
	@sleep 1
	@echo "Dashboard:  http://localhost:$(DASHBOARD_PORT)"
	@echo "Prometheus: http://localhost:$(PROMETHEUS_PORT)"
	@echo "Logs in .port-forward/, stop with 'make port-forward-stop'"

port-forward-stop: ## Stop background port-forwards started by 'make port-forward'
	-@pkill -f 'kubectl port-forward.*k8s-sustain-dashboard' 2>/dev/null || true
	-@pkill -f 'kubectl port-forward.*k8s-sustain-prometheus-server' 2>/dev/null || true
	@rm -f .port-forward/*.pid
