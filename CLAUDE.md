# CLAUDE.md - AudioSilo Server

Guidance for working in this repository. Keep this file updated as the codebase
evolves.

## What this is

A self-hosted **audiobook server** in Go: a JSON API plus a **baked-in
admin/connect web UI**; the separate player frontend is served at `/web` from
`web_dir` (not vendored). It must be **safe to expose to the internet** for
inexperienced users: secure defaults, no default passwords, app-layer hardening,
configurable TLS.

Module path: `github.com/kodestar/audiosilo-server`.

## Build / test / run

```sh
go build ./...                 # build everything
go vet ./...                   # static checks
go test -race ./...            # unit + integration tests (in-memory SQLite + testdata fixtures)
golangci-lint run              # lint (v2 required since Go 1.25; config .golangci.yml)
go build -o bin/audiosilo ./cmd/audiosilo
./bin/audiosilo --data ./data  # first run prints admin creds + auth code ONCE

AUDIOSILO_WEB_DIR=… ./bin/audiosilo  # serve the web player at /web from that dir
scripts/build-web.sh                 # dev helper: build the frontend export locally (prints the env to set)

scripts/build-admin.sh               # the admin console's gate + build (Node 24): npm ci, check, build into internal/web/adminui/dist
```

Flags: `--data` (config/db/certs dir), `--ffprobe` (`""` disables ffprobe),
`--ffmpeg` (`""` disables on-the-fly transcoding), `--setup` (first-run **web
setup wizard** instead of the auto-admin banner - see below).

**Native distribution** (GoReleaser → GitHub Releases on `v*` tags, see
`.goreleaser.yml` + `.github/workflows/release.yml`, and the workspace
`DISTRIBUTION.md`): the binary is CGO-free so it cross-compiles everywhere. Two
build-time knobs make it self-contained for home users: `-tags embedplayer` bakes
the web player into the binary (`internal/web/player/`, gitignored, populated from
the pinned web image by `scripts/fetch-web-player.sh`) so `/web` works with no
`web_dir`. **ffmpeg/ffprobe are NOT bundled** (large, and usually already present):
`pkg/launcher.resolveTools` prefers a local copy (explicit `--ffmpeg`/`--ffprobe`
path → next to the binary → `$PATH`) and, only if none is found, auto-downloads a
cached static build into `<data>/tools` (`internal/toolfetch`, HTTPS, self-checked
by running `-version`; degrades gracefully offline and retries next start). Native
GUI installers + a system-tray launcher are a **planned follow-up** (see
DISTRIBUTION.md).

**First-run setup wizard** (`--setup`, intended default for a future GUI launcher):
instead of auto-creating the admin and printing credentials, the server mints a
one-time setup token and enables a guarded browser wizard at `/setup` (handlers in
`internal/api/handlers_setup.go`, page in `internal/web/assets/setup.{html,js}`).
The wizard sets the admin password and the books folder; the token rides in the URL
**fragment** (`/setup#token=…`, never logged), POST verifies it in constant time,
and the wizard **self-closes the moment an admin exists** (404 when never enabled).
`API.EnableSetup(token)` turns it on; `pkg/launcher` prints the URL and reports it
via `Options.OnURL` (so the audiosilo-manager desktop app, which runs the server
in-process, can open a browser).

**Before a change is done, run `go build ./... && go vet ./... && go test -race ./...
&& golangci-lint run`**, plus **`scripts/build-admin.sh`** (npm ci, then `npm run check` =
typecheck + eslint + prettier + vitest, then the build) when `admin-ui/` changed - CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml))
gates all of them on every PR/push, building the console first so the embed tests in
`internal/web/adminui` run against a real build (locally they skip without one). A few scanner tests need `ffmpeg` (ffprobe);
without it they `t.Skip` (CI installs it). The linter is adopted at a **green
baseline** - its suppressions in `.golangci.yml` are documented and intentional;
fix new findings rather than widening the excludes.

> Before adding code, read the workspace **[CODE-HEALTH.md](../CODE-HEALTH.md)** -
> Definition of Done + the recurring drift patterns (wire-contract, dead code,
> stale docs, untested packages) a full review found. Conventions only help if
> they're checked; that file is the checklist.

## Documentation

The product docs live in [`../audiosilo-docs`](../audiosilo-docs/) (Docusaurus:
User Guide + Developer Docs, generated screenshots). **Updating them is part of
Definition of Done**: a change here that touches behaviour, UI, config, or the
wire format updates the affected pages in the same logical change - for this
repo that's chiefly `docs-developers/server/**` (the **API reference**
`server/api/reference.md` on every wire change, `server/configuration.md` on
any config/flag change) plus the User Guide's getting-started/admin pages, and
regenerated screenshots (`audiosilo-docs/screenshots/run.sh`) when the baked-in
admin/connect UI changes. Mapping table:
`audiosilo-docs/docs-developers/contributing/documentation.md`. Docs gate:
`npm run build` in audiosilo-docs.

## Design priorities (in order)

1. Safe to expose to the internet.
2. Fast regardless of library size (FTS5 + keyset pagination).
3. No-wait first connection (the filesystem view needs no indexing).
4. Portable: the filesystem is the source of truth for content; the database is
   a **rebuildable** index/cache. Never put content only in the DB.

## Package layout

```
cmd/audiosilo/        entrypoint: flag wiring; delegates to pkg/launcher.Run
pkg/launcher/         shared run loop (config→store→services→bootstrap→serve); PUBLIC so the audiosilo-manager desktop app runs it in-process (Run/Options)
pkg/match/            PUBLIC fuzzy book matcher (Best/CleanTitle/SeqFromTitle) - same-book identification across messy titles; shared with the manager, usable for server-side enrichment/dedup
internal/config/      YAML + env config, validation, secure defaults
internal/store/       SQLite (modernc, pure Go) open + embedded migrations (internal/store/migrations)
internal/auth/        users, argon2id, opaque hashed tokens, auth codes; hash.go has the crypto
internal/catalog/     libraries, access grants, books, FTS search, listening state (the data layer)
internal/library/     filesystem view (fsview.go) + background scanner (scanner.go)
internal/metadata/    dhowden/tag + ffprobe extraction; DeriveFromPath (structural path parsing)
internal/media/       Range streaming, download, embedded cover extraction
internal/meta/        Phase 1.5 community metadata lookup: HTTP client + Service (asin/isbn -> composed enrichment envelope) with a bounded TTL cache; the admin console's match (match.go: metaserve works/match over tag + path facts, pathfacts.go; works/search fallback for an older metaserve); owned books' work ids for the Series cards (workids.go)
internal/toolfetch/   on-demand ffmpeg/ffprobe download+cache (<data>/tools) when none is local; Version reads a tool's -version
internal/logring/     the admin console's log viewer: an slog handler teeing records into a bounded in-memory ring (secrets redacted)
internal/updates/     the update check: GitHub Releases' latest release, once a day while on (config update_check)
internal/backup/      database backups: VACUUM INTO the backups folder on a schedule or on request, retention, restore applied at the next start
internal/notify/      the event feed (the console's bell) and its deliveries to webhook / ntfy / Discord destinations
internal/api/         HTTP transport: routing (api.go), middleware, rate limiting, handlers_*.go
internal/server/      HTTP(S) server, TLS modes (off/selfsigned/autocert), graceful shutdown
internal/web/         baked-in connect/setup pages (vanilla HTML/CSS/JS, no build step), mounts the
                      admin console at /admin and the web player at /web from web_dir (not vendored here)
internal/web/adminui/ embeds the admin console (admin-ui/ build output in dist/, gitignored)
internal/web/spa/     the one SPA file handler the console and the player share (caching, MIME, deep links)
admin-ui/             the admin console: React 19 + Vite + TS, shadcn/ui on Base UI, Tailwind v4 (own README + STYLEGUIDE.md)
testdata/library/     tiny generated M4B fixtures used by tests
Dockerfile            multi-stage build: builds admin-ui (node stage), bakes a pinned web build into /app/web
scripts/build-admin.sh  build the admin console locally (npm ci + check + build) before go build
scripts/build-web.sh  dev helper: build the frontend export locally for AUDIOSILO_WEB_DIR
```

