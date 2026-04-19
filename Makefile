BINARY ?= k8s-sustain
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest
IMG ?= ghcr.io/noony/k8s-sustain:dev

.PHONY: help build test lint generate manifests sync-crds verify-crds tidy fmt vet coverage docker-build docker-push helm-deps helm-lint helm-template

.DEFAULT_GOAL := help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## Build binary to bin/k8s-sustain
	go build -o bin/$(BINARY) ./

test: ## Run all tests
	go test ./...

coverage: ## Run tests with coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint: ## Run golangci-lint
	golangci-lint run

tidy: ## Run go mod tidy
	go mod tidy

generate: ## Generate DeepCopy methods
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

manifests: sync-crds ## Generate CRD manifests and sync to Helm chart

generate-crds:
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=config/crd/bases

sync-crds: generate-crds
	./hack/sync-crds.sh

verify-crds: sync-crds ## Verify Helm chart CRD is in sync with generated one
	@git diff --exit-code charts/k8s-sustain/templates/crd-policy.yaml || \
		(echo "ERROR: CRD in Helm chart is out of sync. Run 'make manifests' and commit." && exit 1)

docker-build: ## Build Docker image
	docker build -t $(IMG) .

docker-push: ## Push Docker image
	docker push $(IMG)

helm-deps: ## Fetch Helm chart dependencies
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
	helm dependency build charts/k8s-sustain

helm-lint: helm-deps ## Lint Helm chart
	helm lint charts/k8s-sustain

helm-template: helm-deps ## Render Helm chart templates
	helm template k8s-sustain charts/k8s-sustain
