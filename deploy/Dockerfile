# Multi-stage build for both VortexUI binaries. Build a specific image with:
#   docker build -f deploy/Dockerfile --target panel -t vortexui/panel .
#   docker build -f deploy/Dockerfile --target node  -t vortexui/node  .
# The binaries are pure Go (CGO disabled), so the panel ships on distroless/static.

# ---- builder ----
FROM golang:1.26-alpine AS builder
WORKDIR /src
# Cache modules first for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=1.0.0
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/panel ./cmd/panel && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/node ./cmd/node

# ---- xray fetch (for the node image) ----
FROM alpine:3.20 AS xray
ARG XRAY_VERSION=v26.10.21
ARG TARGETARCH
RUN apk add --no-cache curl unzip && \
    XARCH=$([ "$TARGETARCH" = "arm64" ] && echo "arm64-v8a" || echo "64") && \
    curl -fsSL -o /tmp/xray.zip \
      "https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-${XARCH}.zip" && \
    unzip /tmp/xray.zip -d /xray && chmod +x /xray/xray

# ---- sing-box fetch (for the node image) ----
FROM alpine:3.20 AS singbox
ARG SINGBOX_VERSION=1.12.12
ARG TARGETARCH
RUN apk add --no-cache curl tar && \
    SB_ARCH=$([ "$TARGETARCH" = "arm64" ] && echo "arm64" || echo "amd64") && \
    curl -fsSL -o /tmp/sb.tgz \
      "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${SB_ARCH}.tar.gz" && \
    tar -xzf /tmp/sb.tgz -C /tmp && \
    mv /tmp/sing-box-*/sing-box /usr/local/bin/sing-box && chmod +x /usr/local/bin/sing-box

# ---- metered sing-box (compiled here, not downloaded) ----
# The official release is built without with_v2ray_api, so a node running it
# cannot attribute traffic to a user and every quota silently reads zero.
# Compiling it inside a stage that is discarded keeps the image small and means
# the operator never needs Docker, Go or network access at runtime.
FROM golang:1.26-alpine AS metered
ARG SINGBOX_METERED_VERSION=1.14.2
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH}
RUN apk add --no-cache git && \
    mkdir -p /src && cd /src && go mod init build >/dev/null 2>&1 && \
    GOFLAGS=-mod=mod go get github.com/sagernet/sing-box@v${SINGBOX_METERED_VERSION} && \
    GOFLAGS=-mod=mod go build -tags with_v2ray_api -trimpath \
      -ldflags "-s -w -X github.com/sagernet/sing-box/constant.Version=v${SINGBOX_METERED_VERSION}" \
      -o /out/sing-box-metered github.com/sagernet/sing-box/cmd/sing-box && \
    go version -m /out/sing-box-metered | grep -q 'with_v2ray_api' || \
      { echo "built binary lacks with_v2ray_api" >&2; exit 1; }

# ---- panel (control plane, lean — for multi-node setups) ----
FROM gcr.io/distroless/static-debian12 AS panel
COPY --from=builder /out/panel /usr/local/bin/panel
COPY --from=builder /src/migrations /migrations
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/panel"]

# ---- panel-aio (all-in-one: panel + xray + sing-box for a single-server,
#      in-process local node). Used by the default single-server compose. ----
FROM alpine:3.20 AS panel-aio
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/panel /usr/local/bin/panel
COPY --from=xray /xray/xray /usr/local/bin/xray
# Only the metered build ships here: the vendor sing-box cannot attribute traffic,
# and carrying both costs 42 MB for a binary this image would never want to run.
COPY --from=metered /out/sing-box-metered /usr/local/bin/sing-box
# The agent this panel hands to new nodes. Same arch as the image; a fleet with
# mixed architectures needs a matching panel build for the other one.
COPY --from=builder /out/node /opt/vortexui/agent/vortex-node
COPY --from=xray /xray/*.dat /etc/vortex/assets/
ENV XRAY_LOCATION_ASSET=/etc/vortex/assets \
    VORTEX_BUNDLED_METERED=/usr/local/bin/sing-box \
    VORTEX_SINGBOX_BIN=/usr/local/bin/sing-box \
    VORTEX_SINGBOX_V2RAY_API=true \
    VORTEX_AGENT_DIR=/opt/vortexui/agent
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/panel"]

# ---- node (agent + xray-core) ----
FROM alpine:3.20 AS node
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 vortex
COPY --from=builder /out/node /usr/local/bin/node
COPY --from=xray /xray/xray /usr/local/bin/xray
COPY --from=singbox /usr/local/bin/sing-box /usr/local/bin/sing-box
# Geo databases live in a writable dir so the panel can refresh them at runtime.
COPY --from=xray /xray/*.dat /etc/vortex/assets/
RUN chown -R vortex:vortex /etc/vortex
# Defaults to xray; set VORTEX_CORE=singbox + VORTEX_CORE_BIN=/usr/local/bin/sing-box
# on a node to run sing-box instead. Keep VORTEX_SINGBOX_V2RAY_API=false for stock sing-box.
ENV VORTEX_CORE=xray \
    VORTEX_CORE_BIN=/usr/local/bin/xray \
    VORTEX_CORE_CONFIG=/etc/vortex/core.json \
    VORTEX_SINGBOX_V2RAY_API=false \
    XRAY_LOCATION_ASSET=/etc/vortex/assets
EXPOSE 50051
USER vortex
ENTRYPOINT ["/usr/local/bin/node"]
