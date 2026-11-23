# Build on the builder's native arch and cross-compile to the target (buildx supplies
# TARGETOS/TARGETARCH), so the arm64 image is built without QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=go-mod go mod download
COPY . .

# Per-arch cache id: the two builds run concurrently and would thrash a shared mount.
RUN --mount=type=cache,target=/root/.cache/go-build,id=go-build-$TARGETARCH \
    --mount=type=cache,target=/go/pkg/mod,id=go-mod \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o longhorn-replica-affinity ./cmd/longhorn-replica-affinity

FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /app/longhorn-replica-affinity /longhorn-replica-affinity
USER 65532:65532
EXPOSE 8443 9100
ENTRYPOINT ["/longhorn-replica-affinity"]
