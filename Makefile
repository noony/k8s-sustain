BINARY ?= k8s-sustain
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest
IMG ?= ghcr.io/noony/k8s-sustain:dev

.PHONY: build test lint generate manifests tidy fmt vet coverage docker-build docker-push helm-lint helm-template

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

manifests:
	$(CONTROLLER_GEN) crd paths="./..." output:crd:artifacts:config=config/crd/bases

docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

helm-lint:
	helm lint charts/k8s-sustain

helm-template:
	helm template k8s-sustain charts/k8s-sustain
