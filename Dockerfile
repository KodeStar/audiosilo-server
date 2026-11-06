# syntax=docker/dockerfile:1
#
# AudioSilo server image. The web player is NOT vendored in this repo; it is baked
# in here at image-build time from a pinned, prebuilt frontend image so the server
# and the bundled player ship as one known-compatible artifact. Updating either is
# a new image + `docker pull` - the web build never lives in the /data volume.

# Pinned prebuilt web player image (audiosilo-frontend CI publishes a tiny image
# holding only its static web export, built with baseUrl=/web). Override per
# release: --build-arg WEB_IMAGE=ghcr.io/<owner>/audiosilo-web:<version> (the
# reference must be lowercase). Declared before any FROM so it is usable in the
# `FROM ${WEB_IMAGE}` below. To build a server-only image (web_player off, /web
# unmounted), comment out the web stage and the COPY --from=web line.
ARG WEB_IMAGE=ghcr.io/kodestar/audiosilo-web:latest

# --- build the admin console (admin-ui/) -----------------------------------------
# A static Vite build embedded into the binary (internal/web/adminui). Built on
# the build host's platform: the output is plain JS/CSS, identical for every arch.
FROM --platform=$BUILDPLATFORM node:24-alpine AS admin
WORKDIR /src/admin-ui
COPY admin-ui/package.json admin-ui/package-lock.json ./
RUN npm ci
COPY admin-ui/ ./
# vite.config.ts writes to ../internal/web/adminui/dist (embedded by the Go build).
RUN npm run build

# --- build the Go server -------------------------------------------------------
# Pinned to the build host's platform: multi-arch legs cross-compile via
# GOOS/GOARCH instead of running the whole Go toolchain under QEMU emulation
# (which made the arm64 image build many times slower).
#
# SQLITE_ENGINE=native (the default) builds the native C SQLite in (the store's
# sqlite_native engine, see SQLite in CONVENTIONS.md), linked statically against
# glibc: one static binary, so the Alpine image below runs it as it is, and with
# glibc's allocator, not musl's (musl's cost a first scan 27% against the
# pure-Go engine; glibc's runs it 6% faster). The Debian-based Go image has gcc
# and glibc's static libraries; a cross leg adds the target's cross compiler.
# netgo and osusergo keep the static binary off glibc's NSS, and
# sqlite_omit_load_extension off dlopen. SQLITE_ENGINE=modernc builds the
# pure-Go engine without cgo, as before. Keep the tags and the static link in
# step with .goreleaser.yml's audiosilo-linux build (the native tarballs).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
# Release version stamped into the binary (reported by GET /server, the admin
# console and the web player). image.yml passes the release tag on a v* tag and
# dev-<short sha> otherwise (:edge, dispatch); defaults to dev.
ARG VERSION=dev
ARG SQLITE_ENGINE=native
# Target platform for the cross-compile, provided by BuildKit per leg (empty on
# a legacy single-platform build, where the host defaults apply).
ARG TARGETOS TARGETARCH BUILDARCH
RUN if [ "$SQLITE_ENGINE" = native ] && [ -n "$TARGETARCH" ] && [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      case "$TARGETARCH" in \
        arm64) pkgs="gcc-aarch64-linux-gnu libc6-dev-arm64-cross" ;; \
        amd64) pkgs="gcc-x86-64-linux-gnu libc6-dev-amd64-cross" ;; \
        *) echo "no cross compiler for $TARGETARCH" >&2; exit 1 ;; \
      esac; \
      apt-get update && apt-get install -y --no-install-recommends $pkgs && rm -rf /var/lib/apt/lists/*; \
    fi
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=admin /src/internal/web/adminui/dist ./internal/web/adminui/dist
RUN set -e; \
    ldflags="-s -w -X github.com/kodestar/audiosilo-server/internal/api.Version=${VERSION}"; \
    if [ "$SQLITE_ENGINE" = native ]; then \
      cc=gcc; \
      if [ -n "$TARGETARCH" ] && [ "$TARGETARCH" != "$BUILDARCH" ]; then \
        case "$TARGETARCH" in arm64) cc=aarch64-linux-gnu-gcc ;; amd64) cc=x86_64-linux-gnu-gcc ;; esac; \
      fi; \
      CGO_ENABLED=1 CC=$cc GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
        -tags "sqlite_native sqlite_fts5 sqlite_preupdate_hook sqlite_omit_load_extension netgo osusergo" \
        -ldflags="$ldflags -linkmode external -extldflags -static" \
        -o /out/audiosilo ./cmd/audiosilo; \
    else \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
        -ldflags="$ldflags" -o /out/audiosilo ./cmd/audiosilo; \
    fi

# --- pin the prebuilt web player ----------------------------------------------
FROM ${WEB_IMAGE} AS web

# --- final image ---------------------------------------------------------------
# Track the current stable Alpine (3.20 went EOL in April 2026).
FROM alpine:3.24
RUN apk add --no-cache ca-certificates ffmpeg su-exec
COPY --from=build /out/audiosilo /usr/local/bin/audiosilo
COPY --from=web   /web           /app/web
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
# The server statically links glibc (LGPL-2.1-or-later): its notice ships with it.
COPY NOTICE /usr/share/doc/audiosilo/NOTICE
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
# Default to uid/gid 1000; override with PUID/PGID to match your data dir's owner
# (Unraid: 99/100). The entrypoint chowns /data and drops to that user.
ENV AUDIOSILO_WEB_DIR=/app/web \
    PUID=1000 \
    PGID=1000
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["audiosilo", "--data", "/data"]
