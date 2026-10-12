# Releasing & publishing

Maintainer notes for building and publishing the container images, and a quick
end-to-end smoke test. End users only need [README.md](README.md).

The project ships two images on GHCR:

- `ghcr.io/kodestar/audiosilo-web` - the web player (the `audiosilo-frontend`
  Expo web export, built with `baseUrl=/web`). Just static files.
- `ghcr.io/kodestar/audiosilo-server` - this server, with a pinned web build
  baked in at `/app/web` via `COPY --from`. This is the deployable image.

The server image pins a web version, so server + bundled player are always a
known-compatible pair. Native apps negotiate via `GET /api/v1/server` capability
flags. (Owner `kodestar` is from the module path; the CI workflows use
`${{ github.repository_owner }}` and self-adjust - only the `Dockerfile`'s default
`WEB_IMAGE` hardcodes the owner.)

## One-time setup

- After the first push, make both GHCR packages **public**, or `docker login
  ghcr.io` on the deploy host so it can pull private images.

## 1 - publish the web player (audiosilo-frontend)

Push to `main` (or tag `v*`). `.github/workflows/web.yml` exports the web build
(`baseUrl=/web` from `app.json`) and pushes `ghcr.io/kodestar/audiosilo-web:latest`
(and `:<version>` on tags).

## 2 - publish the server (this repo)

Push a tag `v*` (or run the *server image* workflow manually).
`.github/workflows/image.yml` builds the multi-stage `Dockerfile`, baking in
`ghcr.io/kodestar/audiosilo-web:latest`, and pushes
`ghcr.io/kodestar/audiosilo-server`. Publish the web image first - this build
pulls it.

Build locally instead:

```sh
docker login ghcr.io
docker build --build-arg WEB_IMAGE=ghcr.io/kodestar/audiosilo-web:latest \
  -t ghcr.io/kodestar/audiosilo-server:dev .
docker push ghcr.io/kodestar/audiosilo-server:dev
```

## 2b - native binaries (GitHub Releases)

The same `v*` tag also triggers `.github/workflows/release.yml` (GoReleaser,
`.goreleaser.yml`): self-contained cross-platform binaries for home users who
don't want Docker. It embeds the web player (`-tags embedplayer`, populated from
the pinned web image by `scripts/fetch-web-player.sh`), producing `.tar.gz`/`.zip`
archives, `.deb`/`.rpm` packages and `checksums.txt` as a **draft** GitHub Release
to review and publish. The notes end with GoReleaser's `release.footer`, which
also carries the one sponsor line ("AudioSilo is free; sponsors keep it going",
linking GitHub Sponsors); keep it when you rewrite the notes by hand, and add
nothing more (no tiers, no rewards). ffmpeg/ffprobe are not bundled - the server uses a local
copy or auto-downloads one into `<data>/tools` on first run (see DISTRIBUTION.md).
Publish the web image first (step 1) so the embedded player matches. The full
distribution strategy (and the deferred desktop installers/tray) lives in the
workspace [DISTRIBUTION.md](../DISTRIBUTION.md).

Validate the GoReleaser config locally without releasing:

```sh
goreleaser check
goreleaser build --snapshot --clean --skip=before --single-target
```

`--skip=before` also skips the admin console build (`scripts/build-admin.sh
--build-only`, a before-hook); run `scripts/build-admin.sh`
first if the snapshot should include the console rather than its "not built" page.

## 2c - the LinuxServer.io image (follows automatically)

[linuxserver/docker-audiosilo](https://github.com/linuxserver/docker-audiosilo)
builds `lscr.io/linuxserver/audiosilo` (the image Unraid's Community Applications
lists) from this repo's GitHub Releases. Nothing to do per release: their
`external_trigger` workflow polls `releases/latest` hourly and, on a new tag, builds
`<tag>-ls<N>` (e.g. `v2.1.0-ls1`) from the linux archives. That makes these a
contract:

- **Publishing the draft is the trigger**, so publish only once the tag's
  `release.yml` run is green: its smoke test (below) runs after the draft is
  uploaded. Drafts and prereleases are invisible to `releases/latest`, but
  GoReleaser does not mark a `-rc` tag as a prerelease itself - tick *Set as a
  pre-release* when publishing one.
- **Archive names and layout.** Their Dockerfile downloads
  `releases/download/v<version>/audiosilo_<version>_linux_{amd64,arm64}.tar.gz`
  (`<version>` is the tag without its `v`) and takes the `audiosilo` binary from the
  archive root, so keep the `v` tags, the names and the flat layout.
- **A static binary** (`CGO_ENABLED=0`): the image is Alpine (musl).
- **The web player stays embedded** (`-tags embedplayer`): the image sets no
  `AUDIOSILO_WEB_DIR`.
- **How it runs:** `audiosilo --data /config` and nothing else, so it relies on the
  headless first run (the admin banner in the log), the self-signed TLS default on
  port 8080, and writes staying under `--data` (it supports a read-only root
  filesystem). Its readiness check reads `bind:` from `/config/config.yaml`.

`release.yml`'s last step checks the archive side of this while the release is still
a draft: both archives' exact names and layout, each binary's `embedplayer` tag,
`CGO_ENABLED=0` and architecture, and an amd64 boot run the way the image runs it
(`--data` only, self-signed TLS) serving `/healthz` and `/web/`.

## 3 - end-to-end smoke test

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
