# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
# The SQLite driver is pure Go, so the binary is fully static (no CGO).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/nepthass ./cmd/nepthass \
    && mkdir /out/data

# distroless/static ships CA certificates (needed for Discord TLS) and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="NeptHass" \
      org.opencontainers.image.description="Secure, self-hosted, Discord-first control and monitoring bridge for Home Assistant" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/nepthass/nepthass"
COPY --from=build /out/nepthass /usr/local/bin/nepthass
# Pre-create the data directory owned by the non-root user so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATABASE_PATH=/data/nepthass.db \
    LOG_FORMAT=json
USER 65532:65532
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/nepthass"]
