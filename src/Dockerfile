# syntax=docker/dockerfile:1

# Build context is src/:
#   docker build -t serverscom-exporter \
#     --build-arg VERSION=$(git describe --tags --match 'v*' --always) \
#     --build-arg REVISION=$(git rev-parse HEAD) \
#     src/

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
ARG BRANCH=unknown
ARG BUILD_DATE=unknown
# Optional module proxy, e.g. an Athens or Nexus instance. Empty means the Go
# default (proxy.golang.org). Checksums are still verified against go.sum.
ARG GOPROXY

WORKDIR /src

# Download modules in a separate layer so code changes do not refetch them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static binary, same ldflags as the Makefile; version info is read by
# prometheus/common/version and shown by --version, the landing page and
# serverscom_exporter_build_info.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/prometheus/common/version.Version=${VERSION} \
        -X github.com/prometheus/common/version.Revision=${REVISION} \
        -X github.com/prometheus/common/version.Branch=${BRANCH} \
        -X github.com/prometheus/common/version.BuildDate=${BUILD_DATE}" \
      -o /out/serverscom-exporter ./cmd/serverscom-exporter

# Directory for --cache.file. The runtime image has no shell to create it, so
# it is prepared here; Docker named volumes mounted at /data inherit its owner.
RUN mkdir -p /out/data

# distroless/static: CA certificates and tzdata only, no shell, runs as
# uid 65532. To persist the cache use --cache.file=/data/cache.json with a
# volume mounted at /data.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/serverscom-exporter /usr/bin/serverscom-exporter
COPY --from=build --chown=nonroot:nonroot /out/data /data

USER nonroot:nonroot
EXPOSE 9920
ENTRYPOINT ["/usr/bin/serverscom-exporter"]
