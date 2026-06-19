#!/bin/sh
# Run the server as PUID:PGID (default 1000:1000) so SQLite can write the data dir
# no matter how the host-mounted volume is owned — the usual NAS pain point. The
# container starts as root only to chown /data and drop privileges; the server
# itself never runs as root. On NAS setups set PUID/PGID to the owner of your
# appdata (Unraid: PUID=99 PGID=100; or run `id` for your user).
#
# If the container is started with an explicit non-root user (docker `--user` /
# compose `user:`), we skip the chown/drop and just exec as that user.
set -e

if [ "$(id -u)" = "0" ]; then
  PUID="${PUID:-1000}"
  PGID="${PGID:-1000}"
  chown -R "${PUID}:${PGID}" /data 2>/dev/null || true
  exec su-exec "${PUID}:${PGID}" "$@"
fi

exec "$@"
