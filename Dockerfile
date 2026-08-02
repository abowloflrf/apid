# ---- webui build stage ----
FROM node:24-alpine AS webui
WORKDIR /src
RUN corepack enable
COPY webui/package.json webui/pnpm-lock.yaml webui/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY webui/ ./
# vite.config.ts outDir is "../server/webui/dist" relative to webui/,
# which resolves to /server/webui/dist inside this container.
RUN pnpm build

# ---- build stage ----
# Run on the builder's native arch and cross-compile, so multi-platform
# builds never compile Go under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

# Cache modules first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build static binary (CGO off: store uses pure-Go sqlite if applicable)
# GOEXPERIMENT=jsonv2 swaps encoding/json to the v2 engine (v1 semantics kept):
# ~2x faster SSE chunk decode, ~5x fewer allocations. Build-time only.
ARG TARGETOS TARGETARCH
ARG GOEXPERIMENT=jsonv2
COPY . .
COPY --from=webui /server/webui/dist /src/server/webui/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOEXPERIMENT=$GOEXPERIMENT \
    go build -trimpath -ldflags="-s -w" -o /out/apid .

# ---- runtime stage ----
FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata curl \
    && adduser -D -u 10001 apid
WORKDIR /app

COPY --from=build /out/apid /usr/local/bin/apid
COPY --from=build /src/config.example.toml /app/config.example.toml

# Data dir for the metrics DB (APID_DB), owned by the non-root user.
RUN mkdir -p /app/data && chown -R apid:apid /app

USER apid
EXPOSE 19092

ENTRYPOINT ["apid"]
CMD ["--config", "/app/config.toml"]
