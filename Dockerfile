# Build the manager binary. The builder always runs on the build machine's own
# platform and cross-compiles, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace
# Download dependencies first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o manager ./cmd

# Distroless: no shell, no package manager, runs as a non-root user.
FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/Rijngshaffen/webapp-operator" \
      org.opencontainers.image.description="Kubernetes operator that runs web applications from a WebApp resource" \
      org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