Dependency direction: `api` → (`auth`, `catalog`, `library`, `media`, `config`);
`library` → (`catalog`, `metadata`, `config`); everything DB-backed → `store`.
`api` is transport-only - keep business logic out of handlers.

## Identity = the filesystem path

Audiobook metadata is unreliable, so **the path is the identity**. Content is
addressed by `(library_id, rel_path)`; playback, progress, bookmarks, notes and
share membership all key on the path. `books.id` is an internal, rebuildable
index artifact - never put it in the API contract or in durable user state. A
cheap fingerprint (sha256 of size + first/last 64KB, stored in `books.content_hash`)
is used **only** to detect moves; it is not an identity.

## Data model (SQLite, see internal/store/migrations/)

`users`, `tokens` (sessions + pairing, hashed; pairing tokens minted from an
auth code carry `auth_code_id` - `0014` migration - so they live and die with
the code), `auth_codes`, `libraries` (no
`layout` column - shape is auto-detected), `books` (+ `content_hash` fingerprint,
+ `codec`), `book_files`, `chapters` (with `file_path`), `books_fts` (standalone
FTS5). Durable user state is **path-keyed** and decoupled from the index (no FK to
books): `progress`/`bookmarks`/`notes`/`listening_history` on `(user_id,
library_id, rel_path)`, plus `folder_overrides` (`library_id, path, mode`) which
pins a folder's book/collection classification, and `book_enrichment`
(`library_id, path, asin, isbn`) which attaches metadata to a book (set by the
manager when it matches an external source) - both durable, path-keyed, no FK to
the index. The admin console's metadata edits are likewise durable and path-keyed
(`0016`): `book_overrides` (`library_id, path, field, value, source, updated_by`),
`chapter_overrides` (keyed by the chapter's book-relative file + start in ms, not its
index, so a shifted chapter list can't move a rename; the API still sends indexes) and
`book_covers` (custom cover blobs). Phase 3 (`0017`) adds `scan_runs` (the job queue's scan
history, per library, bounded), `issue_ignores` (`library_id, path, kind`: an admin's "ignore this"
on a Health issue; path-keyed, moves with the book), `libraries.scan_schedule` / `ignore_patterns`
(per-library settings, off the player wire) and, on `books`, `scan_error` / `scan_error_file` /
`scan_error_detail` (the last indexing's read problem) and `suspect_parts`; `0022` adds `books.split_parent` (the
folder holding a disc of a book split across disc folders, else `''`). Phase 4a (`0018`) adds
`listening_sessions` (server-derived listening sessions, path-keyed, no FK to the index, bounded
retention), `listening_daily` (their per-day roll-up), `tokens.client_app` / `client_version` /
`client_platform` / `last_ip` (the app and newest address behind each token) and
`progress.started_at` / `finished_at`. Phase 5b (`0019`) adds `audit_events` (the admin audit log), `notification_targets`
(where notifications go) and `server_events` (the console's bell); backups are files, not rows. Sharing:
`shares` (named), `share_paths` (`library_id`, `path`; `""` = whole library),
`user_share_access`.

Book identity carries `author`/`series`/`title` plus optional `asin`/`isbn` so a
future metadata site can attach enrichment without reshaping the schema. The
`books` metadata columns are the effective values (scan, then enrichment, then
admin overrides; see Metadata overrides below).

## Conventions

- **Every feature ships with a test.** Handler/integration tests use the
  `newTestEnv` harness in `internal/api/api_test.go` (in-memory SQLite +
  `testdata/library` fixtures); pure-logic tests sit next to the code (see
  `internal/api/middleware_test.go`, `internal/catalog/shares_test.go`,
  `internal/web/web_test.go`). **Security-critical code requires both an allowed
  and a denied regression test** - anything touching `library.SafeJoin`,
  `Scope.Allows`/`VisibleInBrowse`/`pathFilterSQL`, the rate limiters,
  `auth.ResolveRequest`/`lookupToken`, or `web.htmlCSP`. Keep business logic in the non-`api`
  packages so it stays unit-testable (`api` is transport-only).
- **Migrations are append-only**: add `internal/store/migrations/000N_*.sql`;
  never edit an applied migration. Applied names are tracked in `schema_migrations`.
- **Secrets** (tokens, auth codes) are stored only as SHA-256 hashes; passwords
  use argon2id (`auth/hash.go`). Never log or persist plaintext secrets; the
  first-run banner prints them once and is the only place they appear.
- **Connect / invite / pairing flow** (`internal/web` connect page + `api/qr.go`):
  the admin's **Copy invite** button mints an auth code and shares
  `<base>/connect#code=...` - the code rides in the URL **fragment** so it never
  reaches the server or its logs. The connect page auto-redeems a fragment code,
  showing a QR plus **Open in app** / **Open web player** buttons. `buildPairing`
  emits two carriers for the pairing token: `web_url`
  (`<base>/web/connect?token=` - encoded in the QR; opens the app via a Universal/
  App Link when the domain is claimed, else the embedded web player) and `uri`
  (`audiosilo://connect?...` - custom scheme, launches an installed app on any
  domain). **The QR is as redeemable as the invite**: a pairing token minted by
  redeeming a code is linked to it (`tokens.auth_code_id`) and inherits the
  invite's uses/expiry, so each device being set up can scan the same QR (the
  redeem response's `uses_remaining`/`code_expires_at` advertise the budget);
  recovery-derived tokens instead last `auth.recoveryPairingTTL` (multi-scan
  within it), and
  `/auth/pair` + demo tokens stay unlinked/single-use. Invite codes minted via
  the admin API default to 5 uses / 1-day expiry (`defaultAuthCode*` in
  `handlers_admin.go`); explicit values override.
- **Invite vs recovery (`auth_codes.kind`)**: an auth code is either an admin-minted
  `invite` (bounded) or a user-owned `recovery` code (durable: unlimited uses, never
  expires). Both pair through the same `ResolveAuthCode` → `IssuePairingToken` →
  `ConsumePairing` path. **Redeem validates without consuming** (opening an
  invite link costs nothing); **exchange claims the use** - `ConsumePairing`
  folds the cap check, the code-expiry check and the first-claim `redeemed_at`
  stamp into one atomic UPDATE, and rejects a disabled/deleted user first, so a
  rejected attempt never burns a use or marks an invite accepted (`uses` counts
  devices that actually paired). Linked pairing tokens die with their code: delete/
  supersede/recovery-regeneration cascade (`ON DELETE CASCADE`), rotate revokes
  them explicitly. Recovery decouples
  re-auth from invitation: a signed-out/password-less user mints a recovery code from
  the player's Settings (`POST /auth/recovery`) and re-pairs without an admin. Recovery
  mint/redeem and `POST /auth/password` are gated by `accountLimiter` and **refused for
  demo accounts** (`User.IsDemo`) so a throwaway session can't forge a durable login.
  `ListAuthCodes` returns only invites; recovery presence surfaces as
  `User.HasRecovery`, and the admin can revoke a leaked one via `DELETE
  /admin/users/{id}/recovery` (`ClearRecoveryCode`) - the only lever, since recovery
  codes aren't listable. **Invite hygiene**: `CreateInvite` mints and, in one
  transaction, supersedes the user's other *still-redeemable* invites
  (`supersedeActiveInvites` - not expired, not used-up) so there's exactly one active
  invite each; spent/expired ones stay as history. `POST /admin/authcodes/{id}/rotate`
  (`RotateAuthCode`) regenerates an invite's secret in place (the console's "Rotate"),
  **preserving** its `max_uses` and renewing its expiry for the original window (never
  silently downgrading to defaults) and **revoking the invite's outstanding pairing
  tokens** (a QR already on screen dies with the old secret) and, like a mint, retiring
  the user's other still-redeemable invites; `redeemed_at` records
  acceptance (first successful exchange) but the console buckets invites by whether
  they are still redeemable, not by `redeemed_at`. **Self-
  service password**: `POST /auth/password` reuses `SetPassword`; setting a first
  password needs no challenge, but changing an existing one requires `current_password`
  (`CheckPassword`), an empty password is rejected (clearing is admin-only), and the
  admin-must-keep-a-password guard still holds.
- **Personal API keys (`tokens.kind='api'`)**: user-minted, **non-expiring** bearer
  tokens for headless integrations, owner-scoped mint/list/revoke via
  `POST`/`GET`/`DELETE /auth/tokens` (`IssueAPIToken`/`ListAPITokens`/
  `RevokeTokenByID`; label carried in `device_name`, ≤100 chars). `requireAuth` now
  accepts **session OR api** (`ResolveRequest(..., KindSession, KindAPI)`), so a key
  authenticates like a session acting as its owner (an admin's key passes
  `requireAdmin`; media `?token=` accepts it too) but is never valid for pairing
  `/auth/exchange`; a pairing token is never accepted as a bearer credential.
  Secrets are stored SHA-256-only and shown once; create/list/revoke go through
  `gateSelfService` (shared `accountLimiter` + demo refusal). **Containment**: a key
  authenticates as its owner but cannot mint a *fresh durable credential* -
  `ResolveRequest` returns the matched kind (`Credential.Kind`) and `denyAPIKey` refuses an api-key
  caller (403) on `POST /auth/{tokens,recovery,pair,password}`, so revoking a leaked
  key cuts off everything it could reach (it cannot spawn another key/recovery
  code/pairing token/password that outlives its own revocation - GitHub's "a token
  cannot create tokens"). It may still list/revoke keys and clear a recovery code
  (those only reduce access). Surfaced by the `api_keys` capability.
- **Web player at `/web`** (`web.go`, served from `cfg.WebDir`): a separate Expo
  Router project (`~/dev/audiosilo/audiosilo-frontend`) exported as a static site. It is
  **not vendored** in this repo or the binary - the server serves it at runtime
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
- **Admin console (`admin-ui/` + `internal/web/adminui`)**: a React SPA in the Shelf design
  (workspace `ADMIN-CONSOLE-PLAN.md` holds the phases still to come; `admin-ui/STYLEGUIDE.md` the
  design). It replaced the classic vanilla-JS console at the Phase 1b cutover (no switch, no
  `/admin/classic`). Vite builds into `internal/web/adminui/dist`, embedded with
  `//go:embed all:dist` (only `dist/.gitkeep` is committed; a build without it serves a 503
  "console not built" page). Serving goes through **`internal/web/spa`**, the one SPA handler
  the console and the web player share: files under the asset dirs are immutable and 404 when
  missing, other files and HTML revalidate, a missing top-level file with an extension 404s,
  anything else boots `index.html` for client routing; MIME types are pinned process-wide.
  Every static file (console, player, connect/setup assets) carries a strong content-hash ETag
  (a match is a 304, documents included) and text types go out gzipped to clients that accept it
  (`spa.Files`: worked out once per file and kept, redone when a web_dir file's size or mtime
  changes; never for a Range request; `Vary: Accept-Encoding`).
  **The CSP does not change for it**: no inline script/style anywhere (`theme-init.js` is
  external, Base UI runs under `CSPProvider disableStyleElements`, banned libraries are
  ESLint-enforced, `admin-ui/scripts/check-csp.mjs` fails the build and `TestEmbeddedBuild`
  checks the embedded `index.html`). It keeps the classic console's token key
  (`localStorage["audiosilo_token"]`), so an admin stays signed in across the upgrade. A 403 from
  an `/admin` endpoint makes the console re-check `/me` and sign a demoted admin out.
  Admin-only endpoints that exist for it: `GET /admin/libraries` adds `book_count` and
  `available` (root reachable: `Scanner.RootAvailable`, a 2 s-bounded, 15 s-cached, one-at-a-time
  probe per root so a hung NFS mount can't stall the page, plus "empty while books are indexed"
  and "last scan stopped at the guard"); scan status adds `unavailable` and `queued`, and
  `API.startScan` queues the scan in the job queue (see "Job queue" below), which marks the
  library queued before the request returns so the first poll sees it; `GET /admin/invites` lists every account's invites (`auth.ListInvites`, never a
  code); `GET /admin/fs/dirs?path=` is the add-library folder picker (`library.ListDirs`:
  absolute paths, folder names only, no dot-folders, 1,000-entry cap); `GET /admin/shares` adds
  `member_ids` (`catalog.ShareMembers`).
- **Community metadata lookup (Phase 1.5, `internal/meta`)**: `GET
  /api/v1/libraries/{id}/meta?path=` (authed, scope-checked via `authorizedPath`
  + `bookForPath`, exactly like `item`) resolves the book's `asin`/`isbn`
  (backfilled via `book_enrichment`) against the community metadata API
  (`metaserve`, meta.audiosilo.app) and returns a composed enrichment envelope
  (work + matched recording + series rails, each carrying its own `web_url`).
  The `work` also carries the community **characters** and **recaps** (the CC
  BY-SA expressive layer: spoiler-tagged, position-keyed - `reveal`/`through`
  are logical work-chapter positions) when metaserve has them, plus a
  whole-book `recap_summary` (`{in_short, ending}` - `ending` is a full spoiler
  by construction); all three are additive/`omitempty`, mirrored on
  `upstreamWorkDetail` and `MetaWork` and passed through by
  `toCharacters`/`toRecaps`/`toRecapSummary` (which drops an all-blank summary).
  **Work-id lookup**: `GET /api/v1/meta/work?id=<work id>` (authed, *not*
  library-scoped - global read-only community data with no path to authorize;
  the id is a query param because work-id slugs are not path-segment safe, and
  `internal/meta` URL-escapes it upstream) returns `{"work": {...}}` with the
  same `MetaWork` shape. It is the "catch me up on the previous book" path: a
  series rail carries sibling work ids but no characters/recaps, so a client
  resolves one of those ids here. `Service.Work` wraps the single
  `works/{id}` GET and caches under a `"w:"` key space in the SAME bounded
  cache, with Enrich's TTL policy (24h / 1h not-found / 2min error) and the
  same "never cache a caller-cancelled failure" rule. Because the work id is the
  one **caller-chosen** value that becomes a cache key, an outbound GET and a log
  field, it is bounded on three axes: the handler rejects an id over
  `maxWorkIDLen` (200 bytes) or carrying control characters (400 `invalid id`,
  `validWorkID` - transport-level hygiene, not a slug grammar); the `"w:"` key
  space has its own cache quota (`maxWorkEntries`) so an id flood can only evict
  other work entries, never the enrichment cache; and uncached upstream fetches
  go through a small semaphore (`maxConcurrentWorkFetches`, the transcodeSem
  pattern) as an amplification bound on the shared community service. A
  wrong-shaped upstream 200 (an id colliding with a literal metaserve route
  decodes leniently to an empty work) is treated as `ErrNotFound`, never cached
  or served as a positive blank work. Degradation: metadata off
  -> 404; missing/blank `id` -> 400; malformed `id` -> 400; unknown work id ->
  404; upstream error -> 502.
  Config is `metadata.{enabled,base_url}` (env `AUDIOSILO_METADATA_ENABLED` /
  `AUDIOSILO_METADATA_BASE_URL`; `base_url` must be an absolute http(s) URL when
  enabled) - one key disables ALL outbound calls. **Runtime toggle**: `meta.Service`
  is constructed in `api.New` whenever `base_url` is valid (`MetadataConfig.ValidBaseURL`),
  regardless of `enabled`, and the live config's `metadata.enabled` gates it - so an admin
  can flip it on/off without a restart (Server settings, below). The handler and the
  `metadata` capability both gate on `metadataOn()` (`a.meta != nil && enabled`);
  `a.meta == nil` (empty/invalid `base_url` at start) can't be enabled (400
  `metadata_unavailable`); a new `base_url` waits for a restart. `meta.Service` owns the compose logic
  (lookup -> works/{id} -> pick the recording by `recording_id`, first as
  fallback -> up to 3 series rails, one per ordering family) behind a bounded in-memory TTL cache (24h
  positive / 1h not-found / 2min transport-error, ~2048-entry cap) so a hot path
  or a down upstream isn't hammered; the api handler (`handlers_meta.go`) is
  transport-only. Degradation: disabled -> 404 (and the `metadata` capability is
  false, so clients hide the UI); no asin/isbn or no upstream match -> `200
  {"matched": false}`; upstream unreachable -> 502. Out of scope for now: no cover
  remote-fallback, no persisting meta into the DB, no tag-based ASIN extraction.
  **Reading-order families** (metaserve schema_version 7): `seriesRails` collapses
  each family (key `ordering_of || id`) into ONE rail whose top-level view is the
  MAIN view - the ref with no `ordering_of` (the primary), else the first ref - so
  a shipped player that ignores the new fields never sees a chronological order's
  earlier books as "previous". The other orders ride along as additive
  `orderings` (at most `maxOrderingAlternates` = 2 per family, failures make the
  envelope partial), and `maxSeriesRails` counts FAMILIES. Every main view is
  fetched before any alternate, so under `composeTimeout` a slow upstream costs
  alternates, never rails (`TestEnrichMainsBeforeAlternates`). A pre-v7 upstream
  yields byte-identical rails (`TestEnrichPreV7RailsUnchanged`), except that a
  work listed at two positions of one series is now one rail rather than two
  (`TestEnrichRepeatedMembershipIsOneRail`). Server-side because shipped players
  lag.
