FROM --platform=$BUILDPLATFORM node:26-alpine AS ui-builder
WORKDIR /workspace/internal/dashboard/ui/frontend
COPY internal/dashboard/ui/frontend/package.json internal/dashboard/ui/frontend/package-lock.json ./
RUN npm ci
COPY internal/dashboard/ui/frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /workspace/internal/dashboard/ui/dist internal/dashboard/ui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -a -trimpath -ldflags="-s -w" -o k8s-sustain .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/k8s-sustain .
USER 65532:65532
ENTRYPOINT ["/k8s-sustain"]
