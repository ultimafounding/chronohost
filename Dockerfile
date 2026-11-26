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

# --- Runtime stages: .NET runtimes for both VS generations -----------------
# 1.21.x targets .NET 8; 1.22.x targets .NET 10. Copying the official runtime
# directories avoids apt entirely (no mirror/hash-sum failures) and keeps both
# side by side. BusyBox supplies a static wget for the healthcheck.
FROM mcr.microsoft.com/dotnet/runtime:8.0-noble AS dotnet8
FROM busybox:stable AS tools
FROM mcr.microsoft.com/dotnet/runtime:10.0-noble AS runtime

ENV DOTNET_ROOT=/usr/share/dotnet \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 \
    PATH=/usr/share/dotnet:$PATH

COPY --from=dotnet8 /usr/share/dotnet/shared/Microsoft.NETCore.App/ /usr/share/dotnet/shared/Microsoft.NETCore.App/
COPY --from=dotnet8 /usr/share/dotnet/host/fxr/ /usr/share/dotnet/host/fxr/
COPY --from=tools /bin/busybox /usr/local/bin/busybox

# Fail the build if either server generation's runtime is missing.
RUN dotnet --list-runtimes | grep -Eq 'Microsoft.NETCore.App 8\.' \
 && dotnet --list-runtimes | grep -Eq 'Microsoft.NETCore.App 10\.'

RUN if ! getent group chronohost >/dev/null; then groupadd --system chronohost; fi \
 && if ! id -u chronohost >/dev/null 2>&1; then useradd --system --gid chronohost --create-home --home-dir /home/chronohost --shell /usr/sbin/nologin chronohost; fi \
 && mkdir -p /data/binaries /data/instances /data/backups /data/db \
 && chown -R chronohost:chronohost /data /home/chronohost

COPY --from=builder /out/chronohost /usr/local/bin/chronohost
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/chronohost /usr/local/bin/entrypoint.sh

VOLUME ["/data"]
WORKDIR /data
USER chronohost

# Web portal and the documented multi-instance game port range.
EXPOSE 3620 42420-42430/tcp 42420-42430/udp

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD /usr/local/bin/busybox wget -q -O /dev/null http://127.0.0.1:3620/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["-addr", ":3620"]