- **Native deep-link association**: `GET /.well-known/apple-app-site-association`
  and `/assetlinks.json` are served from `config.AppLinkConfig` (`app_links` in
  YAML) and 404 when unset. They only enable auto-app-launch for domains the
  shipped app build claims - self-hosted arbitrary domains fall back to the web
  player + the custom-scheme "Open in app" button.
- **SQLite** runs with a single write connection (writers serialize) plus a
  read-only reader pool, WAL mode.
- **Pagination** is keyset/cursor-based (`catalog.ListBooks`); don't switch list
  endpoints to OFFSET for large tables.
- **Path safety**: any filesystem access derived from user input goes through
  `library.SafeJoin`, which rejects traversal outside the library root.
- **Path-addressed API**: content endpoints are `GET /libraries/{id}/{item,
  chapters,cover,stream}?path=` and `{GET,PUT} .../progress?path=` etc. The path
  is the handle (a query param, to avoid encoded-slash issues). `item`/`chapters`/
  `cover`/`meta` resolve `(library, path)` to a book in ONE place, `bookForPath`:
  `GetBookByPath`, then the indexed folder book holding the path
  (`GetBookHolding`: a part, a disc folder of a joined book), then indexing on
  demand (`Scanner.IndexPathWithin`) if the scan hasn't reached it. A book found
  above the requested path must be in the caller's scope too (`library.ErrNotAllowed`,
  answered as the out-of-scope 403 `no access to this path`, nothing read or
  indexed for that caller); `stream` serves an
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
  the path key; moving keeps it via the fingerprint. The per-user path-keyed tables are listed
  ONCE, in `carryListeningState` (`catalog/listening.go`; add a table -> add a line), which both
  a move (offset 0) and a join (`JoinDurableState`) use: positions shifted onto the new
  timeline, a listener's progress already at the new path merged by a strategy the caller
  passes (`progressMerge`), a favourite landing once, so a collision never fails a move. A
  **move** takes the newest save whole (`mergeNewest`: `updated_at`, then version; a row
  already at the new path is a removed book's, so its position or finish must not win by
  being further on); a **join** keeps the furthest (`mergeFurthest`: the rows are parts of one
  book; finished over not, the newer save's stamp, the earlier start). Both take a version
  above both rows. A favourite on a navigation folder (author, series) has no book to move:
  `detectMoves` re-keys it (`catalog.MoveFolderFavourites`) to the folder its moved books
  went to (`renamedFolders`), only when every move out of it agrees and it is gone from disk
  by exact name (so a case-only rename, of it or a folder above it, counts).
- **Auto book/folder detection**: there is **no per-library layout**. The model
  (`booksInDir` in `library/scanner.go`) matches the dominant "folder per book"
  convention (and Audiobookshelf): **a directory that directly contains audio is
  ONE book**, with all those files as its tracks/chapters - whether it holds a
  single m4b or fifty distinctly-named mp3 chapters (do NOT split a folder's files
  into separate books by filename; that produced one phantom book per chapter).
  The only per-file case is the **library root** (loose files there are individual
  single-file books - the old "flat"). A folder of loose single-file books
  (`books_in_folder`) is expressed with a **per-folder override**:
  `folder_overrides(library_id, path, mode)`, `mode ∈ {book, collection}` -
  `collection` = one book per file, `book` = force folder-is-one-book. Overrides
  are durable, path-keyed config (no FK to the rebuildable index, like
  progress/bookmarks). `PUT/DELETE /admin/libraries/{id}/folder-override?path=`
  sets/clears it and rescans; the admin console's per-library **Folder detection** dialog
  drives it. `GET /fs` annotates each entry's effective `override`.
  **Joined books (`library/joined.go`)**: `book` joins only a **disc set** (`discSets`): a
  folder below the root with no audio of its own whose every folder holding audio beneath
  it is a disc folder (`isDiscFolder`: cd/disc/disk + number) **directly** in it, at least
  two (`minDiscs`; one disc already reads as one book). A CD rip (`Book/CD1`, `Book/CD2`,
  ...) becomes ONE book of its discs' audio (hidden/ignore rules applied), with no books for
  the discs. Any other folder keeps `book`'s old meaning (its own files; without any, a
  no-op): overrides the old detection dialog set on author/series folders must never merge
  a series (or its listeners' progress) on upgrade, and nothing is auto-detected
  (re-shaping existing books would orphan progress). Order: the disc folders by
  `discOrder` (natural: CD2 before CD10, "CD 1" = "CD1", ties by path), each disc's files
  in exactly the order a book of that disc alone has them (byte-wise name order, as
  `audioEntries`/the walk read them; NOT natural), since the state carry-over maps a disc
  position as offset + position. Files and chapters keep real paths in the discs
  (`file_path`, a chapter title led by its disc: "CD2 - 01"); the cover is the folder's own
  image, then the first disc's; `IndexPath` resolves the folder, a disc folder or a file in
  it to the joined book, and so does `bookForPath` from the index first (`GetBookHolding`),
  so the disc paths shipped clients still hold don't re-walk and re-probe every disc on
  each request. Reaching the joined book through a disc path takes a grant on the book: a
  share made before the join on one disc folder gets the out-of-scope 403 for it (as for
  the book's own path) and never triggers the re-read, though it still streams the disc's
  real files (scoped on the file path). Discovery and `IndexPath` decide a join in one place (`joinRoot`:
  the folder or its parent, with `book` and a disc set), and the discovery walk
  (`audioDirs`) reads each folder's audio files once, handing them to
  `booksInDir`/`joinedBook`. The scan where it takes effect runs `carryJoinedState` before
  the prune: `catalog.JoinDurableState` moves the disc books' listening state onto the
  joined timeline (offset = earlier discs' lengths; furthest progress per listener wins;
  bookmarks/notes/history/sessions offset; favourites once) and copies their
  edits/cover/enrichment (earliest disc wins per field, chapter renames re-keyed). A disc's
  listening state is carried only when its offset is **known** (`joinParts`): the first
  disc's always (0), a later disc's only when every earlier disc's length is (its files',
  else its indexed length). With ffprobe off or failing, a later disc is `Unplaced`: its
  listening state stays on its own path (path-keyed, back if the join is undone) rather
  than landing in disc 1, its config is still copied, and its `joined` event carries code
  `length_unknown` (the console: "progress stayed with the disc: length unknown"). Each
  disc is logged `joined`, not `removed`, and the joined book is a reshape, not new
  content: not counted in `Added`/`AddedTitles` (`joinsIndexed`), so no "books added"
  notification. Removing the override splits the discs out again; the joined state stays
  on the folder's path. That un-join is a reshape too (`Scanner.splitFrom`, the
  `joinsIndexed` counterpart: books in the disc folders of a folder whose stored book was
  joined, `isJoined`, and is no book now): the discs are not counted in
  `Added`/`AddedTitles`, and the joined book is logged `split`, not `removed` (nor counted
  in `Removed`). A folder book with audio of its own that goes is still `removed`. `GET /fs`
  marks a disc set whose discs are indexed (`split_discs`, from `books.split_parent`) for an
  **admin** caller only (`annotateWithBooks`; `omitempty`), so the player's listing is
  unchanged; the console offers "Always one book" on a folder without audio only there or
  on a joined book.
