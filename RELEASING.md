# Releasing & publishing

Maintainer notes for building and publishing the container images, and a quick
end-to-end smoke test. End users only need [README.md](README.md).

The project ships two images on GHCR:

- `ghcr.io/kodestar/audiosilo-web` — the web player (the `audiosilo-frontend`
  Expo web export, built with `baseUrl=/web`). Just static files.
- `ghcr.io/kodestar/audiosilo-server` — this server, with a pinned web build
  baked in at `/app/web` via `COPY --from`. This is the deployable image.

The server image pins a web version, so server + bundled player are always a
known-compatible pair. Native apps negotiate via `GET /api/v1/server` capability
flags. (Owner `kodestar` is from the module path; the CI workflows use
`${{ github.repository_owner }}` and self-adjust — only the `Dockerfile`'s default
`WEB_IMAGE` hardcodes the owner.)

## One-time setup

- After the first push, make both GHCR packages **public**, or `docker login
  ghcr.io` on the deploy host so it can pull private images.

## 1 — publish the web player (audiosilo-frontend)

Push to `main` (or tag `v*`). `.github/workflows/web.yml` exports the web build
(`baseUrl=/web` from `app.json`) and pushes `ghcr.io/kodestar/audiosilo-web:latest`
(and `:<version>` on tags).

## 2 — publish the server (this repo)

Push a tag `v*` (or run the *server image* workflow manually).
`.github/workflows/image.yml` builds the multi-stage `Dockerfile`, baking in
`ghcr.io/kodestar/audiosilo-web:latest`, and pushes
`ghcr.io/kodestar/audiosilo-server`. Publish the web image first — this build
pulls it.

Build locally instead:

```sh
docker login ghcr.io
docker build --build-arg WEB_IMAGE=ghcr.io/kodestar/audiosilo-web:latest \
  -t ghcr.io/kodestar/audiosilo-server:dev .
docker push ghcr.io/kodestar/audiosilo-server:dev
```

## 2b — native binaries (GitHub Releases)

The same `v*` tag also triggers `.github/workflows/release.yml` (GoReleaser,
`.goreleaser.yml`): self-contained cross-platform binaries for home users who
don't want Docker. It embeds the web player (`-tags embedplayer`, populated from
the pinned web image by `scripts/fetch-web-player.sh`), producing `.tar.gz`/`.zip`
archives, `.deb`/`.rpm` packages and `checksums.txt` as a **draft** GitHub Release
to review and publish. ffmpeg/ffprobe are not bundled — the server uses a local
copy or auto-downloads one into `<data>/tools` on first run (see DISTRIBUTION.md).
Publish the web image first (step 1) so the embedded player matches. The full
distribution strategy (and the deferred desktop installers/tray) lives in the
workspace [DISTRIBUTION.md](../DISTRIBUTION.md).

Validate the GoReleaser config locally without releasing:

```sh
goreleaser check
goreleaser build --snapshot --clean --skip=before --single-target
```

## 3 — end-to-end smoke test

1. `docker compose up -d`; grab the admin password from `docker compose logs`.
2. Open `/admin`, sign in, add a library, create a user, click **Copy invite**.
3. **Web:** open the invite link → connect screen → **Open web player** (or visit
   `/web`) → it exchanges the token and drops you into the player.
4. **Native:** run the app from audiosilo-frontend (`expo start` / a dev build),
   scan the QR or open the invite to pair. For tap-to-open-app from the OS, set
   `app_links` (and serve over HTTPS on the claimed domain).

## Local dev without Docker

```sh
scripts/build-web.sh    # builds the frontend export, prints the AUDIOSILO_WEB_DIR to use
AUDIOSILO_WEB_DIR=~/dev/audiosilo/audiosilo-frontend/dist AUDIOSILO_TLS_MODE=off \
  ./bin/audiosilo --data ./data
```
