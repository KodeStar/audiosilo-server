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
go test -race ./...            # unit + integration tests (SQLite + testdata fixtures)
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
The wizard creates the admin account only and hands over to the console's "Set up your
server" flow (`/admin`; see First-run setup in CONVENTIONS.md); a POST that still sends
`library_name` + `library_root` (an older page, the manager) also creates the library, as
before. The token rides in the URL
**fragment** (`/setup#token=…`, never logged), POST verifies it in constant time,
and the wizard **self-closes the moment an admin exists** (404 when never enabled).
`API.EnableSetup(token)` turns it on; `pkg/launcher` prints the URL and reports it
via `Options.OnURL` (so the audiosilo-manager desktop app, which runs the server
in-process, can open a browser).

**Before a change is done, run `go build ./... && go vet ./... && go test -race ./...
&& golangci-lint run`**, plus **`scripts/build-admin.sh`** (npm ci, then `npm run check` =
typecheck + eslint + prettier + vitest, then the build) when `admin-ui/` changed - CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml))
gates all of them on every PR/push in three parallel jobs (Go tests, lint with govet, admin console),
the Go job building the console first (`--vite-only`) so the embed tests in
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
internal/relpath/     library-relative path rules shared by catalog and auth: Clean (canonical form) and Within (the segment-boundary rule of share scopes and cast tokens)
internal/catalog/     libraries, access grants, books, FTS search, listening state (the data layer)
internal/library/     filesystem view (fsview.go: SafeJoin, and ContentPath/ContentJoin, the one hidden-segment rule) + background scanner (scanner.go: incl. IndexMoved and the .audiosilo/ walk); attachments.go (a book's Bridge attachments: signatures, RefreshAttachments); ebook.go (BookEbook, the Bridge's own .epub rule, for the estimate; a readable one only: a zip whose directory lists META-INF/container.xml, as the Bridge reads it)
internal/metadata/    dhowden/tag + ffprobe extraction (a narrator tag beats composer in both passes; mp4narrator.go reads the iTunes \xa9nrt item, which neither reads, with a bounded box walk; a series tag ending in a marked position, "Discworld, book #40", is the series and that position: seriestag.go SplitSeriesTag; a `series-part` tag "1-3" is a range: seriesrange.go, ParseSeriesRange, the one grammar of a range position, and ParseSeriesPosition, a number or a range; incl. ReleaseDate: a date tag as YYYY[-MM[-DD]], stored as books.released, never as published); DeriveFromPath (structural path parsing, the scan's baseline: the folder holding the book is the series and the one above it the author, but a lone folder is the author); layout.go: ReadPathLayout (author/series/book LAYOUT, the match's path facts) and FromPathLayout (a path-first library's values); absfile.go: ParseABSFile/ReadABSFile (an Audiobookshelf metadata.json, see Metadata overrides); attachments.go: AttachmentsDir `.audiosilo`, Read/ParseCharacters, Recaps, Description and Provenance (the Bridge's generated files, checked against audiosilo-meta's formats; see Bridge attachments); chapterlist.go: Read/ParseChapterList (the Bridge's `chapters.json`, chapters found in the narration; see Generated chapters); titlepos.go: PositionInTitle (a series position read from a title that is the series' name then a number, "He Who Fights with Monsters 4: ..."; narrow: set off by a space or comma, 1-3 digits, then the end or a separator, never a range)
internal/media/       Range streaming, download, embedded cover extraction, transcoding (a descriptor input read seekably via fd:, pipe:0 for an older ffmpeg); remux.go: Remuxer, the MP3-in-MKA remux cache (<data>/cache/remux, 8 GiB LRU, single-flight, warmed by /item and /chapters, a cold stream transcoded after 2 s, own ETag; see Recognized audio)
internal/names/       reading people in an Author/Narrator credit: Split (the deliberately shy co-credit rule), Reversed ("Surname, Given"), SortKey (surname first; the catalog registers it as the SQL function name_sort for the admin list's surname sort)
internal/meta/        Phase 1.5 community metadata lookup: HTTP client + Service (asin/isbn -> composed enrichment envelope) with a bounded TTL cache and its persistent SQLite level (store.go); the /meta bundle's previous works and spoiler gating (bundle.go); the admin console's match (match.go: metaserve works/match over tag + path facts, metadata.ReadPathLayout; works/search fallback for an older metaserve); community cover fetches (cover.go: public addresses only); owned books' work ids for the Series cards (workids.go); placing the caller's books on rails and the next rail entry (local.go); which of a work's series a book is in (seriesrules.go: the series rules below); generated content laid beside the community work (generated.go: `Answer`, `Unmatched`, `MetaGenerated`, `MetaGeneratedDescription`); cache-only reads for the Bridge's estimate (cachedenrich.go: `CachedEnrichment`; `Coverages`, a `Coverage` per book whose `Description` is the community work's `community_description`)
internal/chapteralign/ fits a community recording's chapter list onto a book's own audio (pure, no I/O): anchors by title + time, places the rest in proportion, snaps each to its pause (an injected Prober); classifies fill/titles/refine/restructure/same or why not (length_mismatch, structure_mismatch, crosses_files). testdata/mythos is a real 34-vs-174-chapter golden case
internal/chaptercheck/ the community chapter check: a background pass (every 10 min, and on Catalog.OnBookChange) and on request (Start), meta.RecordingChapters -> chapteralign (ffmpeg silencedetect via media.DetectSilences) -> catalog.SaveCommunityChapters
internal/covercolors/ the background cover colour pass: Runner reads the colour of every book whose cover may have art and holds none for it (catalog.CoverColorsDue), one at a time, once the start or a burst of OnBookChange kicks has been quiet 30 s (<= 10 min), and hourly; the reading is api.colorCover (a cached thumbnail, else an UNCACHED 160 px one), handed in as its Colorer; art with no colour is recorded as such (cover_color = the version alone; CoverSource.ColorRead, while Colored stays "has a colour", so a thumbnail that decodes still records over it); art files that aren't there (an unmounted share) are covercolors.ErrArtMissing: left due, not a failure
internal/pool/        Each: work over a list a few items at a time (the background jobs that wait on the community service share it)
internal/matchrun/    bulk community matching (Health > Not matched): a background run matches every unmatched book (or repicks community ASINs for the preferred marketplace, or refreshes the details of books that have an ASIN/ISBN by it), records each book's best candidate for review, and applies the admin's picks under a scope (plan.go is the one statement of what each scope writes)
internal/importer/    listening imports from Audiobookshelf (admin, v1): the read-only ABS client (abs.go: http/https only, same-host redirects, /status identifies ABS before the token is sent, size caps, timeouts; no private-address block - the routes are admin-only), the normalized payload, the path/ASIN/ISBN/title matcher (match.go), the pure planner (plan.go) and the background fetch/review/apply (service.go); abstest/ is a fake ABS serving recorded 2.37.1 responses (tests only)
internal/toolfetch/   on-demand ffmpeg/ffprobe download+cache (<data>/tools) when none is local; Version reads a tool's -version
internal/metamirror/  metadata mirror mode: the runner keeping a local copy of the community metadata database in <data>/meta-mirror/ (audiosilo-meta's pkg/release download, pkg/query handler over it; see "Mirror mode" below)
internal/mirrortest/  mirror-mode test support (tests only): the data releases published on audiosilo-meta's releasetest GitHub fake (querytest's artifact, a schema_version rewrite, a realistic release list), a remote metadata service that counts and answers 500 (Remote), and Eventually
internal/diskspace/   a filesystem's size and free space per OS (statfs / GetDiskFreeSpaceEx): Health > System's roots and the mirror's disk guard
internal/logring/     the admin console's log viewer: an slog handler teeing records into a bounded in-memory ring (secrets redacted)
internal/updates/     the update check: GitHub Releases' latest release, once a day while on (config update_check)
internal/backup/      database backups: VACUUM INTO the backups folder on a schedule or on request, retention, restore applied at the next start
internal/jsonfile/    the small JSON files beside the database (backup's restore marker/result, the mirror's state.json): Write is owner-only, temp file synced then renamed (all or nothing); Read[T] is nil without a file
internal/bridge/      the Bridge's side on the server (BRIDGE-PLAN.md): its last heartbeat (status.go: validation, the 2-minute online window, features, the lenient `estimates`, Writable, CanContribute: the `setup` block's, else the estimates'; setup.go: the lenient, bounded onboarding checklist and the reply's `accepts`) and the job queue (jobs.go: kinds the server can check (`bridge.verify`, `sidecars.generate`, `sidecars.describe`, `chapters.detect`, and Phase 2b's `sidecars.contribute` and `meta.correct`, contribute.go: their strict params, `ModuleFor`), dedupe, priority claim with a lease (a queued job whose managed module is off held: `ModuleOn`), long-poll wake, lease expiry, held updates and parks, cooperative cancel, `OnTransition` / `Job.Notable`); estimate.go (EstimateSidecars: a job's cost from the heartbeat's figures); pairing and the bridge token are auth's (auth/bridge.go)
internal/podcast/     podcast feed.json v1 reader and URL helpers (pure format in feed/), poll/fetch/retention scheduler, bounded on-demand media waits and scoped one-ahead prefetch; catalog/podcasts.go and podcast_{fetch,retention}.go hold the generation index, identity-bound status, follows, durable policies and aggregate checks; library/podcast_media.go opens committed media
internal/podcast/     (2c-3) follow.go: Follower, follow by URL for the single follow and OPML rows; opmlimport.go (rows, the five-new-per-upload wave); export.go (BuildExport); discovery/ (iTunes + Podcast Index search, the artwork proxy, caches/quotas/cooldown, the build-tagged test origin); opml/ (the bounded strict OPML parser and the export writer); quota/ (token buckets with a bounded key map, slots)
internal/netguard/    the guarded dialer: PublicAddr, Control/Dialer check the address actually dialled (meta covers, podcast discovery)
internal/modules/     managed modules (Bridge Phase 2a): Module (id = capability + config key, RequiresBridge, Routes, Start), Registrar, Deps, Validate, BridgeState/Active (the capability rule), Runner (each module's Start while on); modules/builtin is the registry (contributions: no routes of its own, its actions are Bridge jobs gated by `bridge.ModuleFor`)
internal/notify/      the event feed (the console's bell) and its deliveries to webhook / ntfy / Discord destinations (incl. the Bridge's `bridge_job_finished` / `bridge_job_failed`)
internal/api/         HTTP transport: routing (api.go), middleware, rate limiting, handlers_*.go
internal/server/      HTTP(S) server, TLS modes (off/selfsigned/autocert), home HTTP on the self-signed port, graceful shutdown
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
`library` → (`catalog`, `metadata`, `config`); everything DB-backed → `store`; `catalog` and `auth`
share `relpath` (the segment-boundary path rule of share scopes and cast tokens), so `auth` never
imports `catalog`.
`api` is transport-only - keep business logic out of handlers.

## Identity = the filesystem path

Audiobook metadata is unreliable, so **the path is the identity**. Content is
addressed by `(library_id, rel_path)`; playback, progress, bookmarks, notes and
share membership all key on the path. `books.id` is an internal, rebuildable
index artifact - never put it in the API contract or in durable user state. A
cheap fingerprint (sha256 of size + first/last 64KB, stored in `books.content_hash`)
is used **only** to detect moves; it is not an identity.

## Data model (SQLite, see internal/store/migrations/)

`users`, `tokens` (sessions, API keys, pairing and cast tokens, hashed; pairing tokens minted from an
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
`book_covers` (custom cover blobs; `0036` adds `source`, `edited` for an upload or `community` for a
match's, backfilled from the audit log and match runs). Phase 3 (`0017`) adds `scan_runs` (the job queue's scan
history, per library, bounded), `issue_ignores` (`library_id, path, kind`: an admin's "ignore this"
on a Health issue; path-keyed, moves with the book), `libraries.scan_schedule` / `ignore_patterns`
(per-library settings, off the player wire) and, on `books`, `scan_error` / `scan_error_file` /
`scan_error_detail` (the last indexing's read problem) and `suspect_parts`; `0022` adds `books.split_parent` (the
folder holding a disc of a book split across disc folders, else `''`); `0023` adds
`books.cover_art` (the cover art identity whose short hash is the wire `cover_version`) and
`books.cover_color` (read from a thumbnail, tagged with the version it was read for; both derived, see below) and `0024`
`meta_cache` (the community metadata cache's persistent level: derived, keyed by identifier,
not user state; see Phase 1.5 below). `0035` adds `community_chapters`, `chapter_choices` and `books.chapters_source` /
`chapters_fit` / `scanned_chapters` / `chapters_hash` (see Community chapters below). `0027` adds `ratings` (a listener's 1-5 stars + note per
book: durable, path-keyed, no FK to the index, purged with its user or library). Phase 4a (`0018`) adds
`listening_sessions` (server-derived listening sessions, path-keyed, no FK to the index, bounded
retention), `listening_daily` (their per-day roll-up), `tokens.client_app` / `client_version` /
`client_platform` / `last_ip` (the app and newest address behind each token) and
`progress.started_at` / `finished_at`. Phase 5b (`0019`) adds `audit_events` (the admin audit log), `notification_targets`
(where notifications go) and `server_events` (the console's bell); backups are files, not rows. Player
redesign Phase 1b `0028` adds `listening_goals` (`user_id` PK → users CASCADE, `books_per_year`
1-1000: a person's yearly goal; names no book, so nothing moves it) and `0029` the index
`listening_sessions(user_id, last_at)` (a person's own stats read their sessions by it). Phase 4
`0030` adds `bookmarks.label` (a machine key, `''` for none: `catalog.checkBookmark` checks only its
shape, `^[a-z][a-z0-9_]{0,31}$`, never the player's set of keys) and `0031` the indexes
`bookmarks(user_id, created_at)`, `notes(user_id, created_at)` and `listening_history(user_id,
ended_at)` behind the `annotations` capability's lists (`catalog/annotations.go`: `GET /me/bookmarks`,
`/me/notes`, `/me/history` newest first, keyset-paged on (timestamp, id) after the access filter, an
opaque `next_cursor`, each row's `book` attached per page; `PATCH /bookmarks/{id}`, `/notes/{id}` are
the owner's edits, one writer transaction, 404 for another user's id or a path outside current access;
an edit checks only the fields it sets). Their `created_at`, a note's `updated_at` and a listening span's
`started_at`/`ended_at` are fixed-width UTC milliseconds (`c.stamp`; a client's span times are
normalised, an unparsable one replaced by the server's; `0031` rewrites the rows stored before it),
since the lists order by them as text. The
per-book `GET /libraries/{id}/bookmarks|notes|history` answer `[]` when empty (`null` before). `0034` adds listening imports
(`internal/importer`, `catalog/imports.go`): `imports` (one per source user and AudioSilo user; status,
cutoff, the gzipped fetched payload, the review's summary and unmatched list; never a credential),
`import_id` on `listening_sessions`, `listening_daily`, `bookmarks` and `listening_history` (0 = recorded
here), `bookmarks.import_note` (the note the import wrote: an undo keeps an edited one) and
`import_progress_prior` (the progress row before an import and the row it wrote, path-keyed, carried by
`carryListeningState`; an undo restores only rows still as the import left them). Sharing:
`shares` (named), `share_paths` (`library_id`, `path`; `""` = whole library),
`user_share_access`.

Player redesign Phase 1b: `up_next` (`0025`: a user's queue) and `collections` /
`collection_items` / `collection_shares` (`0026`: named lists, shared read-only with named
users). Items are path-keyed like favourites (no FK to `books`), ordered by `position`, and
share ONE implementation (`catalog/lists.go` `orderedList`: add-at/move as one range shift plus
the row, remove leaving a gap (positions need not be dense), replace with the skip rule writing
only the rows that changed, books attached by `booksAt`); every read passes through the
READER's current access (a viewer never sees or counts an owner's item outside their own
shares; such "hidden" rows are kept). An add works in the list as the CALLER sees it (their
scopes; for a collection, the owner's): `position` is a 0-based index among the visible rows
(lands just before the visible row at it; absent or past the visible end = just after the last
visible row; hidden rows stay put), and the caps (`MaxQueue` 500, `MaxCollectionItems` 1000)
count visible rows: a new book the hidden rows alone would overflow evicts the oldest hidden
rows (`added_at`), 409 only when the visible rows are at the cap (a whole-list PUT replaces
everything, hidden rows included, except podcast entries an old player can't see, which it keeps
(see Podcasts in CONVENTIONS.md), and its length check is unchanged); a move or join carries them
(`carryListeningState`, the destination entry kept on a collision).
Player redesign Phase 8: `household_sharing` (`0042`: `user_id` PK -> users CASCADE,
`share_listening` 0/1; no row = off; names no book) and `cast_grants` (`0043`: a cast token is a
`tokens` row of kind `cast`, expires_at set; its grant is `token_id` PK -> tokens CASCADE,
`library_id` + `rel_path` of the book, `parent_token_id` -> tokens CASCADE (the sender's token that
minted it); path-keyed, no FK to the index, short-lived, not moved with a book).
See "Listening in the house" and "Cast tokens" below.
Player redesign Phase 8b-1 (`0044`): `users.managed_by` (-> users SET NULL), `member_kind`
(`kid`|`grownup`), `member_access` (`all`|`collections`), all three NULL or all set (kept by `auth`,
SQLite can't add that CHECK), `users.can_add_members` (the admin's per-person switch, default 1),
`users.display_name` (the name an owner typed for a member) and `tokens.created_on_token_id` (-> tokens
SET NULL: which of the owner's devices a member's sign-in was made on; information only). See
"Household members" below. `0045` adds `books.series_index_end` and `book_more_series.position_end`
(a range's end, 0 for none; see Series ranges below; the list's key index now covers it). `0046` adds
`books.abs_sig` / `abs_indexed_at` / `abs_file` (an Audiobookshelf `metadata.json` in a folder book's
folder: its signature, the index it was read for, what it says; derived from the file; see Audiobookshelf
metadata.json below). `0047` adds `bridge_jobs` (the Bridge's jobs: path-keyed, no FK to `books`, FK to
`libraries` with cascade, `requested_by` -> users SET NULL, a partial unique index on `dedupe_key` among
open states) and the one-row `bridge_status` (the pairing code's hash and expiry, who paired the Bridge and
when, its last heartbeat); see The Bridge below. `0048` adds `books.attachments_sig` / `attachments_indexed_at`
and `book_attachments` (book_id -> books CASCADE: the Bridge's checked `.audiosilo/` characters, recaps and
provenance; derived from the files like `abs_file`, so rebuildable and never carried by a move). `0049`
adds `books.subtitle` / `publisher` / `language` / `abridged` (NULL unknown) / `genres` / `tags` (effective
values; the lists canonical JSON), `books.genres_checked` (the genre tag's one-time backfill, as 0037's
`released_checked`), `book_labels` (book_id -> books CASCADE, `library_id`, kind = the field,
`genres`|`tags`, `label_key`, the book's spelling: derived, rebuilt by `refreshEffective`), recreates
`books_fts` with `genres, tags` columns (rows copied) and sets `abs_indexed_at` NULL on every book with
a metadata.json (`abs_sig <> ''`), so the next scan reads each again for the new fields; see Labels
below. `0050` adds `book_attachments.description` (the Bridge's checked `description.json`, Phase 1c;
`''` for none; derived like the rest of the row). `0051` adds `book_attachments.chapters` (the Bridge's
checked `chapters.json`, Phase 1d, a chapter source of its own: see Generated chapters; `''` for none).
`0053` inserts `server_state('setup_state')` as finished and dismissed on a server that already has a
library or a second account (not a demo or member), so an upgraded server never shows the first-run flow.
`0055` adds `books.file_order` (a folder book's file order key, `catalog.FileOrderKey` bit-cast to a
signed integer) and `file_order_indexed_at` (the index it was recorded with; NULL or another index's,
an older server's re-index, = not recorded, filled by the next scan without a re-index; derived; see
Auto book/folder detection in CONVENTIONS.md).

Routes `/me/queue`, `/me/collections/**`, `/me/share-targets` (`handlers_queue.go`,
`handlers_collections.go`; a stranger's collection id is 404, a viewer's write 403
`not_owner`, both settled before the body is checked; a whole-list PUT without its
`items` / `user_ids` array is 400, never "empty it"; a viewer disabled since being shared
stays in `shared_with` and may be sent back; capabilities `queue`, `collections`).

Book identity carries `author`/`series`/`title` plus optional `asin`/`isbn` so a
future metadata site can attach enrichment without reshaping the schema. The
`books` metadata columns are the effective values (scan, then an Audiobookshelf
`metadata.json`, then enrichment, then admin overrides; see Metadata overrides below).

## Reference: CONVENTIONS.md (read on demand)

These topics live in [CONVENTIONS.md](CONVENTIONS.md), not here, to keep this file small.
**Before changing code a topic below covers, read its entry** (`grep -n '<topic>'
CONVENTIONS.md`, then Read that range). When you add or change a rule, edit it there and
keep this index in step (one line per topic).

- **Conventions**
  - Every feature ships with a test
  - Migrations are append-only
  - Secrets
  - Connect / invite / pairing flow
  - Home and away addresses (`addresses` capability)
  - Home HTTP (`tls.home_http`, security-relevant)
  - Invite vs recovery (`auth_codes.kind`)
  - Personal API keys (`tokens.kind='api'`)
  - Web player at `/web`
  - Admin console (`admin-ui/` + `internal/web/adminui`)
  - Community metadata lookup (Phase 1.5, `internal/meta`)
  - Native deep-link association
  - SQLite
  - Pagination
  - Path safety
  - Path-addressed API
  - Recognized audio
  - Transcoding
  - Sharing & access (shares)
  - Move-tracking
  - Auto book/folder detection
  - Metadata overrides (admin redesign Phase 2a, `catalog/overrides.go`)
  - Series spellings (`catalog/moreseries.go`)
  - Admin catalog API
  - Rate limiting by route class
  - Health issues (`catalog/issues.go`, `api/handlers_health.go`)
  - Server settings, system status, updates, logs (admin redesign Phase 5a)
  - Backups, audit log, notifications (admin redesign Phase 5b)
  - Support card
  - Library export
  - Library admin
  - User/account admin
  - Admin stats
  - Progress reconciliation
  - Chapters are normalized
  - Community chapters (incl. Generated chapters, Bridge Phase 1d)
  - Podcasts (Bridge Phases 2c-1, 2c-2a, 2c-2b and 2c-3: discovery, OPML import/export)
  - Managed modules (Bridge Phase 2a, `internal/modules`, `api/modules.go`)
  - Unavailable-root guard
  - First-run setup (Phase F1, `catalog/setup.go`, `library/preview.go`, `api/handlers_admin_setup.go`)

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
`upload`, `transcode`, `websocket`, `api_keys`, `metadata`, `meta_bundle`, `meta_covers`,
`export`, `browse_people`, `series_memberships`, `series_books`, `cover_sizes`, `next_book`, `queue`, `collections`,
`user_stats`, `ratings`, `progress_edit`, `my_devices`, `annotations`, `addresses`, `household`, `cast`,
`household_members`, `bridge`, `podcast_fetch`, `podcast_discovery`, `podcast_opml`), plus one flag per managed module (`contributions`, `podcasts`); flip them on
as phases land. A module's flag is its toggle AND its Bridge requirement met (see Managed modules). `bridge` is true only while a paired Bridge's heartbeat is under two minutes old, with
the `bridge: {version, features}` block beside `capabilities` for a signed-in caller only (see The Bridge above). `series_memberships` is true (books in several series: `memberships=1` on
`/libraries/{id}/books?series=` and `/libraries/{id}/series`, and `series_list` on a player
`Book` in more than one series; see API surface below). `series_books` is true (the first page of several
series' books in one request, `GET /libraries/{id}/series/books`; see API surface below). `addresses` is true (home/away addresses on pairing, exchange and login, and
`GET /addresses`; see Home and away addresses above). `household` (`/me/household`,
`/household/listening`) and `cast` (`POST /cast/token` + cast tokens on a book's cover/stream) are
true (player redesign Phase 8; see Listening in the house and Cast tokens above). `household_members`
is true (`/me/household/members`; player redesign Phase 8b-1, see Household members above).
`browse_people` is true (the player's browse lists and `/books?narrator=`),
`cover_sizes` is true (`GET /libraries/{id}/cover?size=`) and `next_book` is true
(`GET /libraries/{id}/next`). `queue`, `collections`, `user_stats`, `ratings`,
`progress_edit` and `my_devices` are true (player redesign Phase 1b, see the API surface
below and Your listening). `transcode` already reflects whether ffmpeg is configured;
`api_keys` is true (user-minted personal access tokens are supported);
`metadata` reflects whether the Phase 1.5 metadata lookup is live
(`metadataOn()`: a valid `metadata.base_url` at start AND the live
`metadata.enabled`, which the admin can toggle at `PATCH /admin/settings`);
`meta_bundle` (`/meta`'s `include=previous` / `spoilers=hide`) and `meta_covers`
(`GET /libraries/{id}/meta/cover`) track `metadata`.

## API surface

See `internal/api/api.go` for the full route table. Public: `/server`,
`/auth/redeem`, `/auth/exchange`, `/auth/login`, the well-known association files,
and the static UI (`/`, `/connect`, `/admin`, `/web/...`), and the Bridge's pairing exchange
(`POST /bridge/pair`). Everything else needs a session bearer token (or an API key), except the
Bridge's own routes (`/bridge/*`), which take only a bridge token; `/admin/*` additionally requires
the admin role. The
metadata lookup is `GET /libraries/{id}/meta?path=` (authed, scope-checked like
the other `?path=` content endpoints; 404 when metadata is disabled), plus
`GET /meta/work?id=<work id>` (authed, no library scope - global community data;
404 when metadata is disabled or the work id is unknown). The player's community
covers come through `GET /libraries/{id}/meta/cover?path=&url=&size=` (`meta_covers`;
see Community covers).
The player's browse lists are `GET /libraries/{id}/authors` (`{authors, unknown}`),
`/narrators` (`{narrators, unknown}`) and `/series` (`{series}`) (authed,
`libraryScope`: 403 no access, 404 unknown library; counts only the caller's
granted books; `api/handlers_browse.go`), and `GET /libraries/{id}/books` filters
by `series=` (any spelling, see Series spellings), and by `author=` / `narrator=` matching the whole credit or exactly
one person it names (`catalog.creditFilter`: an FTS5 phrase narrows the candidates,
the `credit_has` SQL function keeps the exact ones; `/admin/books` filters the same way),
and by `genre=` / `tag=` (one value each, any case, `catalog.labelFilters`; see Labels; an older
server ignores them, so a player filtering a loaded library by the books' own `genres` / `tags`
needs no capability).
Books in several series (capability `series_memberships`): with `memberships=1`,
`/books?series=` also matches a book whose `more_series` names it (`catalog.seriesFilter`,
as `/admin/books`) and `/series` counts a book in every series it is in (`extra_books`);
without it both see main series only, as a client that places books by `series_index`
needs. A player `Book` in more than one series carries `series_list` (every series, the
main one first, with its position in each; omitted otherwise). A position that is a range
(an omnibus) adds `series_index_end` / an entry's `position_end` (omitempty; `series_index` /
`position` stay the start).
The first page of several series' books in one request (capability `series_books`, the
player's Series cards) is `GET /libraries/{id}/series/books?name=A&name=B[&limit=N]` ->
`{"series": [{name, books, next_cursor?}]}` (authed, `browseScope` like `/books`; 400 for no
names or over `maxSeriesBatch` = 50 distinct ones). Names come back verbatim (each finds its series in any spelling, by its key), empty ones ignored
(`catalog.ListSeriesBooks` skips them too), duplicates answered once, entries in request
order; each entry is exactly the page `/books?series=<name>&memberships=1&limit=N` returns
(`ListSeriesBooks`: one `ListBooks` per name), so its `next_cursor` continues there, with
`books: []` for a series with no books in scope.
Player redesign Phase 1b (`api/handlers_ratings.go`, `handlers_mydevices.go`): **ratings**
are `GET`/`PUT`/`DELETE /libraries/{id}/rating?path=` (`{"rating": Rating | null}`; `Rating =
{library_id, path, rating 1-5, note, created_at, updated_at}`; PUT `{rating, note?}` resolves a
part path to its book with `bookAt` and stores on the book's path, 400 for a rating that isn't a
whole 1-5 or a note over 500 runes after trimming; GET/DELETE are exact via `authorizedPath`;
DELETE 204 idempotent) and `GET /me/ratings` (`{"ratings": [Rating + book?]}`, newest first,
scope-filtered like favourites: a revoked share hides a rating, never deletes it; books via
`BooksByPaths`). Table `ratings` (`0027`), path-keyed, carried by `carryListeningState` (a
collision keeps the newer `updated_at`, whole). **My devices** are `GET
/me/devices` (the caller's own live sessions and API keys, `auth.ListDevices(caller)` without
`user_id`/`username`, `current` marks the request's token) and `DELETE /me/devices/{id}` (200
`{"current": bool, "members_signed_out": int}`; `auth.RevokeOwnDeviceAndMembers`, with
`?also_members=1` also signing out the caller's household members' sign-ins made on that device;
owner-scoped: anyone else's, unknown, revoked or
pairing id is 404 and untouched; revoking the current device is allowed and its token is dead from
the next request; an API-key caller may revoke).
Listening in the house is `GET`/`PUT /me/household` and `GET /household/listening`; casting is
`POST /cast/token` (see the two Phase 8 bullets above). Household members are
`/me/household/members[/{id}[/session|/pair|/devices[/{device_id}]]]` (see Household members above).
What to play after a book is `GET /libraries/{id}/next?path=` (authed, scoped like
`item`: 400 bad id/missing path, 403 outside the grant, 404 no library/book;
`{source: community|series|folder|none, next?, book?, work?}`, see Next book above).
The library export is `GET /admin/libraries/{id}/export` (admin only; returns a
JSON attachment, not the usual envelope - see Library export above).
Server settings are `GET`/`PATCH /admin/settings` (admin only): a section-keyed
envelope (`general`, `network`, `players`, `metadata`, `demo`, `household`, `backups`, `modules`, plus `locked`,
`restart_settings`, `restart_pending`) whose PATCH takes the same shape with only
the settings to change (`{"metadata":{"enabled":false}}` still flips the lookup);
see "Server settings" above.
