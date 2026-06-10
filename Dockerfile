FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
# TARGETARCH is provided by buildx. Compiling on the native BUILDPLATFORM and
# cross-targeting via GOARCH avoids QEMU-emulating the whole Go build (which is
# ~5-10x slower); the binary is static (CGO_ENABLED=0) so no cross toolchain is
# needed.
#
# Note on plugin host: internal/pluginhost has cgo loader files (host_callbacks_unix.go,
# loader_unix.go) gated `//go:build cgo && (linux || darwin || freebsd)` with a
# parallel no-op loader_unsupported.go used when cgo is off. Building with CGO=0
# trades runtime .so plugin loading for a static, fast cross-compile — we do not
# use plugins, so this is the right tradeoff.

ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM alpine:3.23

RUN apk add --no-cache tzdata ca-certificates

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Australia/Sydney

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
