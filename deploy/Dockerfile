# Build from the repo root:  docker build -f deploy/Dockerfile -t sd2nas .
# A node stage that builds web/dist is added in M3, when the UI exists.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sd2nas ./cmd/sd2nas

FROM alpine:3.22
# tzdata: TZ support. exiftool: RAW previews. ffmpeg: video thumbnails.
# su-exec: drop from root to PUID:PGID in the entrypoint.
RUN apk add --no-cache tzdata exiftool ffmpeg su-exec \
 && mkdir -p /cards /photos /config \
 && chown 99:100 /photos /config
# Pre-created and owned by 99:100 so an unmounted /config (an anonymous
# volume) is still writable under --user; bind mounts replace these.
COPY --from=build /out/sd2nas /usr/local/bin/sd2nas
COPY --chmod=755 deploy/entrypoint.sh /usr/local/bin/entrypoint.sh

ENV PUID=99 \
    PGID=100 \
    SD2NAS_ADDR=:8080 \
    SD2NAS_CARDS=/cards \
    SD2NAS_PHOTOS=/photos \
    SD2NAS_CONFIG=/config
EXPOSE 8080
VOLUME ["/config"]

# Starts as root only to fix /config ownership, then runs sd2nas as
# PUID:PGID (99:100 = nobody:users on Unraid).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
