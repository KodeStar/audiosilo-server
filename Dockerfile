# syntax=docker/dockerfile:1
#
# AudioSilo server image. The web player is NOT vendored in this repo; it is baked
# in here at image-build time from a pinned, prebuilt frontend image so the server
# and the bundled player ship as one known-compatible artifact. Updating either is
# a new image + `docker pull` — the web build never lives in the /data volume.

# --- build the Go server -------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/audiosilo ./cmd/audiosilo

# --- pin the prebuilt web player ----------------------------------------------
# audiosilo-frontend CI publishes a tiny image holding only its static web export
# (built with baseUrl=/web). Pin it per release. To build a server-only image
# (web_player capability off, /web unmounted), comment out this stage and the
# COPY --from=web below.
ARG WEB_IMAGE=ghcr.io/kodestar/audiosilo-web:latest
FROM ${WEB_IMAGE} AS web

# --- final image ---------------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates ffmpeg \
    && adduser -D -u 10001 audiosilo
COPY --from=build /out/audiosilo /usr/local/bin/audiosilo
COPY --from=web   /web           /app/web
ENV AUDIOSILO_WEB_DIR=/app/web
USER audiosilo
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["audiosilo", "--data", "/data"]
