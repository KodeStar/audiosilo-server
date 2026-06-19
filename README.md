# AudioSilo Server

A self-hosted audiobook server written in Go. It exposes a **JSON API**, a small
**baked-in admin/connect web UI**, and (optionally) the **web player at `/web`**,
and is designed to be **safe to leave exposed to the internet**: no default
passwords, app-layer hardening (rate limiting + brute-force lockout), and
configurable TLS.

The audiobook **player frontend** lives in a separate project
(`audiosilo-frontend`, an Expo app that ships as native iOS/Android **and** a web
build). The server does **not** vendor it: the Docker image bakes a pinned web
build in at `/app/web`, served from `web_dir`. A metadata-enrichment site is
planned separately.

## Features (this iteration)

- **Connect flow with copy-invite + app-or-web QR** — admins mint an auth code or
  click **Copy invite** for a shareable link (`/connect#code=…`, the code rides in
  the URL fragment so it never hits server logs). The connect screen shows a QR
  encoding an HTTPS link plus **Open in app** / **Open web player**: scanning opens
  the native app (when it claims the domain) or the web player, which exchanges a
  single-use pairing token for a device-scoped session. Username/password login is
  also supported.
- **Web player at `/web`** — the audiosilo-frontend web build, served from
  `web_dir` (not vendored; baked into the Docker image). Reports a `web_player`
  capability flag via `GET /api/v1/server`.
- **Path is the identity** — content is addressed by `(library, path)`, not a
  fragile database id. The filesystem is the source of truth (audiobook metadata
  is often junk); playback, progress, bookmarks and sharing all key on the path,
  so they survive DB rebuilds and re-tagging. A cheap fingerprint provides
  **move-tracking**: rename a file and its progress follows it.
- **Filesystem-based sharing** — a **share** is a named set of filesystem paths
  (a whole author, one series, a single book, or a whole library). Grant shares
  to users for partial access; they browse a **filtered tree** scoped to what
  they're allowed, and the computed view + search are scoped to match.
- **Admin** — create users, create libraries, build/grant shares, mint auth
  codes, edit library layout, trigger rescans.
- **Three library views** — **Filesystem** (browse as-is, instant, no indexing),
  **Computed** (catalog from tags/path), **Hybrid** (default; filesystem enriched
  with indexed metadata). Storage layouts: `flat`, `chapters_in_folder`,
  `books_in_folder`.
- **Fast search** — SQLite FTS5, fast at thousands of books; **keyset pagination**.
- **Streaming** — HTTP Range (seek/scrub) by path, direct download, cover art.
- **Normalized chapters** — single-file m4b chapters and multi-file mp3 parts
  share one shape; each chapter carries the `file_path` to play.
- **Per-user listening state** — progress, bookmarks, notes, history, playback
  speed, with last-write-wins reconciliation (the basis for realtime sync).
- **Baked-in web UI** — a public connect page at `/` (and `/connect`) and an admin
  console at `/admin` (users, libraries incl. layout edit/delete, **shares** with a
  filesystem path picker, auth codes + copy-invite, rescans). Vanilla HTML/CSS/JS,
  no build step, served from the same binary.

Planned next: upload + placement suggestions and AAX→M4B conversion (Phase B);
on-the-fly transcoding and WebSocket realtime sync (Phase C); server federation
(Phase D, designed).

## Requirements

- Go 1.25+ (to build from source), or Docker (to run the published image).
- **ffmpeg/ffprobe** (optional but recommended) — used for durations, chapters,
  and (later) transcoding/AAX. Without it the server still runs; durations and
  chapters are simply unavailable. (The Docker image includes ffmpeg.)

## Quick start

