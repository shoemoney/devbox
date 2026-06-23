# devbox hub — multi-stage, static (CGO-free, pure-Go sqlite), tiny final image.
# Build:  docker build -t devbox-hub .
# Run:    docker run -d --restart unless-stopped -p 8088:8088 -v devbox:/data devbox-hub
# (or just `docker compose up -d` — see docker-compose.yml)

FROM golang:1.26-alpine AS build
WORKDIR /src
# cache deps separately from source for faster rebuilds
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
# CGO off -> a fully static binary that runs on scratch/alpine without libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/devbox-hub ./cmd/devbox-hub

FROM alpine:3.20
# ca-certificates + tzdata for correct TLS/timestamps; wget (busybox) backs the healthcheck.
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 devbox
COPY --from=build /out/devbox-hub /usr/local/bin/devbox-hub
# Own /data BEFORE declaring the volume + dropping root, so the non-root user can
# write the sqlite DB + blobs (a named volume inherits the image dir's ownership).
RUN mkdir -p /data && chown devbox:devbox /data
USER devbox
VOLUME /data
# 8088 = hub API · 8099 = live dashboard (only reachable if you enable --dashboard)
EXPOSE 8088 8099
# ENTRYPOINT is just the binary so `command:` / `docker run … <args>` can tweak flags
# (e.g. add --dashboard). Default CMD serves the hub on all interfaces.
ENTRYPOINT ["devbox-hub"]
CMD ["serve", "--data", "/data", "--listen", "0.0.0.0:8088"]
