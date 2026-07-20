FROM --platform=$BUILDPLATFORM node:26-alpine AS ui-builder
WORKDIR /workspace/internal/dashboard/ui/frontend
COPY internal/dashboard/ui/frontend/package.json internal/dashboard/ui/frontend/package-lock.json ./
RUN npm ci
COPY internal/dashboard/ui/frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
# VERSION is embedded into the binary so `k8s-sustain version` and the startup
# log report the release tag. Defaults to "dev" for local builds.
ARG VERSION=dev
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /workspace/internal/dashboard/ui/dist internal/dashboard/ui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -a -trimpath \
    -ldflags="-s -w -X github.com/noony/k8s-sustain/internal/version.Version=${VERSION}" \
    -o k8s-sustain .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/k8s-sustain .
USER 65532:65532
ENTRYPOINT ["/k8s-sustain"]
