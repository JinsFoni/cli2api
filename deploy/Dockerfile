# ---- frontend ----
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /web
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --cache /tmp/npm-cache
COPY frontend/ ./
RUN npm run build

# ---- go control plane ----
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG APP_VERSION=dev
ARG APP_COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /web/dist/ ./internal/webui/static/
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags "-s -w -X github.com/caigee-cmd/cli2api/internal/buildinfo.Version=${APP_VERSION} -X github.com/caigee-cmd/cli2api/internal/buildinfo.Commit=${APP_COMMIT}" -o /out/qoder-api-proxy ./cmd/server \
  && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags "-s -w -X github.com/caigee-cmd/cli2api/internal/buildinfo.Version=${APP_VERSION} -X github.com/caigee-cmd/cli2api/internal/buildinfo.Commit=${APP_COMMIT}" -o /out/cli2api-updater ./cmd/updater

# ---- single runtime (pure Go; no Node) ----
FROM debian:bookworm-slim
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl tini \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/qoder-api-proxy /app/qoder-api-proxy
COPY --from=build /out/cli2api-updater /app/cli2api-updater
RUN mkdir -p /data && chmod 700 /data
ENV HOST=0.0.0.0 \
    PORT=3010 \
    QODER_DATA_DIR=/data
EXPOSE 3010
ENTRYPOINT ["/usr/bin/tini","--"]
CMD ["/app/qoder-api-proxy"]
