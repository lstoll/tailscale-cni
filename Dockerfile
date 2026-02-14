FROM golang:1-trixie AS builder

WORKDIR /src

ARG CNI_PLUGINS_VERSION=v1.9.0

RUN go install github.com/containernetworking/plugins/plugins/ipam/host-local@${CNI_PLUGINS_VERSION} && \
    go install github.com/containernetworking/plugins/plugins/main/bridge@${CNI_PLUGINS_VERSION} && \
    go install github.com/containernetworking/plugins/plugins/meta/portmap@${CNI_PLUGINS_VERSION} && \
    go install github.com/containernetworking/plugins/plugins/main/loopback@${CNI_PLUGINS_VERSION}

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go install ./cmd/tailscale-cni ./cmd/cert-fetcher

FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/bin/host-local /cni/host-local
COPY --from=builder /go/bin/bridge /cni/bridge
COPY --from=builder /go/bin/portmap /cni/portmap
COPY --from=builder /go/bin/loopback /cni/loopback

COPY --from=builder /go/bin/tailscale-cni /usr/local/bin/tailscale-cni
COPY --from=builder /go/bin/cert-fetcher /usr/local/bin/cert-fetcher
ENTRYPOINT ["/usr/local/bin/tailscale-cni"]
