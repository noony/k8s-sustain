BINARY ?= k8s-sustain
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest

.PHONY: build test lint generate manifests tidy

build:
	go build -o bin/$(BINARY) ./

test:
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy

generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

manifests:
	$(CONTROLLER_GEN) crd paths="./..." output:crd:artifacts:config=config/crd/bases
