# cairnd with the pinned gVisor release, for Kubernetes (deploy/helm) or any
# container host. Build from the repo root:
#   docker build -f deploy/docker/Dockerfile -t cairnd:dev .
# Versions come from deploy/versions.env. On a laptop, `cairn up` runs it
# (privileged, with a named volume); see README.md.

# Go cross-compiles, so the build stage runs natively even for another target
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN export GOOS=$TARGETOS GOARCH=$TARGETARCH \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairnd ./cmd/cairnd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairn ./cmd/cairn \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairn-init ./cmd/cairn-init

FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl zstd iproute2 iptables e2fsprogs \
 && rm -rf /var/lib/apt/lists/*
# gVisor as pinned in deploy/versions.env (the default release plus RUNSC_KEEP),
# installed by the same script as every host
COPY deploy/install-gvisor.sh deploy/versions.env /tmp/gvisor/
RUN /tmp/gvisor/install-gvisor.sh && rm -rf /tmp/gvisor
COPY --from=build /out/cairnd /out/cairn /out/cairn-init /usr/local/bin/
COPY deploy/docker/entrypoint.sh /usr/local/bin/cairnd-entrypoint
VOLUME /var/lib/cairn
EXPOSE 7420
# leaves the container's root cgroup first (see the script), then runs
# cairnd --data /var/lib/cairn --listen 0.0.0.0:7420 plus any arguments
ENTRYPOINT ["cairnd-entrypoint"]
