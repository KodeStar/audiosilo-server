# CLAUDE.md — AudioSilo Server

Guidance for working in this repository. Keep this file updated as the codebase
evolves.

## What this is

A self-hosted **audiobook server** in Go. **API only** — no bundled web UI (the
frontend ships separately). It must be **safe to expose to the internet** for
inexperienced users: secure defaults, no default passwords, app-layer hardening,
configurable TLS.

Module path: `github.com/kodestar/audiosilo-server`.

## Build / test / run

```sh
go build ./...                 # build everything
go vet ./...                   # static checks
go test -race ./...            # unit + integration tests (in-memory SQLite + testdata fixtures)
golangci-lint run              # lint (v2 required for Go 1.25; config .golangci.yml)
go build -o bin/audiosilo ./cmd/audiosilo
./bin/audiosilo --data ./data  # first run prints admin creds + auth code ONCE

AUDIOSILO_WEB_DIR=… ./bin/audiosilo  # serve the web player at /web from that dir
scripts/build-web.sh                 # dev helper: build the frontend export locally (prints the env to set)
```

Flags: `--data` (config/db/certs dir), `--ffprobe` (`""` disables ffprobe),
`--ffmpeg` (`""` disables on-the-fly transcoding).

**Before a change is done, run `go build ./... && go vet ./... && go test -race ./...
&& golangci-lint run`** — CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml))
gates all four on every PR/push. A few scanner tests need `ffmpeg` (ffprobe);
without it they `t.Skip` (CI installs it). The linter is adopted at a **green
baseline** — its suppressions in `.golangci.yml` are documented and intentional;
fix new findings rather than widening the excludes.

## Design priorities (in order)

1. Safe to expose to the internet.
2. Fast regardless of library size (FTS5 + keyset pagination).
3. No-wait first connection (the filesystem view needs no indexing).
4. Portable: the filesystem is the source of truth for content; the database is
   a **rebuildable** index/cache. Never put content only in the DB.

## Package layout

```
cmd/audiosilo/        entrypoint: flag wiring, first-run bootstrap, library sync, startup scan
internal/config/      YAML + env config, validation, secure defaults
internal/store/       SQLite (modernc, pure Go) open + embedded migrations (internal/store/migrations)
internal/auth/        users, argon2id, opaque hashed tokens, auth codes; hash.go has the crypto
internal/catalog/     libraries, access grants, books, FTS search, listening state (the data layer)
internal/library/     filesystem view (fsview.go) + background scanner (scanner.go)
internal/metadata/    dhowden/tag + ffprobe extraction; DeriveFromPath (structural path parsing)
internal/media/       Range streaming, download, embedded cover extraction
internal/api/         HTTP transport: routing (api.go), middleware, rate limiting, handlers_*.go
internal/server/      HTTP(S) server, TLS modes (off/selfsigned/autocert), graceful shutdown
internal/web/         baked-in admin/connect UI (vanilla HTML/CSS/JS, no build step);
                      also serves the web player at /web from web_dir (not vendored here)
testdata/library/     tiny generated M4B fixtures used by tests
Dockerfile            multi-stage build that bakes a pinned web build into /app/web
scripts/build-web.sh  dev helper: build the frontend export locally for AUDIOSILO_WEB_DIR
```

Dependency direction: `api` → (`auth`, `catalog`, `library`, `media`, `config`);
`library` → (`catalog`, `metadata`, `config`); everything DB-backed → `store`.
`api` is transport-only — keep business logic out of handlers.

## Identity = the filesystem path

Audiobook metadata is unreliable, so **the path is the identity**. Content is
addressed by `(library_id, rel_path)`; playback, progress, bookmarks, notes and
share membership all key on the path. `books.id` is an internal, rebuildable
index artifact — never put it in the API contract or in durable user state. A
cheap fingerprint (sha256 of size + first/last 64KB, stored in `books.content_hash`)
is used **only** to detect moves; it is not an identity.

