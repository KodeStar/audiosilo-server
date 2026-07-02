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

# --- build the Go server -------------------------------------------------------
# Pinned to the build host's platform: the binary is CGO-free, so multi-arch legs
# cross-compile natively via GOOS/GOARCH instead of running the whole Go toolchain
# under QEMU emulation (which made the arm64 image build many times slower).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
# Release version stamped into the binary (reported by GET /server, the admin
# console and the web player). image.yml passes the release tag; defaults to dev.
ARG VERSION=dev
# Target platform for the cross-compile, provided by BuildKit per leg (empty on
# a legacy single-platform build, where the host defaults apply).
ARG TARGETOS TARGETARCH
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/kodestar/audiosilo-server/internal/api.Version=${VERSION}" \
    -o /out/audiosilo ./cmd/audiosilo

# --- pin the prebuilt web player ----------------------------------------------
FROM ${WEB_IMAGE} AS web

# --- final image ---------------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates ffmpeg su-exec
COPY --from=build /out/audiosilo /usr/local/bin/audiosilo
COPY --from=web   /web           /app/web
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
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
