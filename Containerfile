# Build: static Go binary, cross-compiled on the build platform (native on the amd64 runner, no QEMU for the
# arm64 image's compile).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION: ship release passes the release version. TARGETOS/TARGETARCH: set by buildx per platform.
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /relume-tv ./cmd/relume-tv

# Runtime: slim image with CA certs (for cloud discovery via HTTPS)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /relume-tv /usr/local/bin/relume-tv
VOLUME /data
WORKDIR /data
# Default: serve. Invoke setup/link/discover via `docker compose run`.
# WORKDIR=/data, so the default config (relume-tv.json) already lands in the volume.
ENTRYPOINT ["relume-tv"]
CMD ["serve"]
