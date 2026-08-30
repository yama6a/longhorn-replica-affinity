# Build on the builder's native arch and cross-compile to the target (buildx supplies
# TARGETOS/TARGETARCH), so the arm64 image is built without QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=go-mod go mod download
COPY . .

# Per-arch build cache id: the amd64 and arm64 builds run concurrently and would
# otherwise thrash a single shared mount. The module cache is arch-independent.
RUN --mount=type=cache,target=/root/.cache/go-build,id=go-build-$TARGETARCH \
    --mount=type=cache,target=/go/pkg/mod,id=go-mod \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o longhorn-replica-affinity ./cmd/longhorn-replica-affinity

FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=build /app/longhorn-replica-affinity /longhorn-replica-affinity
USER 65532:65532
EXPOSE 8443 9100
ENTRYPOINT ["/longhorn-replica-affinity"]
