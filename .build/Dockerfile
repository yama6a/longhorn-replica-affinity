# Cross-compile from the builder's native arch, so arm64 needs no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
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

FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/longhorn-replica-affinity /longhorn-replica-affinity
USER 65532:65532
EXPOSE 8443 9100
ENTRYPOINT ["/longhorn-replica-affinity"]
