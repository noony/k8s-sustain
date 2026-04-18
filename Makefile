BINARY ?= k8s-sustain
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest
IMG ?= ghcr.io/noony/k8s-sustain:dev

.PHONY: build test lint generate manifests sync-crds verify-crds tidy fmt vet coverage docker-build docker-push helm-lint helm-template

build:
	go build -o bin/$(BINARY) ./

test:
	go test ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint:
	golangci-lint run

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

manifests: sync-crds

generate-crds:
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=config/crd/bases

# Wrap the generated CRD with Helm template directives and copy into the chart.
sync-crds: generate-crds
	./hack/sync-crds.sh

# CI check: fail if the chart CRD is out of sync with the generated one.
verify-crds: sync-crds
	@git diff --exit-code charts/k8s-sustain/templates/crd-policy.yaml || \
		(echo "ERROR: CRD in Helm chart is out of sync. Run 'make manifests' and commit." && exit 1)

docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

helm-lint:
	helm lint charts/k8s-sustain

helm-template:
	helm template k8s-sustain charts/k8s-sustain
