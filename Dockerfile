# syntax=docker/dockerfile:1

# --- Build stage: one static Go binary -------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/chronohost ./cmd/chronohost

# --- Runtime stage: .NET runtimes for both VS generations ------------------
# 1.21.x targets .NET 8; 1.22.x targets .NET 10. The base image supplies 10,
# and the installer adds 8 side by side.
FROM mcr.microsoft.com/dotnet/runtime:10.0-noble AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    DOTNET_ROOT=/usr/share/dotnet \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 \
    PATH=/usr/share/dotnet:$PATH

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tar \
 && curl -fsSL https://dot.net/v1/dotnet-install.sh -o /tmp/dotnet-install.sh \
 && chmod +x /tmp/dotnet-install.sh \
 && /tmp/dotnet-install.sh --runtime dotnet --channel 8.0 --install-dir "$DOTNET_ROOT" --no-path \
 && rm -f /tmp/dotnet-install.sh \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

# Fail the build if either server generation's runtime is missing.
RUN dotnet --list-runtimes | grep -Eq 'Microsoft.NETCore.App 8\.' \
 && dotnet --list-runtimes | grep -Eq 'Microsoft.NETCore.App 10\.'

RUN groupadd --gid 1000 chronohost \
 && useradd --uid 1000 --gid chronohost --create-home --home-dir /home/chronohost --shell /usr/sbin/nologin chronohost \
 && mkdir -p /data/binaries /data/instances /data/backups /data/db \
 && chown -R chronohost:chronohost /data /home/chronohost

COPY --from=builder /out/chronohost /usr/local/bin/chronohost
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/chronohost /usr/local/bin/entrypoint.sh

VOLUME ["/data"]
WORKDIR /data
USER chronohost

# Web portal and the documented multi-instance game port range.
EXPOSE 8080 42420-42430/tcp 42420-42430/udp

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["-addr", ":8080"]
