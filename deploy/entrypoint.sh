#!/bin/sh
# Starts as root, makes /config owned by PUID:PGID, then runs sd2nas as that
# user. If the container was started with --user, it is already unprivileged
# and runs sd2nas directly.
set -eu

if [ "$(id -u)" != "0" ]; then
    exec /usr/local/bin/sd2nas "$@"
fi

PUID="${PUID:-99}"
PGID="${PGID:-100}"
case "$PUID$PGID" in
    *[!0-9]*) echo "entrypoint: PUID and PGID must be numbers (got PUID=$PUID PGID=$PGID)" >&2; exit 1 ;;
esac

# /config only. /photos is the real photo share and is never chowned; /healthz
# reports it if it isn't writable. Only files with the wrong owner are touched,
# so restarts are quick.
if ! find "${SD2NAS_CONFIG:-/config}" \( ! -user "$PUID" -o ! -group "$PGID" \) \
        -exec chown "$PUID:$PGID" {} + ; then
    echo "entrypoint: WARNING: could not set ownership of ${SD2NAS_CONFIG:-/config} to $PUID:$PGID; /healthz will show whether it is writable" >&2
fi

exec su-exec "$PUID:$PGID" /usr/local/bin/sd2nas "$@"
