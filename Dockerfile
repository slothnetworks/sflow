# syntax=docker/dockerfile:1
# Builds one NetMeta command per image:
#   docker build --build-arg CMD=migrate -t ghcr.io/slothnetworks/netmeta-migrate .
#   docker build --build-arg CMD=geoip   -t ghcr.io/slothnetworks/netmeta-geoip .

FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build
ARG CMD
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$CMD" || (echo "build with --build-arg CMD=<geoip|migrate>" >&2; exit 1)
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/$CMD
# State directory for geoip. A named volume mounted here starts with this ownership, so the
# nonroot user can write to it (an absent mount point would be created root-owned).
RUN mkdir -p /out/state

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/app /usr/local/bin/app
COPY --from=build --chown=65532:65532 /out/state /var/lib/netmeta-geoip
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/app"]
