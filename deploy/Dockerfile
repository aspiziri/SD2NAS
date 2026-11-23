# Build from the repo root:  docker build -f deploy/Dockerfile -t sd2nas .
# A node stage that builds web/dist is added in M3, when the UI exists.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
# go.sum appears once there are dependencies (M1); copy it then and run go mod download here.
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sd2nas ./cmd/sd2nas

FROM alpine:3.22
# tzdata: TZ support. exiftool: RAW previews. ffmpeg: video thumbnails.
RUN apk add --no-cache tzdata exiftool ffmpeg \
 && mkdir -p /cards /photos /config \
 && chown 99:100 /photos /config
# Pre-created and owned by 99:100 so an unmounted /config (an anonymous
# volume) is still writable; bind mounts replace these with host folders.
COPY --from=build /out/sd2nas /usr/local/bin/sd2nas

ENV SD2NAS_ADDR=:8080 \
    SD2NAS_CARDS=/cards \
    SD2NAS_PHOTOS=/photos \
    SD2NAS_CONFIG=/config
EXPOSE 8080
VOLUME ["/config"]

# nobody:users on Unraid; the template also passes --user 99:100
USER 99:100
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/sd2nas"]
