# ---- build stage ----
FROM golang:1.26 AS build
WORKDIR /src

# Cache modules first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build static binary (CGO off: store uses pure-Go sqlite if applicable)
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/apid .

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