## Data model (SQLite, see internal/store/migrations/)

`users`, `tokens` (sessions + pairing, hashed), `auth_codes`, `libraries` (no
`layout` column — shape is auto-detected), `books` (+ `content_hash` fingerprint,
+ `codec`), `book_files`, `chapters` (with `file_path`), `books_fts` (standalone
FTS5). Durable user state is **path-keyed** and decoupled from the index (no FK to
books): `progress`/`bookmarks`/`notes`/`listening_history` on `(user_id,
library_id, rel_path)`, plus `folder_overrides` (`library_id, path, mode`) which
pins a folder's book/collection classification. Sharing: `shares` (named),
`share_paths` (`library_id`, `path`; `""` = whole library), `user_share_access`.

Book identity carries `author`/`series`/`title` plus optional `asin`/`isbn` so a
future metadata site can attach enrichment without reshaping the schema.

## Conventions

- **Every feature ships with a test.** Handler/integration tests use the
  `newTestEnv` harness in `internal/api/api_test.go` (in-memory SQLite +
  `testdata/library` fixtures); pure-logic tests sit next to the code (see
  `internal/api/middleware_test.go`, `internal/catalog/shares_test.go`,
  `internal/web/web_test.go`). **Security-critical code requires both an allowed
  and a denied regression test** — anything touching `library.SafeJoin`,
  `Scope.Allows`/`VisibleInBrowse`/`pathFilterSQL`, the rate limiters,
  `auth.ResolveToken`, or `web.htmlCSP`. Keep business logic in the non-`api`
  packages so it stays unit-testable (`api` is transport-only).
- **Migrations are append-only**: add `internal/store/migrations/000N_*.sql`;
  never edit an applied migration. Applied names are tracked in `schema_migrations`.
- **Secrets** (tokens, auth codes) are stored only as SHA-256 hashes; passwords
  use argon2id (`auth/hash.go`). Never log or persist plaintext secrets; the
  first-run banner prints them once and is the only place they appear.
- **Connect / invite / pairing flow** (`internal/web` connect page + `api/qr.go`):
  the admin's **Copy invite** button mints an auth code and shares
  `<base>/connect#code=...` — the code rides in the URL **fragment** so it never
  reaches the server or its logs. The connect page auto-redeems a fragment code,
  showing a QR plus **Open in app** / **Open web player** buttons. `buildPairing`
  emits two carriers for the single-use pairing token: `web_url`
  (`<base>/web/connect?token=` — encoded in the QR; opens the app via a Universal/
  App Link when the domain is claimed, else the embedded web player) and `uri`
  (`audiosilo://connect?...` — custom scheme, launches an installed app on any
  domain). Invite codes minted via the admin API default to 5 uses / 1-day
  expiry (`defaultAuthCode*` in `handlers_admin.go`); explicit values override.
- **Invite vs recovery (`auth_codes.kind`)**: an auth code is either an admin-minted
  `invite` (bounded) or a user-owned `recovery` code (durable: unlimited uses, never
  expires). Both redeem through the same `RedeemAuthCode` → pairing → exchange path;
  `RedeemAuthCode` resolves and rejects a disabled/deleted user **before** consuming a
  use, and folds the first-redemption `redeemed_at` stamp into the atomic claim, so a
  rejected attempt never burns a use or marks an invite accepted. Recovery decouples
  re-auth from invitation: a signed-out/password-less user mints a recovery code from
  the player's Settings (`POST /auth/recovery`) and re-pairs without an admin. Recovery
  mint/redeem and `POST /auth/password` are gated by `accountLimiter` and **refused for
  demo accounts** (`User.IsDemo`) so a throwaway session can't forge a durable login.
  `ListAuthCodes` returns only invites; recovery presence surfaces as
  `User.HasRecovery`, and the admin can revoke a leaked one via `DELETE
  /admin/users/{id}/recovery` (`ClearRecoveryCode`) — the only lever, since recovery
  codes aren't listable. **Invite hygiene**: `CreateInvite` mints and, in one
  transaction, supersedes the user's other *still-redeemable* invites
  (`supersedeActiveInvites` — not expired, not used-up) so there's exactly one active
  invite each; spent/expired ones stay as history. `POST /admin/authcodes/{id}/rotate`
  (`RotateAuthCode`) regenerates an invite's secret in place (the admin "Resend"),
  **preserving** its `max_uses` and renewing its expiry for the original window (never
  silently downgrading to defaults); `redeemed_at` records acceptance but the console
  buckets invites by whether they are still redeemable, not by `redeemed_at`. **Self-
  service password**: `POST /auth/password` reuses `SetPassword`; setting a first
  password needs no challenge, but changing an existing one requires `current_password`
  (`CheckPassword`), an empty password is rejected (clearing is admin-only), and the
  admin-must-keep-a-password guard still holds.
