# AudioSilo Server

A self-hosted audiobook server written in Go. It exposes a **JSON API** plus a
small **baked-in admin/connect web UI**, and is designed to be **safe to leave
exposed to the internet**: no default passwords, app-layer hardening (rate
limiting + brute-force lockout), and configurable TLS.

This repository is iteration 1 — the **server**. The audiobook *player* frontend
(web/mobile) and a metadata-enrichment site are planned separately; only the
admin/connect UI ships in this binary.

## Features (this iteration)

- **Auth-code connect flow** — enter an auth code, receive a QR pairing code for
  mobile, exchange it for a device-scoped session token. Username/password login
  is also supported.
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
- **Baked-in web UI** — a public connect page at `/` (auth code → QR) and an
  admin console at `/admin` (users, libraries incl. layout edit/delete, **shares**
  with a filesystem path picker, auth codes, rescans). Vanilla HTML/CSS/JS, no
  build step, served from the same binary.

Planned next: upload + placement suggestions and AAX→M4B conversion (Phase B);
on-the-fly transcoding and WebSocket realtime sync (Phase C); server federation
(Phase D, designed).

## Requirements

- Go 1.25+
- **ffmpeg/ffprobe** (optional but recommended) — used for durations, chapters,
  and (later) transcoding/AAX. Without it the server still runs; durations and
  chapters are simply unavailable.

## Quick start

```sh
# Build
go build -o bin/audiosilo ./cmd/audiosilo

# First run: prints the admin password + auth code exactly once. Save them.
./bin/audiosilo --data ./data

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

## Connect flow (auth code → QR)

1. `POST /api/v1/auth/redeem` with `{"code":"<auth code>"}` → returns a pairing
   token, a `audiosilo://pair?...` deep link, and a QR PNG data URI.
2. A mobile client scans the QR and calls `POST /api/v1/auth/exchange` with the
   pairing token to obtain a durable, device-named session token.
3. All other endpoints take `Authorization: Bearer <session token>`.

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

## Testing

```sh
go test ./...
```

Tests use in-memory SQLite and the fixtures under [`testdata/`](testdata/) (tiny
generated M4B files), covering auth, the catalog/search/pagination, the scanner,
path-traversal defense, and the HTTP auth flow.

## Architecture

See [CLAUDE.md](CLAUDE.md) for the package layout, data model, and roadmap.