- **Metadata overrides (admin redesign Phase 2a, `catalog/overrides.go`)**: an admin's
  edits never touch files. A `books` row holds the **effective** values - what the scan
  found (kept in `books.scanned`, JSON field -> value, stamped with the upsert's
  `indexed_at`: a snapshot whose stamp isn't the row's was left by an older server, so
  `loadLayers` reads the scan's values off the row instead), then `book_enrichment`
  (asin/isbn), then `book_overrides`. `bookLayers.resolve` is the ONE statement of that
  precedence (and of each field's source); `refreshEffective` writes its values to the
  row (plus chapter titles from `chapters.scanned_title` + `chapter_overrides`, and FTS)
  and the book page shows it as provenance, so the two can't disagree. `UpsertBook` runs
  `refreshEffective` in the scan's own transaction, which is what makes an edit a lock (a
  rescan rewrites the scanned values and re-applies the edit before anything can read
  the row) and why there is no separate post-scan enrichment pass any more.
  `SetEnrichment` and `EditBook`/`EditBooks` call it too. Players, search, `/fs` and
  export read the row, so they see edits with no join; the player's book JSON shape is
  unchanged (`published`, `description`, `has_cover`, per-file codec are admin-only,
  `json:"-"`). Validation: `normalizeOverride`; sources: a scanned value is `path` when
  it equals what `DeriveFromPath` yields, else `tag`; an override is `edited` or
  `community`; an enrichment-attached ASIN/ISBN reads as `community`. Revert = delete the
  override + `refreshEffective` (restores the scanned value; no reindex, no disk).
  `MoveDurableState` carries overrides and custom covers as one set: when the moved book
  has any, the new path's own rows in all three tables are dropped first; it moves
  them (with enrichment) in a transaction of their own, so a failure carrying the per-user
  state can't strand them. `detectMoves` doesn't pair a folder reclassified as a collection (or
  back) with its own first part, nor a joined book renamed away from its override with its first disc (`reclassified`).
  `has_cover` holds whenever there is a sibling cover (`UpsertBook` enforces it) and is
  NULL until checked; the scanner backfills unchanged pre-0016 rows in one transaction
  with a tag read (`media.EmbeddedCover`, no ffprobe).
- **Admin catalog API** (`api/handlers_catalog.go`, admin-only, transport-only):
  `GET /admin/books` (keyset over the named orderings in `catalog.adminSorts`, sorted and
  paged on ids before the per-row columns are computed; the cursor names its ordering;
  filters in `catalog.BookFilter`, unparseable ones 400; `media.DirectPlayableSQL` is
  `DirectPlayable` in SQL) + `/admin/books/facets` (each dimension counted without its
  own filter, the unfiltered yes/no ones in one pass);
  `POST /admin/books/bulk` (one edit over <= 1000 books, all or nothing);
  `GET /admin/authors|narrators` (whole field values + `merge_suggestions` from
  `personKey`, keyed by `match.Fold`, which keeps every script's letters) and
  `/admin/series`; `GET`/`PATCH /admin/libraries/{id}/book?path=` (book
  page: per-field provenance, chapters, files, listeners, shares, folder override);
  `GET /admin/libraries/{id}/book/match?path=` (`meta.Service.Candidates`: metaserve's
  STRUCTURED match `works/match`, then up to 6 works expanded, uncached, bounded by
  `workSem` via `fetchWork`. The match gets the book's facts separately
  (`matchParams`): the path's LAYOUT (`derivePathFacts`: disc/track folders dropped,
  top folder = author, holding folder = series, leaf = a title guess sent RAW -
  metaserve is the one place that reads `SW06 - `/`Sharpe - 08 - `/`02. `
  numbering and takes the position from it) then the tagged title, path + tag
  authors, the series folder AND the tagged series when they differ (folder first;
  metaserve judges each, the better counts), the tagged position (only when no
  DIFFERENT series folder goes up: one position applies to every series guess and
  replaces the leaf's numbering), runtime, `?q=`, and the asin/isbn (typed;
  the book's only when nothing was typed), which metaserve looks up itself; candidates carry metaserve's
  `score` and `reasons`. A typed `?asin=`/`?isbn=` with no text is a plain `lookup`.
  Fallback, FROZEN until the production metaserve serves `works/match` (then delete
  it): when the route is missing (404 through `works/{id}`, 405, or a 200 without
  `results`) `searchHits` runs the old `lookup` + `works/search` unchanged (typed
  text, else `CleanTitle` + author) scored by the tag-only `scoreCandidate`, and
  the missing route is remembered for 15 min (`matchUnsupportedUntil`); a 503/5xx
  from works/match is an outage (502), never a fallback; identifiers normalized for
  the exact lookup; metadata off -> 404 `metadata_off`);
  `POST /admin/books/works` (`{books:[{library_id,path}]}`, <= 100, `catalog.BooksByRefs`) answers
  `{"works":[{library_id,path,work_id,failed}]}` in request order: each book's community work id
  (`meta.Service.WorkIDs`: per distinct normalized identifier, first the cached enrichment's
  work id (a cached "no match" too), so a book a player opened costs the card no lookup, then its
  own `"l:"` key space (Enrich's TTLs, quota `maxLookupEntries`), which only `fetchLookup` writes
  (Enrich's compose looks up fresh and records nothing there); only misses go upstream,
  through `sharedLookup`: bounded across console requests by `lookupSem`, concurrent misses of one
  identifier sharing one flight whose waiters can each leave without failing the others, the batch
  under `composeBudget`, and a failure from the caller's, the flight's or the budget's context never
  cached. One-way: Enrich never reads `"l:"` nor waits on `lookupSem`, so a console batch can't time
  a player's `/meta` out or age its enrichment); `""` = no identifier, no match or a failed lookup,
  and per book `failed` says its own lookup failed or ran out of time, so asking again may resolve it
  (never for a clean miss); metadata off -> 404
  `metadata_off`. The Series cards place an owned book on a community rail by this work id first and
  fall back to `series_index` (display only: no book is changed);
  `PUT`/`DELETE /admin/libraries/{id}/cover?path=` (custom cover in the DB;
  `catalog.SetCover` enforces 5 MiB, sniffed JPEG/PNG/WebP and an indexed book), which
  `GET /libraries/{id}/cover` serves first (by the requested path, then by the book a
  part path resolves to; only while a book is indexed there), behind the caller's
  scope, validated by an ETag from its `updated_at` (not Last-Modified, which the
  sidecar fallback would answer with a stale 304 after a delete) so a matching
  `If-None-Match` is a 304 without reading the image; sidecar and embedded art keep
  `max-age=86400`. Only GET/HEAD of streaming-shaped
  paths skip the request timeout, so the cover upload stays bounded.
  Error codes `book_not_found`, `invalid_override` (+ `field`), `too_large`,
  `unsupported_image`. The internal book id appears only inside the opaque cursor.
  `POST /admin/covers` (`api/handlers_covers.go`, Phase 2b) is how the console shows
  covers: `{books:[{library_id,path}], size: 160|320|640}` (<= 60) returns JPEG
  thumbnails as `data:` URLs in request order (`""` = no art), resolved like
  `/cover` (custom, sidecar via `SafeJoin`, embedded; never on-demand indexing), from
  one `catalog.CoverSources` query per library for the whole batch.
  One request per page of covers instead of one per cover, no token in any URL,
  ~20 KB a cover instead of full art.
  `media.Thumbnail` refuses sources over `MaxThumbnailSourcePixels` from the header
  (decompression bombs), `media.ThumbCache` is a byte-bounded LRU keyed by the art's
  version (custom `updated_at`, file size + mtime) holding finished data: URLs, and
  `thumbSem` bounds decodes (reads are bounded per request, outside it). Admin book rows
  carry `matched` (the `matched=` filter's rule), and `POST /admin/shares/{id}/paths`
  also takes `{"rules":[...]}` (<= 1000, one transaction) for adding a selection.
- **Rate limiting by route class** (`rateLimit` in `api/middleware.go`, buckets in
  `api/ratelimit.go`), read off the handler `mux.Handler` picks: static files
  (`web.IsStatic`) are not counted; media routes (`requireMediaAuth`, whose
  `mediaHandler` marks cover/stream) limit themselves: refused while the address's
  general bucket is empty (`Ready`), a failed authentication charges it (`Charge`:
  even past empty, down to -burst, so requests that passed `Ready` together all pay), an
  authenticated one spends from `mediaLimiter` (~200/s, burst 2000, per credential id)
  and charges the address's bucket when that refuses it (the lookup was already done);
  everything else spends from `ipLimiter` (~50/s, burst 200, per IP). The brute-force
  lockouts (`limiter`) are separate. Tests: `ratelimit_routes_test.go`.
- **Job queue, scan history, schedules, ignore rules (admin redesign Phase 3, `library/jobs.go`,
  `schedule.go`, `ignore.go`, `problems.go`)**: every scan (startup, schedule, admin rescan, a
  library or folder-setting change) goes through `Scanner.Enqueue`; `Scanner.Start` runs ONE
  worker (scans never compete for the disk or the single DB writer) and the scheduler. Coalescing:
  a library already waiting returns the waiting job; one running gets one follow-up for a manual or
  change trigger (the running scan may have read the old setting) but not for a schedule or
  startup. Each run is a `scan_runs` row (`catalog/scanruns.go`: trigger, who, status
  `running|ok|partial|unavailable|failed|cancelled|interrupted`, counts (a move counts once, as
  moved), a log of at most 300 `RunEvent`s (problems take at most half, so the moves and removed
  paths after them keep room; past the cap events are only counted); 100 kept per library; rows
  left `running` by a stopped server become `interrupted` at startup). `Cancel` drops a queued job
  or cancels the running scan's context: discovery and the per-book loop stop on it, and a scan
  stops before its prune, never inside it (the prune, `DeleteBooksNotIn`, is one transaction run
  to completion once begun). Requested scans are capped at an hour (`jobContext`); the startup
  scan, a library's full index, isn't. A root that doesn't answer the bounded root probe stops at
  the unavailable guard instead of holding the one worker. Deleting a library cancels its scans
  (`CancelLibrary`). **Prune semantics are unchanged**: a vanished
  book is deleted from the index (no ghost rows) and its path lands in the run log; durable state
  is path-keyed, so progress comes back if the files do. Schedules (`ParseSchedule`): `""`,
  `every:{1,3,6,12,24}h` after the newest run started, `daily:HH:MM` in the server's zone; the
  newest run of any trigger counts, so a server that was off runs a missed daily scan on return;
  a due slot dropped without a run of its own (cancelled while queued, or folded into a scan of
  the library already running) counts as a start too (`jobQueue.skipped`), so it isn't re-queued.
  Ignore rules (`NormalizeIgnore`/`ParseIgnore`) live in the DB because the server never writes to
  the library: gitignore-lite (no `/` = a name at any depth, a `/` before the end, a leading one
  included, = anchored to the root, trailing `/` = folders only, case-insensitive, `#` comments,
  <= 100 patterns of <= 200 bytes); the scan, `BrowseFS` and `IndexPath` all honour them, and rules
  that skip every indexed book prune it rather than tripping the unavailable guard.
  `metadata.Extract` reports `OpenErr`/`ProbeErr`
  (ffprobe runs with `-v error` so its message survives, path prefix stripped); `noteProblem`
  records the first read problem per book (`unreadable|empty_file|probe_failed`), and a scan
  re-reads an otherwise unchanged book once the file its problem names opens or probes again
  (`problemCleared`: a fixed permission changes neither mtime nor size). A folder book
  whose parts are all >= 1 h with >= 2 distinct titles (tag, else file name) gets
  `suspect_parts`; rows from before 0017 are checked once with a tag read. `PATCH
  /admin/libraries/{id}` rescans only when the root or ignore rules change.
- **Health issues (`catalog/issues.go`, `api/handlers_health.go`)**: computed from the index on
  request; each book kind is one SQL predicate (`issuePredicates`) shared by the counts and by
  `GET /admin/books?issue=` (and `&issue_ignored=true`), so they can't disagree: `no_cover`
  (checked rows only), `no_chapters` (<= 1 chapter, > 2 h), `unmatched` (left out of the summary
  while metadata is off), `transcode`, `suspect` (not a folder with a `book` override, the
  category's own fix), `scan_error`, `split_discs` (one book split across disc folders, listed by
  its first disc: `books.split_parent` set, no override on that folder; the console's fix sets
  `book` on it, which joins them; its discs never also group as duplicates). `split_parent` is
  worked out during discovery (`library.markSplitDiscs`: a folder book whose folder is a disc
  set, the same `discSets` predicate the join uses, so the fix always joins; `IndexPath` the
  same from the folder's subtree) and stored by `UpsertBook`; an unchanged book whose value
  differs (a sibling came or went, a pre-0022 row) gets it via `SetSplitParent` without a
  reindex. `duplicate` is groups within one
  library (`DuplicateGroups`: same fingerprint AND size, same ASIN/ISBN, or `metaKey` with a
  length within 1 min / 2%, an unknown length matching only another unknown one; copies across
  libraries are deliberate); a group is hidden while every member is ignored, so a new copy
  brings it back. Endpoints (admin only): `GET /admin/issues` (counts + samples + offline
  libraries with books/listeners + `checked_at`), `GET /admin/issues/duplicates` (the open
  groups, or with `?ignored=true` only the ignored ones), `POST`/`DELETE /admin/issues/ignore` (<= 1000), `POST
  /admin/libraries/{id}/book/rescan?path=` (`IndexPath` now, on a context detached from the
  request timeout so a slow re-read is still saved; 404 `not_indexable`), `GET
  /admin/jobs` (running + queued + schedules), `DELETE /admin/jobs/{id}`, `POST /admin/scan`
  (queue every library: `Scanner.EnqueueAll`, also the startup scans), `GET /admin/scan-runs`
  (`?library_id=&before=&limit=`, `next_before`) and `GET /admin/scan-runs/{id}` (with the log).
  Codes `invalid_schedule`, `invalid_pattern`, `not_indexable`.
- **Sessions, devices, Activity stats (admin redesign Phase 4a, `catalog/sessions.go`,
  `activity.go`, `progress_admin.go`, `auth/devices.go`, `api/handlers_activity.go`)**: players name
  themselves in `X-AudioSilo-Client: <app>[/<version>] [(<platform>)]` (`auth.ParseClient`, strict;
  the console sends `AudioSilo Admin (web)`, the player `AudioSilo/<version> (<platform>)`, and only
  same-origin on web). `authenticate` calls `auth.ResolveRequest`, which records the app (only when
  the request names one, so a headerless `<audio>` fetch keeps it) and the newest IP on the token, and
  puts the `auth.Credential` (token id, kind, device name, app) in the context (`credentialFrom`; every
  context key lives in one iota block in `respond.go`, since the IP key once collided). CORS allows the
  header. **Sessions are derived from progress saves**, not from the client-posted
  `listening_history` spans (those arrive only on stop, are dropped offline and carry no device):
  `handlePutProgress` passes every save to `catalog.RecordHeartbeat`, which extends the token's
  session on that book when the save comes within `SessionGap` (10 min), or later when the position
  advanced by about the time that passed (`continuousPlayback`, within `resumeWindow`, 12 h: Android
  pauses the player's save timer while the screen is off), and otherwise starts a new one; listened
  time is the position advance / speed, capped by the server time between saves (only server time is
  used, so a wrong device clock or an offline replay can't inflate it); a session with nothing
  listened yet (a single save: "Mark finished", the manager's stats sync) is shown nowhere
  (`listenedSQL`) and deleted by retention; best effort (a failure is logged, the save still
  succeeds). `catalog.StreamMarks` (in memory) remembers `?transcode=1` streams per token for 10 min
  so the session is marked transcoded. Session times are fixed-width millisecond UTC strings
  (`sessionTime`) so they compare as text; hours are taken with `hourOf` (not `time.Date`, which
  loops on a daylight-saving fall-back). Retention: `pkg/launcher.retention` runs
  `PruneSessions` at startup and daily, rolling sessions older than the live
  `activity.session_days` setting (Settings > General `general.session_days`, env
  `AUDIOSILO_SESSION_DAYS`, 30-3650, default 400; `API.SessionRetention`, read at each run) into
  `listening_daily` per local day, listener and book, and blanks `last_ip` on signed-out or expired tokens
  (`auth.ForgetRevokedAddresses`). Migration 0021 backfilled listening from before sessions were
  recorded: `listening_sessions.backfilled` rows (from the players' `listening_history` spans; no
  device, app or playback mode, so left out of those breakdowns) and `listening_daily.estimated` rows
  (one per book, in totals and tops only, never in a day, calendar or hour; `Activity.estimated` says
  how much). `SaveProgress` stamps `progress.started_at` on insert and
  `finished_at` when `finished` turns on (cleared when it turns off), both from the save's own
  `updated_at`; both are admin-only (not on the player's progress JSON). Endpoints
  (admin only): `GET /admin/sessions/live` (one per device, with chapter and IP), `GET
  /admin/sessions` (`?user_id=&library_id=&path=&before=&limit=`, `next_before`), `GET
  /admin/devices?user_id=` (session + API-key tokens, `current` marks the caller), `DELETE
  /admin/devices/{id}` (409 `current_device` for the caller's own token), `GET
  /admin/users/{id}/progress`, `PATCH /admin/libraries/{id}/progress?path=&user_id=`
  (`catalog.EditProgress`: finished / position / dates, stamped with server time + version so it
  beats stale device saves; it starts progress only on a book the *user* can see, by the user's own
  scope, else 409 `no_access`, while existing rows stay editable after access is taken away; a
  `YYYY-MM-DD` date is the start of that day for `started_at` and the end of it, or now if sooner, for
  `finished_at`) and `GET /admin/stats?range=7d|30d|90d|1y|<year>|year` (`year` = the current year in
  server time, labelled with it, so the browser's clock never picks the year; answers only `{"activity"}`,
  `catalog.ActivityFor`, bucketed in server time, without the Overview's figures; 400 `invalid_range`),
  plus `GET /admin/listening?range=&user_id=` (`catalog.ListeningDaysFor`: the same days, of everyone
  or one person, and nothing else: the year calendar and a person's listening year). Book-page
  listeners carry `started_at`/`finished_at`. Sessions and roll-ups move
  with the book (`MoveDurableState`).
- **Server settings, system status, updates, logs (admin redesign Phase 5a)**: `internal/config/settings.go`
  is the ONE table of console settings (`fields`): each has an id `<section>.<name>` (also where it sits in
  `GET /admin/settings`), its config.yaml key, its `AUDIOSILO_*` variable, whether it is read only at start
  (`restart`) and its per-field check (`fix`). `applyEnv` walks the same table and records which keys the
  environment set (`fromEnv`); those are **locked** in the console (409 `setting_locked`) and `Save` writes
  config.yaml's own value for them (`file`), so an env value never lands in the file. Launcher overrides (the
  desktop manager's bind/TLS/public URL) are `Pin`ned and locked as `"launcher"`. `WithSettings` returns a
  validated copy (all or nothing; `SettingError` with reason unknown/read_only/locked/invalid and the setting
  id, mapped by `writeCatalogError` to `invalid_setting`/`unknown_setting`/`setting_read_only`/
  `setting_locked` + `field`). The API never mutates a config: `API.boot` is the config the server started
  with (restart-only settings read it: bind, TLS, web_dir, metadata.base_url, demo on/off + idle_ttl) and
  `API.live` (`liveConfig`, read via `a.config()`) is swapped whole by a save, carrying the parsed trusted
  proxies and CORS origins, so name, public URL, CORS, proxies, app links, metadata on/off and the demo
  library/cap apply on the next request. `restart_pending` lists restart settings whose saved value differs
  from `boot`. New keys: `name` (`DisplayName`: GET /server `name`, pairing `server_name`; default
  "AudioSilo") and `update_check` (default true, `AUDIOSILO_UPDATE_CHECK`). `GET /admin/system`
  (`handlers_system.go`): tools + versions (`toolfetch.Version`, cached), metadata health (`meta.Service.Ping`:
  metaserve `/healthz`, cached a minute, **only while metadata is on**), TLS certificates read from their
  files (`server.Certificates`; never generates or requests one), database size + schema
  (`catalog.DatabaseInfo`), each root's availability + disk space (`Scanner.RootDisk`: statfs inside the
  same bounded root probe, so a dead mount can't hang it), web player source, update status. The update check
  (`internal/updates`, started by the launcher): GitHub's latest release, a minute after start then daily,
  conditional on the ETag, `User-Agent: AudioSilo/<version>`, nothing else sent, never while off;
  `GET /admin/update`, `POST /admin/update/check` (409 `update_check_off`; within a minute it answers with
  the last result). Logs: the launcher wraps its logger in `logring.Handler` (Info and up into a 2,000-line
  ring; attributes whose key has a secret word (token, password, code, key, secret, cookie, authorization)
  are redacted); `GET /admin/logs?level=&q=&after=&limit=` (`after` = the live tail's cursor). All admin-only.
- **Backups, audit log, notifications (admin redesign Phase 5b)**: **Backups** (`internal/backup`) are the
  whole database written with `VACUUM INTO` (`store.DB.VacuumInto`, on a short-lived connection of its own:
  the reader pool's query_only refuses it and the one writer would stall writes) into the backups folder
  (`backups.dir`, config.yaml/env only, default `<data>/backups`) as `audiosilo-<UTC time>-<kind>.db`
  (`scheduled|manual|before-restore`), written under a hidden temporary name, owner-only (0600: it holds
  password and token hashes), then renamed. Settings `backups.schedule` (`""`, `daily:HH:MM`,
  `weekly:DAY:HH:MM`, server time; default `daily:03:00`) and `backups.keep` (scheduled ones kept, default 7;
  manual and before-restore copies stay until deleted). Only names matching `backup.validName` are listed,
  served, deleted or restored, and a symlink is never followed. **A restore never swaps a live database**:
  `RequestRestore` checks the file quickly (`store.Inspect` without `full`: read-only open, an AudioSilo
  schema whose every applied migration this server knows, else `ErrNewerDatabase`; `quick_check` reads every
  page, so it runs only at start) and writes `<data>/restore.json`; the launcher's
  `backup.ApplyPendingRestore` runs before `store.Open`, checks again in full, copies the current database into the
  folder as `before-restore` (or, if it can't be read, renames its files aside in the data folder), swaps
  the backup in (removing the old -wal/-shm), writes `<data>/restore-result.json` and always removes the
  marker (a refused restore is reported once, not retried at every start); `recordRestore` logs it in the
  restored database's audit log as the server's own act. **Audit log** (`catalog/audit.go`,
  `api/handlers_audit.go`): `audit_events` (0019; actor id + name copied, no FK; `via` session/api/system;
  no IP), written by `API.audit` after an admin change succeeds (best effort), kept 365 days / 100k rows
  (`PruneAudit`, daily in `launcher.retention`); scans and job cancels are not audited (Jobs history has
  them), backup downloads are. Never a secret in details: a password change reads `set`/`cleared`, a
  destination's address and secret only as `address_changed`/`secret_changed`. **Notifications**
  (`internal/notify`): `notification_targets` (kind webhook/ntfy/discord, name, url + secret write-only:
  the API shows `notify.Redact`'s address and `has_secret`), `server_events` (the bell's feed, 90 days,
  `dedup_key` announces an update once per version). `notify.Service.Emit` records the event and queues one
  delivery per enabled subscribed destination (4 workers, queue 256, drop + log when full); a delivery is
  retried twice on timeout/unreachable/429/5xx (queued again after its delay, never waited out on a
  worker), never follows a redirect, and records only a short reason
  (`timeout`, `unreachable`, `http_<status>`, `failed`), never the URL or the answer. Webhooks POST JSON
  with `X-AudioSilo-Event` and, with a secret, `X-AudioSilo-Timestamp` + `X-AudioSilo-Signature: sha256=`
  HMAC of `<timestamp>.<body>` (`notify.Sign`); ntfy is a JSON publish to the server root (Bearer token
  optional); Discord is one embed with `allowed_mentions` empty. Triggers: `Scanner.OnRunFinished`
  (`book_added` with up to 5 titles from `ScanResult.AddedTitles`, `scan_failed` (its `detail` is shown in the
  bell but never sent out: `internalData`), `library_unavailable`
  only when the previous run wasn't), login/exchange (`new_device`, not demo accounts; `invite_redeemed`
  via `auth.ConsumePairing`'s code kind), `updates.Checker.OnAvailable`, `backup.Service.OnFailure`.
  Private addresses are allowed (LAN webhooks are the common case); destinations are admin-only and the
  console never sees a response body. Endpoints (admin only): `GET`/`POST /admin/backups`,
  `GET`/`DELETE /admin/backups/{name}` (download streams, outside the request timeout),
  `POST /admin/backups/{name}/restore`, `DELETE /admin/restore`, `GET`/`POST /admin/notifications`,
  `PATCH`/`DELETE /admin/notifications/{id}`, `POST /admin/notifications/{id}/test`,
  `GET /admin/events?before=&limit=&kind=` (`kind` one of `notify.Kinds`, else 400),
  `GET /admin/audit`. Codes `backup_running`, `backup_not_found`, `invalid_backup`, `backup_too_new`,
  `invalid_target` (+ `field`, `reason` = a `notify.Reason*` constant the console words, never renamed,
  and `max` for a `*_too_long`), `too_many_targets`. A password sign-in's `new_device` is skipped when
  the browser's `device_id` (`auth.IssueSession`, hashed in `tokens.sign_in_key`, migration 0020) matches
  an earlier session of that person; an admin's `RevokeDevice`, a new password or disabling the account
  forgets it.
- **Library export** (`internal/catalog/export.go` + `api/handlers_export.go`):
  `GET /admin/libraries/{id}/export` (admin only) downloads a library's book list
  as `audiosilo-<library-slug>-<YYYY-MM-DD>.json` - the `{"format":"audiosilo-books",
  "version":1,…}` envelope the community metadata site imports on its Watching page
  so a user can mark which entries of a series they own. **The file leaves the
  server**, so it carries bibliographic facts only (title, authors, narrators,
  series + position, asin/isbn, runtime_min, chapters) and **never** a path, size,
  codec, format or anything else about the filesystem - a regression test asserts
  that. `catalog.ExportLibraryBooks` composes the envelope (keyset paging over
  `ListBooks`, never OFFSET; copies of the same book within the library collapse on
  `exposedDedupKey`, the kept entry taking each fact from whichever copy has it -
  `mergeExportBook`, since the copy that sorts first may be the untagged rip); the
  handler is transport-only and writes it with a `json.Encoder`. The leak guard
  asserts on the MARSHALLED key sets as an allowlist, so a field added to
  `ExportBook` fails the test even when its name (`format`, `files`) is invisible
  to a substring scan. The single `author`/`narrator` string is split into a list only
  where it clearly holds several names (`;`, ` & `, ` and `, and a comma **only**
  when every part still has two words, so "Alexandre Dumas, pere" stays whole).
  Advertised by the additive `export` capability.
- **Library admin**: `PATCH /admin/libraries/{id}` edits name/root/default_view and the scan
  settings (`scan_schedule`, `ignore_patterns`) and queues a rescan when the root or ignore rules
  change; `DELETE /admin/libraries/{id}` removes the library
  + its index (files on disk untouched). Both are surfaced in the admin console.
- **User/account admin**: `PATCH /admin/users/{id}` edits role/password/disabled
  in place (`auth.SetRole`/`SetPassword`/`SetDisabled`); `DELETE /admin/users/{id}`
  (`auth.DeleteUser` → `handleDeleteUser`) permanently removes an account and all
  of its durable state via the schema's `ON DELETE CASCADE` (sessions, auth codes,
  progress/bookmarks/notes/history, share grants) - files on disk are untouched.
  Two safety guards live in `auth`: the **last enabled admin** can't be demoted,
  disabled, or deleted (`ErrLastAdmin`), and an admin must keep a password
  (`ErrAdminNeedsPassword`); the **delete handler additionally refuses self-delete**
  (an admin disables its own account instead, never deletes it). Disabling stays the
  reversible option; deletion is the irreversible one.
  **Passwords are optional for non-admins** (stored as an empty hash; `Authenticate`
  rejects empty-hash accounts) - player-only users onboard purely via auth-code
  pairing. `GET /admin/users/{id}` returns a user + accessible libraries + granted
  shares + issued auth codes (metadata only; codes are unretrievable by design);
  `DELETE /admin/authcodes/{id}` revokes a code. A user's **last activity** is
  derived from `MAX(tokens.last_seen)` (bumped by authenticated requests in
  `ResolveRequest`, at most once a minute per token unless the request's address or
  app changed: `touchInterval`) - there is no `last_login` column; don't add one.
- **Admin stats**: `GET /admin/stats` returns catalog totals, per-library book
  counts (`catalog.CountBooksByLibrary`) and a cross-user "currently listening"
  feed (`catalog.ListeningOverview`, progress LEFT-joined to books on the path);
  with `?range=` it answers the Activity page's `activity` block instead (see Phase 4a above).
- **Progress reconciliation** is last-write-wins by `updated_at` (version breaks
  ties) in `catalog.SaveProgress` - the realtime layer (Phase C) must reuse it so
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
  zero audio files while books are still indexed. This protects the index - and
  the progress/bookmarks that cascade from it - when a network share (SMB/NFS)
  is unmounted. Library roots are always local paths; mount remote shares first.

## Roadmap

- **Phase A (done)**: auth/QR, admin, 3 views, scanner, FTS search, pagination,
  Range streaming, per-user listening state.
- **Phase A.1 (done)**: baked-in web UI (`internal/web`) - public connect page
  (auth-code box → QR + links) and a vanilla-JS admin console, replaced by the
  `admin-ui` React console at the admin redesign's Phase 1b cutover (see "Admin
  console" above). The connect and setup pages are still vanilla: all styling in
  `assets/style.css`, all behaviour in external JS - no inline `<style>`/`style=`/
  `<script>`, so the strict same-origin CSP holds.
- **Phase A.3 (done)**: **copy-invite** links (fragment-carried auth code →
  auto-redeem connect screen), app-or-web QR (HTTPS `web_url` for Universal/App
  Links + `audiosilo://` custom scheme), and the **web player** served at `/web`
  from `web_dir`. The player is the audiosilo-frontend Expo export, baked into the
  Docker image (pinned, not vendored in this repo); updates ship as a new image.
- **Phase A.2 (done)**: path identity + **filesystem-based shares** (named sets of
  path rules; filtered-tree browse; whole-library sugar), durable state re-keyed to
  the path, and cheap **move-tracking**. The admin console manages shares under
  People > Shares, with a library path picker.
- **Phase B**: `POST /uploads` → parse + placement suggestion; AAX→M4B conversion
  (user-supplied activation bytes, never stored).
- **Phase C**: `?transcode=` on the stream endpoint (ffmpeg pipe to MP3) - **done**
  (see Transcoding above). Remaining: WebSocket `/api/v1/ws` realtime sync reusing
  the last-write-wins merge + offline replay.
- **Phase D (designed)**: server federation - peering, remote shelves, hybrid
  routing (proxy catalog + signed direct stream), reusing shares as the share unit.
  See the plan file.

`GET /api/v1/server` advertises capability flags (`admin_ui`, `web_player`,
`upload`, `transcode`, `websocket`, `api_keys`, `metadata`, `export`); flip them
on as phases land. `transcode` already reflects whether ffmpeg is configured;
`api_keys` is true (user-minted personal access tokens are supported);
`metadata` reflects whether the Phase 1.5 metadata lookup is live
(`metadataOn()`: a valid `metadata.base_url` at start AND the live
`metadata.enabled`, which the admin can toggle at `PATCH /admin/settings`).

## API surface

See `internal/api/api.go` for the full route table. Public: `/server`,
`/auth/redeem`, `/auth/exchange`, `/auth/login`, the well-known association files,
and the static UI (`/`, `/connect`, `/admin`, `/web/...`). Everything else needs a
session bearer token; `/admin/*` additionally requires the admin role. The
metadata lookup is `GET /libraries/{id}/meta?path=` (authed, scope-checked like
the other `?path=` content endpoints; 404 when metadata is disabled), plus
`GET /meta/work?id=<work id>` (authed, no library scope - global community data;
404 when metadata is disabled or the work id is unknown).
The library export is `GET /admin/libraries/{id}/export` (admin only; returns a
JSON attachment, not the usual envelope - see Library export above).
Server settings are `GET`/`PATCH /admin/settings` (admin only): a section-keyed
envelope (`general`, `network`, `players`, `metadata`, `demo`, `backups`, plus `locked`,
`restart_settings`, `restart_pending`) whose PATCH takes the same shape with only
the settings to change (`{"metadata":{"enabled":false}}` still flips the lookup);
see "Server settings" above.
