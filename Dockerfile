# Production Dockerfile -- distroless runtime for smallest image + lowest attack surface.
#
# Runtime: gcr.io/distroless/base-debian12:nonroot
#   - glibc (satisfies plugin .so loading: dlopen + glibc-built store plugins)
#   - libssl (outbound TLS to providers)
#   - ca-certificates (TLS verification)
#   - tzdata (the ENV TZ below works without any apt install)
#   - NON-root user (distroless nonroot variant runs as uid 65532)
#   - NO shell, NO apt, NO curl, NO coreutils -- minimal attack surface
#
# Build time is not a concern here; runtime image size, startup speed, memory
# footprint, and attack surface are. Distroless base-debian12 is ~25MB vs
# debian:bookworm-slim's ~80MB, with no package manager or shell for an
# attacker to use if the process is compromised.
#
# The debug variant (Dockerfile.debug) keeps the bookworm-slim + shell + curl
# runtime for situations where you need `docker exec -it <ctr> sh`. Build and
# tag it separately as :prod-arm64-debug when needed; prod runs this file.
#
# CGO is ENABLED so the plugin host (internal/pluginhost/loader_unix.go)
# compiles in the real Go `plugin` (.so) loader instead of the no-op
# loader_unsupported.go stub. That makes X-Cpa-Support-Plugin report "1",
# which the management dashboard reads to decide whether to show the
# Plugins / Plugin Store nav.
#
# Builder + runtime are glibc (debian:bookworm builder, distroless base-debian12
# runtime -- both glibc, so the binary's dynamic linker matches). Two reasons
# both load-bearing for plugin support:
#   1. Go's `plugin` package calls dlopen(), which requires a DYNAMIC libc.
#      A fully-static musl binary (the alpine+CGO=0 path) returns
#      "Dynamic loading not supported" -- plugin.Open always fails.
#   2. The official plugin store (router-for-me/CLIProxyAPI-Plugins-Store)
#      ships .so files built against glibc (ld-linux-aarch64.so.1,
#      __memcpy_chk, fcntl64, etc.). They cannot relocate against musl.
#      Using glibc on our side means the store's plugins load natively
#      with zero extra toolchain.
#
# Cross-compile from Mac (darwin/arm64) to Linux/arm64 still needs a C
# compiler that emits Linux ELF for arm64 -- the host clang/gcc can't
# (Mach-O only). We use zig as the C compiler: `zig cc` is a drop-in
# clang-compatible frontend that targets any Linux/arch natively, no
# separate cross-toolchain package. The -target triple uses glibc
# (linux-gnu), NOT musl, so the resulting binary links against glibc
# and matches the distroless runtime image.
#
# Build:
#   docker buildx build --platform linux/arm64 \
#     -t ghcr.io/harshav167/cliproxyapi:prod-arm64 \
#     --push .
#
# Multi-arch (for local intel Mac / CI testing alongside prod arm64):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t ghcr.io/harshav167/cliproxyapi:prod \
#     --push .

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN apt-get update && apt-get install -y --no-install-recommends xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install zig by downloading the official tarball. Pin the version for
# reproducibility. Bump ZIG_VERSION only after verifying the new release at
# https://ziglang.org/download/ produces working linux-gnu cross-compiles for
# both x86_64 and aarch64 builders.
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

# --- runtime stage ---
# gcr.io/distroless/base-debian12 (root variant, NOT nonroot) already contains:
#   glibc, libssl, ca-certificates, tzdata, /tmp, a root user.
# No apt installs needed. No shell, no package manager, no curl -- the
# healthcheck must be an external HTTP probe of /healthz, not an in-container
# curl. The cpa-usage-keeper sidecar + compose restart policy provide liveness
# signals; add an external `curl http://localhost:8312/healthz` probe in the
# orchestrator if you need a formal healthcheck.
#
# WHY ROOT (not nonroot): the binary hardcodes /root/.cli-proxy-api as the auth
# dir and /CLIProxyAPI/static for management assets. The nonroot variant
# (uid 65532, home /home/nonroot) cannot write to either path -> permission
# denied -> crash loop. Making the binary $HOME-aware + relocating static is a
# bigger code change; for now we run as root in distroless (still no shell/apt,
# still minimal attack surface vs bookworm-slim). Switch to :nonroot after the
# binary is made HOME-aware.
FROM gcr.io/distroless/base-debian12

COPY --from=builder /app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Australia/Sydney

# ENTRYPOINT (distroless convention) -- no shell, direct exec. Compose files
# that set `command:` MUST remove it, otherwise `command` becomes args to this
# entrypoint and the binary fails to parse them.
ENTRYPOINT ["/CLIProxyAPI/CLIProxyAPI"]