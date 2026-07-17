# Dockerfile for weave-adapter-dhcp-windows (local-dev container).
#
# Mirrors weave's approach: the binary is cross-compiled on the host
# (`task build:docker`, static CGO_ENABLED=0) and COPYed in here — no in-image
# Go toolchain. Note: the image is linux/arm64 for local dev on Apple Silicon;
# the production artifact remains the Windows .exe.

FROM ubuntu:22.04

LABEL maintainer="Radiant Garden"
LABEL description="weave-adapter-dhcp-windows — REST adapter for Windows Server DHCP"

# Runtime deps: TLS roots, timezone data, and curl for the healthcheck.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        curl \
    && rm -rf /var/lib/apt/lists/*

# Non-root app user.
RUN useradd -r -u 1000 -g root -s /bin/bash -m -d /app adapter

WORKDIR /app

# Cache-busting: changing BINARY_HASH invalidates the COPY layer below.
ARG BINARY_HASH=unknown
RUN echo "Binary hash: ${BINARY_HASH}" > /dev/null

# Copy the host-built static Linux binary.
COPY weave-adapter-dhcp-windows-linux-arm64 /usr/local/bin/weave-adapter-dhcp-windows
RUN chmod +x /usr/local/bin/weave-adapter-dhcp-windows

USER adapter

# HTTP API port (matches the fixed :8444 until config lands in Phase 1).
EXPOSE 8444

# Health/readiness: the endpoint returns 503 when unavailable, so this flips the
# container to unhealthy automatically.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -sf http://localhost:8444/api/v1/health || exit 1

CMD ["/usr/local/bin/weave-adapter-dhcp-windows"]
