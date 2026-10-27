# Build on the builder's native arch and cross-compile to the target (buildx supplies
# TARGETOS/TARGETARCH), so the arm64 image is built without QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
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

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /app/longhorn-replica-affinity /longhorn-replica-affinity
USER 65532:65532
EXPOSE 8443 9100
ENTRYPOINT ["/longhorn-replica-affinity"]
