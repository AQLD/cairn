# cairnd with the pinned gVisor release, for Kubernetes (deploy/helm) or any
# container host. Build from the repo root:
#   docker build -f deploy/docker/Dockerfile -t cairnd:dev .
# Versions come from deploy/versions.env (pass them as build args to change).

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairnd ./cmd/cairnd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairn ./cmd/cairn \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairn-init ./cmd/cairn-init

FROM ubuntu:24.04
ARG RUNSC_RELEASE=20261005
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl zstd iproute2 iptables \
 && rm -rf /var/lib/apt/lists/*
# gVisor, verified against its published checksum, kept per release (upgrade-safe restore)
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in amd64) a=x86_64 ;; arm64) a=aarch64 ;; *) echo "unsupported arch $TARGETARCH"; exit 1 ;; esac; \
    url="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_RELEASE}/${a}/gvisor.tar.zstd"; \
    cd /tmp && curl -fsSLO --retry 3 "$url" && curl -fsSLO --retry 3 "$url.sha512" && sha512sum -c gvisor.tar.zstd.sha512; \
    mkdir -p /opt/gvisor/${RUNSC_RELEASE} && zstd -dc gvisor.tar.zstd | tar -x -C /opt/gvisor/${RUNSC_RELEASE}; \
    ln -s /opt/gvisor/${RUNSC_RELEASE}/runsc /usr/local/bin/runsc; rm -f /tmp/gvisor.tar.zstd*
COPY --from=build /out/cairnd /out/cairn /out/cairn-init /usr/local/bin/
VOLUME /var/lib/cairn
EXPOSE 7420
ENTRYPOINT ["cairnd", "--data", "/var/lib/cairn", "--listen", "0.0.0.0:7420"]
