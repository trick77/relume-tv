# Build: static Go binary, cross-compiled on the build platform (native on the amd64 runner, no QEMU for the
# arm64 image's compile).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# go:embed cannot reach outside the module: the UI lands in its embed dir here.
COPY ui/ ./web/dist/
# VERSION: ship release passes the release version. TARGETOS/TARGETARCH: set by buildx per platform.
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/trick77/ship/kit/version.Version=${VERSION}" -o /relume-tv ./cmd/relume-tv

# Runtime: distroless static, as root (ship.yaml allow): host networking binds :80 and existing installs keep
# a root-owned /data volume.
FROM gcr.io/distroless/static-debian13:latest@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0
COPY --from=build /relume-tv /usr/local/bin/relume-tv
VOLUME /data
# WORKDIR=/data, so the default config (relume-tv.json) already lands in the volume.
WORKDIR /data
# /healthz on the TV-facing port (-http-port, default 80); a custom port needs the compose healthcheck to pass it.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["relume-tv", "serve", "-healthcheck"]
ENTRYPOINT ["relume-tv"]
CMD ["serve"]
