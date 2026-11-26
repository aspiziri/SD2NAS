# Build from the repo root:  docker build -f deploy/Dockerfile -t sd2nas .

# Base images are pinned by tag and digest (the multi-arch index digest), so a
# release builds from the bytes it was tested on. To refresh a pin:
#   docker pull <image>:<tag>
#   docker image inspect --format '{{index .RepoDigests 0}}' <image>:<tag>
# then write the most specific tag that shares that digest
# (docker buildx imagetools inspect <image>:<tag>) plus @sha256:<digest>.

# The web UI: web/dist, embedded into the binary by package web.
FROM node:24.22.3-alpine3.24@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm test && npm run build

FROM golang:1.26.11-alpine3.24@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/*.go ./web/
COPY --from=web /web/dist ./web/dist
# Fail loudly rather than ship a binary whose UI is the "not built" page.
RUN test -f web/dist/index.html
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sd2nas ./cmd/sd2nas

FROM alpine:3.22.8@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
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
