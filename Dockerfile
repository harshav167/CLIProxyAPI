FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
# CGO is ENABLED so the plugin host (internal/pluginhost/loader_unix.go)
# compiles in the real Go `plugin` (.so) loader instead of the no-op
# loader_unsupported.go stub. That makes X-Cpa-Support-Plugin report "1",
# which the management dashboard reads to decide whether to show the
# Plugins / Plugin Store nav.
#
# Builder + runtime are now glibc (debian:bookworm) instead of alpine/musl.
# Two reasons, both load-bearing for plugin support:
#   1. Go's `plugin` package calls dlopen(), which requires a DYNAMIC libc.
#      A fully-static musl binary (the alpine+CGO=0 path) returns
#      "Dynamic loading not supported" — plugin.Open always fails.
#   2. The official plugin store (router-for-me/CLIProxyAPI-Plugins-Store)
#      ships .so files built against glibc (ld-linux-aarch64.so.1,
#      __memcpy_chk, fcntl64, etc.). They cannot relocate against musl.
#      Using glibc on our side means the store's plugins load natively
#      with zero extra toolchain.
#
# Cross-compile from Mac (darwin/arm64) to Linux/arm64 still needs a C
# compiler that emits Linux ELF for arm64 — the host clang/gcc can't
# (Mach-O only). We use zig as the C compiler: `zig cc` is a drop-in
# clang-compatible frontend that targets any Linux/arch natively, no
# separate cross-toolchain package. The -target triple uses glibc
# (linux-gnu), NOT musl, so the resulting binary links against glibc
# and matches the runtime image.
#
# Tradeoff: builder image is larger (zig ~150MB + bookworm base ~150MB
# vs alpine ~50MB) and the go build step is slower than the CGO=0
# static path (~4min vs ~30s). Runtime image is debian:bookworm-slim
# (~80MB vs alpine ~15MB). This is the same posture upstream's
# Dockerfile takes — the plugin store is tested against it.

RUN apt-get update && apt-get install -y --no-install-recommends xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install zig by downloading the official tarball (alpine's zig package
# isn't available in bookworm apt). Pin the version for reproducibility.
ARG ZIG_VERSION=0.16.0
RUN case "$(uname -m)" in \
      x86_64) ZIG_HOST="x86_64" ;; \
      aarch64|arm64) ZIG_HOST="aarch64" ;; \
      *) echo "unsupported host arch: $(uname -m)" && exit 1 ;; \
    esac && \
    curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-${ZIG_HOST}-linux-${ZIG_VERSION}.tar.xz" -o /tmp/zig.tar.xz && \
    tar -xf /tmp/zig.tar.xz -C /opt && \
    ln -s /opt/zig-${ZIG_HOST}-linux-${ZIG_VERSION}/zig /usr/local/bin/zig && \
    rm /tmp/zig.tar.xz

ARG TARGETARCH

# Map Docker TARGETARCH to the zig glibc target triple. arm64 -> aarch64-linux-gnu,
# amd64 -> x86_64-linux-gnu. Build with CGO=1 + zig as CC, linking against glibc.
RUN case "${TARGETARCH}" in \
      arm64) ZIG_TRIPLE="aarch64-linux-gnu" ;; \
      amd64) ZIG_TRIPLE="x86_64-linux-gnu" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} \
    CC="zig cc -target ${ZIG_TRIPLE}" \
    CXX="zig c++ -target ${ZIG_TRIPLE}" \
    go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm-slim

# curl is needed by the docker-compose healthcheck (alpine's wget isn't
# available in bookworm-slim, and curl isn't there by default either).
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata curl \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Australia/Sydney

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]