- **Web player at `/web`** (`web.go`, served from `cfg.WebDir`): a separate Expo
  Router project (`~/dev/audiosilo/audiosilo-frontend`) exported as a static site. It is
  **not vendored** in this repo or the binary — the server serves it at runtime
  from `web_dir` (env `AUDIOSILO_WEB_DIR`), which the Docker image bakes in at
  `/app/web` from a pinned prebuilt frontend image (see `Dockerfile`). Empty
  `web_dir` → `/web` is unmounted and the `web_player` capability is false. The
  export must be built with `baseUrl=/web` (frontend `app.json experiments.baseUrl`)
  so asset URLs resolve under the subpath. The handler resolves per-route HTML,
  falls back to `index.html` for client-routed deep links, 404s missing assets,
  and sets a **scoped CSP** per HTML response (strict `script-src` with a sha256
  hash of that doc's inline scripts; `style-src` allows `'unsafe-inline'` for
  react-native-web's runtime styles). Admin/connect pages keep the stricter
  site-wide CSP. Compatibility is by construction (the image pins a matching web
  build); native apps negotiate via `GET /server` capability flags.
- **Native deep-link association**: `GET /.well-known/apple-app-site-association`
  and `/assetlinks.json` are served from `config.AppLinkConfig` (`app_links` in
  YAML) and 404 when unset. They only enable auto-app-launch for domains the
  shipped app build claims — self-hosted arbitrary domains fall back to the web
  player + the custom-scheme "Open in app" button.
- **SQLite** runs with a single open connection (writers serialize) + WAL.
- **Pagination** is keyset/cursor-based (`catalog.ListBooks`); don't switch list
  endpoints to OFFSET for large tables.
- **Path safety**: any filesystem access derived from user input goes through
  `library.SafeJoin`, which rejects traversal outside the library root.
- **Path-addressed API**: content endpoints are `GET /libraries/{id}/{item,
  chapters,cover,stream}?path=` and `{GET,PUT} .../progress?path=` etc. The path
  is the handle (a query param, to avoid encoded-slash issues). `item`/`chapters`/
  `cover` resolve `(library, path)` to a book via `GetBookByPath`, indexing on
  demand (`Scanner.IndexPath`) if the scan hasn't reached it; `stream` serves an
  audio file path directly, or transcodes it with `?transcode=1` (see below). The
  `/fs` view lists **audio files and directories only** (non-audio like `.jpg`/
  `.nfo` are filtered in `BrowseFS` so a click is always playable) and annotates
  book entries with metadata (`is_book` + title/author/…); the client acts on the
  entry's `path`.
- **Recognized audio** is `metadata.AudioExtensions` (`IsAudio`), including `.mp4`
  (AAC-in-MP4 audiobooks); `media` serves it as `audio/mp4`.
- **Transcoding**: `GET /libraries/{id}/stream?path=…&transcode=1` pipes the file
  through ffmpeg to MP3 (`media.Transcode`) for codecs browsers can't decode;
  `&t=<seconds>` starts mid-file (transcoded output isn't byte-seekable, so seeks
  re-request). Direct serving + Range stays the default. The scanner records each
  book's audio `codec` (ffprobe `codec_name`, `0008` migration); `item`/`chapters`
  expose `direct_playable` (via `media.DirectPlayable`) so a client knows when to
  transcode. Gated by the `--ffmpeg` flag (the `transcode` capability reflects it).
- **Sharing & access (shares)**: access is via `shares` = named sets of path
  rules. `catalog.UserScope`/`UserScopes` build a `Scope` per library
  (`AllowAll` or specific `Paths`); `Scope.Allows` gates item endpoints,
  `Scope.VisibleInBrowse` filters `/fs` to a navigable subtree, and
  `pathFilterSQL` scopes `ListBooks`/`Search`. Every content handler authorizes
  the path against the caller's scope (`authorizedPath`). Admins are `AllowAll`.
  Whole-library access is sugar (`GrantWholeLibrary` → a `""`-rule share).
- **Move-tracking**: the scanner fingerprints files; when a path vanishes and a
  new path with a matching fingerprint appears, `Scanner.detectMoves` migrates
  durable state old→new (`catalog.MoveDurableState`). Re-tagging keeps state via
  the path key; moving keeps it via the fingerprint.
- **Auto book/folder detection**: there is **no per-library layout**. The model
  (`booksInDir` in `library/scanner.go`) matches the dominant "folder per book"
  convention (and Audiobookshelf): **a directory that directly contains audio is
  ONE book**, with all those files as its tracks/chapters — whether it holds a
  single m4b or fifty distinctly-named mp3 chapters (do NOT split a folder's files
  into separate books by filename; that produced one phantom book per chapter).
  The only per-file case is the **library root** (loose files there are individual
  single-file books — the old "flat"). A folder of loose single-file books
  (`books_in_folder`) is expressed with a **per-folder override**:
  `folder_overrides(library_id, path, mode)`, `mode ∈ {book, collection}` —
  `collection` = one book per file, `book` = force folder-is-one-book. Overrides
  are durable, path-keyed config (no FK to the rebuildable index, like
  progress/bookmarks). `PUT/DELETE /admin/libraries/{id}/folder-override?path=`
  sets/clears it and rescans; the admin console's per-library **Detection** browser
  drives it. `GET /fs` annotates each entry's effective `override`.
- **Library admin**: `PATCH /admin/libraries/{id}` edits name/root/default_view and
  triggers a background rescan; `DELETE /admin/libraries/{id}` removes the library
  + its index (files on disk untouched). Both are surfaced in the admin console.
- **User/account admin**: `PATCH /admin/users/{id}` edits role/password/disabled
  in place (`auth.SetRole`/`SetPassword`/`SetDisabled`) — no delete-and-recreate.
  Two safety guards live in `auth`: the **last enabled admin** can't be demoted or
  disabled (`ErrLastAdmin`), and an admin must keep a password (`ErrAdminNeedsPassword`).
  **Passwords are optional for non-admins** (stored as an empty hash; `Authenticate`
  rejects empty-hash accounts) — player-only users onboard purely via auth-code
  pairing. `GET /admin/users/{id}` returns a user + accessible libraries + granted
  shares + issued auth codes (metadata only; codes are unretrievable by design);
  `DELETE /admin/authcodes/{id}` revokes a code. A user's **last activity** is
  derived from `MAX(tokens.last_seen)` (bumped on every authenticated request in
  `ResolveToken`) — there is no `last_login` column; don't add one.
- **Admin stats**: `GET /admin/stats` returns catalog totals, per-library book
  counts (`catalog.CountBooksByLibrary`) and a cross-user "currently listening"
  feed (`catalog.ListeningOverview`, progress LEFT-joined to books on the path).
- **Progress reconciliation** is last-write-wins by `updated_at` (version breaks
  ties) in `catalog.SaveProgress` — the realtime layer (Phase C) must reuse it so
  REST and WebSocket writes converge.
- **Chapters are normalized** (`metadata.Chapter`) so single-file m4b chapters and
  multi-file mp3 parts share one shape: each chapter carries `file_path` (the
  library-relative file to stream via `/stream?path=`), in-file `start`/`end`, and
  `book_offset` (its start on the whole-book timeline). For folder books the
  scanner probes each part and, if a part has its own embedded chapters (a single
  chaptered m4b in its own book folder), expands those; otherwise the whole part
  becomes one chapter. `GET /libraries/{id}/chapters?path=` returns
  `{chapters, files, duration}`; a player renders single- and multi-file books
  identically.
- ffprobe is optional; code paths must degrade gracefully when it is absent
  (path-derived metadata still works; codec is left unknown → `direct_playable`
  defaults to true). ffmpeg is likewise optional (transcoding off without it).
- **Unavailable-root guard**: the scanner aborts with `ErrLibraryUnavailable`
  (and does NOT prune) if a library root is missing/unreadable, or if it returns
  zero audio files while books are still indexed. This protects the index — and
  the progress/bookmarks that cascade from it — when a network share (SMB/NFS)
  is unmounted. Library roots are always local paths; mount remote shares first.

## Roadmap

- **Phase A (done)**: auth/QR, admin, 3 views, scanner, FTS search, pagination,
  Range streaming, per-user listening state.
- **Phase A.1 (done)**: baked-in web UI (`internal/web`) — public connect page
  (auth-code box → QR + links) and an admin console (login, users, libraries,
  access grants, auth codes, rescan, folder-detection overrides, delete). Static client over the
  JSON API; the API enforces the admin role, so the HTML itself is unprivileged.
  The console takes its design cues from the Expo player (pink `#db2777` accent,
  self-hosted Roboto in `assets/fonts/`, dark-mode-first, logo + wordmark): a
  sidebar-section layout (Overview/Stats, Libraries, Users, Shares) with forms in
  modals and a per-user detail drawer (role/password/disable, access, invite-code
  status). All styling lives in `assets/style.css` and all behaviour in external
  JS — no inline `<style>`/`style=`/`<script>`, so the strict same-origin CSP holds.
- **Phase A.3 (done)**: **copy-invite** links (fragment-carried auth code →
  auto-redeem connect screen), app-or-web QR (HTTPS `web_url` for Universal/App
  Links + `audiosilo://` custom scheme), and the **web player** served at `/web`
  from `web_dir`. The player is the audiosilo-frontend Expo export, baked into the
  Docker image (pinned, not vendored in this repo); updates ship as a new image.
- **Phase A.2 (done)**: path identity + **filesystem-based shares** (named sets of
  path rules; filtered-tree browse; whole-library sugar), durable state re-keyed to
  the path, and cheap **move-tracking**. Admin console has a **Shares** section with
  a filesystem path picker.
- **Phase B**: `POST /uploads` → parse + placement suggestion; AAX→M4B conversion
  (user-supplied activation bytes, never stored).
- **Phase C**: `?transcode=` on the stream endpoint (ffmpeg pipe to MP3) — **done**
  (see Transcoding above). Remaining: WebSocket `/api/v1/ws` realtime sync reusing
  the last-write-wins merge + offline replay.
- **Phase D (designed)**: server federation — peering, remote shelves, hybrid
  routing (proxy catalog + signed direct stream), reusing shares as the share unit.
  See the plan file.

`GET /api/v1/server` advertises capability flags (`admin_ui`, `web_player`,
`upload`, `transcode`, `websocket`); flip them on as phases land. `transcode`
already reflects whether ffmpeg is configured.

## API surface

See `internal/api/api.go` for the full route table. Public: `/server`,
`/auth/redeem`, `/auth/exchange`, `/auth/login`, the well-known association files,
and the static UI (`/`, `/connect`, `/admin`, `/web/...`). Everything else needs a
session bearer token; `/admin/*` additionally requires the admin role.