```sh
# Build
go build -o bin/audiosilo ./cmd/audiosilo

# First run: prints the admin password + auth code exactly once. Save them.
# Default TLS is a self-signed cert (HTTPS); for plain HTTP locally use TLS off:
AUDIOSILO_TLS_MODE=off ./bin/audiosilo --data ./data

# In another terminal — discover the server (public):
curl http://localhost:8080/api/v1/server

# Log in (or redeem the auth code) to get a session token:
curl -X POST http://localhost:8080/api/v1/auth/login \
  -d '{"username":"admin","password":"<printed password>"}'

# Add a library (admin), then browse it immediately via the filesystem view:
TOKEN=...   # from the login response
curl -X POST http://localhost:8080/api/v1/admin/libraries -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Main","root":"/path/to/audiobooks","layout":"books_in_folder"}'
curl "http://localhost:8080/api/v1/libraries/1/fs" -H "Authorization: Bearer $TOKEN"
```

Flags: `--data <dir>` (config, database, certs), `--ffprobe <path>` (`""` to
disable). Configuration and overrides are documented in
[`config.example.yaml`](config.example.yaml).

## Connect flow (auth code → QR → session)

1. `POST /api/v1/auth/redeem` with `{"code":"<auth code>"}` → returns a single-use
   `pairing_token`, a `web_url` (`https://<base>/web/connect?token=…`, encoded in
   the QR), a `uri` (`audiosilo://connect?server=…&token=…`, the "Open in app"
   deep link), and a QR PNG data URI.
2. The client (native app or the web player's `/connect` route) calls
   `POST /api/v1/auth/exchange` with the pairing token to obtain a durable,
   device-named session token. The pairing token is single-use.
3. All other endpoints take `Authorization: Bearer <session token>`.

The admin **Copy invite** button wraps step 0: it mints an auth code and gives you
`https://<base>/connect#code=<auth code>`, which drops the recipient onto the
connect screen with the code pre-filled. For native auto-launch from the QR,
configure `app_links` so the server publishes the matching
`/.well-known/apple-app-site-association` and `/.well-known/assetlinks.json`.

## Web player (`/web`)

When `web_dir` points at a built web player (the Expo web export from
`audiosilo-frontend`, built with `baseUrl=/web`), the server serves it at `/web`
with an SPA fallback and a scoped CSP. It is **not** stored in this repo or the
binary — the Docker image bakes a pinned build in at `/app/web`. Empty `web_dir` →
`/web` is unmounted and `web_player` is `false` in `GET /api/v1/server`.

For local development without Docker, build the export into a directory and point
the server at it:

```sh
scripts/build-web.sh                                   # exports into <frontend>/dist, prints the env to set
AUDIOSILO_WEB_DIR=~/dev/audiosilo-frontend/dist ./bin/audiosilo --data ./data
```

## Exposing to the internet safely

TLS is configurable (see `tls.mode` in the config):

- `selfsigned` (default) — generates and persists a self-signed cert. Good for a
  LAN; clients must accept it.
- `autocert` — automatic Let's Encrypt certificates (set `tls.hosts`). Needs a
  public hostname reachable on :443.
- `off` — plain HTTP for use **behind a reverse proxy** (Caddy/nginx/Traefik).
  Set `trusted_proxies` so client IPs (used for rate limiting) are accurate.

App-layer protections are always on: per-IP request rate limiting, brute-force
lockout on login and auth-code redemption, argon2id password hashing, hashed
(revocable) tokens, strict path-traversal containment, and body-size limits.

## Deploying with Docker

The published image bundles the server **and** a pinned web player. Web assets are
app code (they live in the image); only `/data` (db, config, certs) is persisted.

```sh
docker compose up -d           # see docker-compose.yml
docker compose logs            # first run prints the admin password + auth code ONCE
```

Edit `docker-compose.yml` to mount your audiobooks read-only, persist `./data`, and
set `AUDIOSILO_PUBLIC_URL` (used to build QR/invite links). Update — server or the
bundled player — is always a new image: `docker compose pull && docker compose up -d`.

## Testing

```sh
go test ./...
```

Tests use in-memory SQLite and the fixtures under [`testdata/`](testdata/) (tiny
generated M4B files), covering auth, the catalog/search/pagination, the scanner,
path-traversal defense, and the HTTP auth flow.

## Architecture & maintainers

See [CLAUDE.md](CLAUDE.md) for the package layout, data model, and roadmap, and
[RELEASING.md](RELEASING.md) for building/publishing the container images and the
end-to-end smoke test.
