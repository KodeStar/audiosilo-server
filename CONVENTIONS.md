# AudioSilo Server - conventions

Moved verbatim out of CLAUDE.md so it is read on demand instead of loading into
every session. CLAUDE.md keeps a one-line index of these topics.

## Conventions

- **Every feature ships with a test.** Handler/integration tests use the
  `newTestEnv` harness in `internal/api/api_test.go` (a temp-file SQLite, so reads
  go through the read-only reader pool as in production and a write sent through a
  read method fails the test, + `testdata/library` fixtures; `catalog`'s
  `newTestCatalog` is file-backed for the same reason). Never migrate per test: open a test database with
  `storetest.Open(t)` (`internal/store/storetest`; `storetest.Path(t)` for a file a test opens itself), not
  `store.Open` on a new file or `:memory:`. Under `-race` running every migration costs about a second, so it
  migrates once per test binary and copies the file per test (file-backed, so reads use the reader pool).
  A package creating many password users calls `auth.UseCheapHashingForTests()` from `TestMain`
  (`internal/api`, `internal/auth` and `internal/matchrun`'s `main_test.go`; a real-cost argon2id hash is ~0.25 s under `-race`; it panics outside a test
  binary). A new top-level test starts with `t.Parallel()` (each has its own database and
  temp dirs; the only thing tests share is the read-only `testdata/` fixtures, so copy one into
  `t.TempDir()` before changing it: `copyFixtures` in `internal/api`) unless it calls `t.Setenv` or `t.Chdir`
  (which panic in a parallel test) or writes a package-level var (pass the value in instead, as
  `hashWithCost` does for `TestHashCost`). Pure-logic tests sit next to the code (see
  `internal/api/middleware_test.go`, `internal/catalog/shares_test.go`,
  `internal/web/web_test.go`). **Security-critical code requires both an allowed
  and a denied regression test** - anything touching `library.SafeJoin`,
  `Scope.Allows`/`VisibleInBrowse`/`pathFilterSQL` (its single-parameter form too), the household member rule
  (`catalog/members.go`) and the member routes, the rate limiters,
  `auth.ResolveRequest`/`lookupToken`, the cast token checks (`relpath.Within`/`Clean`, `auth.castGrant.covers`,
  `ResolveCastRequest`, `api.authenticate`/`castOwnBook`), or `web.htmlCSP`. Keep business logic in the non-`api`
  packages so it stays unit-testable (`api` is transport-only).
  A test never holds code to a time in milliseconds: CI's shared runners take several times as long as a laptop,
  and more when busy. How a read grows is compared across sizes on the one machine (`TestSeriesKeyScale`) and
  its shape held by its plan (`queryPlan`: `TestNextInSeriesPlan`, `TestIssueCountsPlan`).
- **Migrations are append-only**: add `internal/store/migrations/000N_*.sql`;
  never edit an applied migration. Applied names are tracked in `schema_migrations`.
  The executor sorts filenames and skips exact recorded names, allowing a missing lower-numbered
  migration to arrive later. Phase F1 owns 0053 and podcast downloads own 0054; neither depends on the other.
  `Open` still starts on a database recording migrations this server doesn't ship (a newer server ran on
  it: a rollback) and warns once, naming them; a restore refuses one (`Inspect`, `ErrNewerDatabase`).
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
  domain). Both end with `sid=<server_id>` (after `home=`/`away=`; also the payload's
  `server_id`), so an app falling back to the link's http home checks `GET /server`'s
  `server_id` there before sending the token in cleartext. **The QR is as redeemable as the invite**: a pairing token minted by
  redeeming a code is linked to it (`tokens.auth_code_id`) and inherits the
  invite's uses/expiry, so each device being set up can scan the same QR (the
  redeem response's `uses_remaining`/`code_expires_at` advertise the budget);
  recovery-derived tokens instead last `auth.recoveryPairingTTL` (multi-scan
  within it), and
  `/auth/pair` + demo tokens stay unlinked/single-use (`/auth/pair`'s optional
  `{ttl_seconds}` body shortens its 10 minutes, `pairTTL` clamping it to 30 s..10 min,
  never longer; no body is the full 10 minutes, a malformed one 400; the console's book page
  "Listen in player" mints a short one with `/auth/pair` and opens the same-origin
  `/web/connect?token=&sid=&uid=&book=&path=` in a new tab, never the payload's
  `web_url`, whose base may be another origin). Invite codes minted via
  the admin API default to 5 uses / 1-day expiry (`defaultAuthCode*` in
  `handlers_admin.go`); explicit values override.
- **Home and away addresses (`addresses` capability)**: a server
  has an **away** address (`public_url`, works from anywhere) and may have a **home** one
  (`lan_url` / `AUDIOSILO_LAN_URL` / console `general.lan_url`, live; else derived from the
  request's own `Host` when that is a home-network host). The pure core is
  `config.Addresses(scheme, host)` + `config.isHomeNetworkHost` (private RFC 1918 / ULA
  `fc00::/7` / link-local IPs, `.local` / `.lan` / `.home.arpa` / `.internal` names, single-label names;
  NOT loopback, `localhost` or CGNAT `100.64.0.0/10`); a home equal to the away is dropped;
  `X-Forwarded-*` is not trusted (like `baseURL`), and a proxied request (any forwarding
  header) derives no home (its Host is the proxy's upstream). `api.addresses(r)` wraps it. It rides on
  every pairing payload (`addresses`, nil when both empty) and as `home=`/`away=` params
  APPENDED after the existing ones on `uri` and `web_url` (only when non-empty), on the exchange / login / demo session
  envelopes (`sessionEnvelope`), and at `GET /addresses` (any signed-in user, `{}` when
  neither) so an already-paired device learns a newly set address. Native apps probe the
  home address and check its `server_id` before sending a token there.
- **Home HTTP (`tls.home_http`, security-relevant)**: the apps can't trust a self-signed
  certificate, so on `tls.mode: selfsigned` with `home_http` on (the default;
  `AUDIOSILO_TLS_HOME_HTTP`, console `network.tls_home_http`, restart; `config.ServesHomeHTTP`)
  the bind port answers both protocols. `server.splitListener` peeks each connection's first
  byte in its own goroutine (bounded by the 10 s header timeout; Close drops a peeking
  conn): `0x16` becomes a `*tls.Conn`, anything else stays plain, and ONE `http.Server`
  serves both (ALPN `h2` set by hand, as `ServeTLS` would). `server.homeHTTPGate` serves a
  plain request only when the connection's own `RemoteAddr` (never `X-Forwarded-For`) is
  loopback, or `config.HomeNetworkIP` (private/ULA/link-local; not CGNAT) AND the Host is
  `config.HomeNetworkHost` (`homeRequest`: a NAT/proxy gateway's private source, Docker's
  IPv6 proxy say, still carries the internet client's public Host); anyone else gets a 308
  to the same URL on https (`:80` for a port-less Host, as `baseURL`; 400 for an unusable
  Host). Off whenever `tls.cert_file` is set (`ServesHomeHTTP`: a supplied cert may be trusted).
  `config.Addresses` then derives the home address as `http://` even for an https request,
  keeping the request's port (`:443` added when the Host has none, the port that answers
  http); a configured `lan_url` is used as is, and `base_url`/`web_url`/`uri server=` are
  always https (`api.baseURL`: a plain-HTTP request gets the https base on its port, 80 for a
  Host without one, `config.HostWithPort`; a proxied one keeps its port-less Host, https's 443), so a link copied from a console opened over http stays https. No HSTS on self-signed and no `upgrade-insecure-requests` in any
  CSP (`TestCSPAllowsPlainHTTP`), so the console and player work over http on the LAN.
- **Invite vs recovery (`auth_codes.kind`)**: an auth code is either an admin-minted
  `invite` (bounded) or a user-owned `recovery` code (durable: unlimited uses, never
  expires; a password change or reset and Sign out everywhere delete it, see Password ends
  sessions). Both pair through the same `ResolveAuthCode` → `IssuePairingToken` →
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
- **Password ends sessions (server#281)**: replacing or clearing a password the account had
  (`auth.UpdateUser`: the self-service `POST /auth/password` and the admin's `PATCH /admin/users/{id}`
  alike; a first password ends nothing) revokes every `session` token of that person but
  `UserChanges.KeepToken`, the caller's own (so changing your own password, or an admin resetting
  their own, keeps the device it was done on), in the same durable transaction (`endSessions`). The
  admin's **Sign out everywhere** (`POST /admin/users/{id}/sign-out`, `auth.SignOutEverywhere`,
  answers `{"signed_out": n}`, audit `user.sign_out_everywhere`) does the same without a password
  change, and forgets the person's browsers. With the sessions go the ways back in a session can make
  without the password, which the person can neither see nor revoke: their open pairing codes
  (unlinked `pairing` tokens, `POST /auth/pair`) and their recovery code (its linked pairing tokens
  cascade). Their household members go the same way where the person put them (server#285): a member's
  session made on any of the person's devices but the kept one (`created_on_token_id`, the device
  revoke's `also_members` rule, a device signed out earlier included) and every member's open pairing
  code; a member's own paired device stays, and `signed_out` counts the person's sessions only. API keys stay (listed and revoked on their own: the device lists, `/auth/tokens`), and so
  do the calendar `feed` token (the person's to end, `DELETE /me/upcoming/calendar`; the console has no
  lever for it) and Bridge tokens (the Bridge's); an invite is the admin's (listed and revoked in the
  console). A cast token needs no write (its parent is checked on every request, `castGrant`). The
  write moves the content generation, so the token cache refuses an ended token on the very next
  request, media `?token=` included. A password sign-in racing the change gets no session:
  `IssueSession` writes it only while the hash `Authenticate` verified is still the account's and the
  account enabled, in its writer transaction, else `ErrInvalidCreds` (401). A shipped app takes its
  existing 401 path (signed out, pair again).
- **Personal API keys (`tokens.kind='api'`)**: user-minted, **non-expiring** bearer
  tokens for headless integrations, owner-scoped mint/list/revoke via
  `POST`/`GET`/`DELETE /auth/tokens` (`IssueAPIToken`/`ListAPITokens`/
  `RevokeTokenByID`; label carried in `device_name`, ≤100 chars). `requireAuth` now
  accepts **session OR api** (`ResolveRequest(..., KindSession, KindAPI)`), so a key
  authenticates like a session acting as its owner (an admin's key passes
  `requireAdmin`; media `?token=` accepts it too) but is never valid for pairing
  `/auth/exchange`; a pairing token is never accepted as a bearer credential.
  Secrets are stored SHA-256-only and shown once; create/revoke go through
  `gateSelfService` (shared `accountLimiter` + demo refusal; its 429 carries Retry-After), the
  list through `nonDemoAccount` alone (a read never spends the per-IP mutation budget a NAT
  household shares). **Containment**: a key
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
  and sets a **scoped CSP** per HTML response (strict `script-src`: `'self'`, the Cast Web
  Sender's three scripts on `https://www.gstatic.com` by PATH (`web.castSenderScripts`:
  `/cv/js/sender/v1/cast_sender.js`, `/cast/sdk/libs/sender/1.0/cast_framework.js`,
  `/eureka/clank/`; capability `cast`; the ONLY remote sources in the policy, never the whole
  origin, which also serves AngularJS, a CSP-bypass gadget; `TestHTMLCSPCastSender` pins the
  whole string) and a sha256
  hash of that doc's inline scripts; `style-src` allows `'unsafe-inline'` for
  react-native-web's runtime styles). A `.html` name in any case is a document (`spa`'s `serve`:
  a case-insensitive `web_dir` opens `INDEX.HTML` too, which must never go out under the file
  policy; `TestCSPDocumentAnyCase`). Admin/connect pages keep the stricter
  site-wide CSP. Compatibility is by construction (the image pins a matching web
  build); native apps negotiate via `GET /server` capability flags.
  **The reader document** (Bridge Phase 3b, `web.readerDocument`): `GET /web/reader.html` is the
  export's `reader.html` (the frontend's `public/reader.html`: foliate-js and the reader in ONE inline
  script plus an inline style, no subresources), which the player's reader screen frames. The `/web/`
  handler sends every path that would name it (`web.isReaderPath`: `reader.html` or `reader`, any
  case, a trailing slash) to it, so it is never served under the player's policy, and a player route
  must never be named `reader`. It gets its own policy (`web.readerCSP`, READ-ALONG-RESEARCH "Reader
  recipe"; `TestReaderCSP` pins it): `default-src 'none'`, `script-src` only the sha256 of the
  document's inline scripts as served (`web.inlineScriptHashes`, shared with `htmlCSP`; `'none'` with
  none), `style-src 'unsafe-inline' blob:`, `img-src`/`font-src blob: data:`, `frame-src blob:` (the
  EPUB's section frames, which inherit it), `media-src`/`connect-src`/`object-src`/`base-uri`/`form-action
  'none'`, `frame-ancestors 'self'`; `X-Frame-Options: SAMEORIGIN` replaces the site-wide `DENY`;
  `Cache-Control: no-cache` with the ETag (`spa.Files.ServeDocument`). No `reader.html` in the export
  (an older web build) is a 404, never the app shell. `/web/`'s own policy is unchanged
  (`TestHTMLCSPCastSender`, `TestReaderDocument`). foliate never runs in the `/web` document.
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
  are logical work-chapter positions, on the scale the work states as `position_scale`, answered on
  this book's chapters when it splits finer: `meta.OnRecording`, per request on a copy, a reveal
  at its unit's start and a through at its unit's end, later of time and count, see CROSS-REPO section 17) when metaserve has them, plus a
  whole-book `recap_summary` (`{in_short, ending}` - both summarize the whole
  book, and `ending` is a full spoiler by construction); all three are additive/`omitempty`, mirrored on
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
  Config is `metadata.{enabled,base_url,region,mode}` (env `AUDIOSILO_METADATA_ENABLED` /
  `AUDIOSILO_METADATA_BASE_URL` / `AUDIOSILO_METADATA_REGION` / `AUDIOSILO_METADATA_MODE`; `base_url` must be an
  absolute http(s) URL when enabled; `region`, the preferred Audible marketplace, one
  of `config.Regions` or "", applies live; `mode` is `remote` or `mirror`, read at start, see Mirror
  mode below) - one key disables ALL outbound calls (lookups and the mirror's downloads). **Runtime toggle**: `meta.Service`
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
  transport-only. **From the recorded match** (LibriVox L5): a book with no asin/isbn, or whose
  identifier upstream doesn't know (`ErrNotFound`), answers from its `book_matches` row
  (`catalog.BookMatchAt`, `bookEnvelope`) through `meta.Service.EnrichMatch` (works/{id} via
  `workDetail`, `fetchWork`'s read without its `workSem` slot (the id comes from a book the server
  holds, as `compose`'s), the recording it names treated as a lookup-named one, no lookup), cached
  exactly as `Enrich` (shared `enrich`) under its own key space `m:<work id>/<recording id>` (each part
  `url.PathEscape`d); a book its identifiers match never reads its match. Degradation: disabled ->
  404 (and the `metadata` capability is false, so clients hide the UI); no asin/isbn match and no
  recorded match -> `200
  {"matched": false}` (with a `work` of the book's own when the Bridge generated
  characters, recaps or a description for it: `meta.Answer`); upstream unreachable -> 502, unless
  the book has generated content (this server's own), answered as unmatched with
  it. **Generated description** (Phase 1c): a book's `description.json` rides in the additive
  `work.generated_description: {text}` (`MetaGeneratedDescription`), NEVER in `community_description`
  (a shipped 2.0 app shows that first and credits the community; it ignores the new field), and
  `work.generated.description` (bool, always sent beside `characters`/`recaps`) marks it. It fills
  only a work with no `community_description` (the core `description`, the publisher's kind, doesn't
  block it: the player orders those). `fillable` (generated.go) is the one statement of which parts a
  work leaves to fill (`metadata.AttachmentParts`): `Fillable` loads by it and `parts` lays in by it.
  `generatedFor` reads only the parts it names that the book's signature names too
  (`Book.GeneratedParts`), plus the provenance (`catalog.BookAttachments` with a parts mask: a work with
  community characters and recaps reads just the description and its provenance, and none for a book
  without one); `?spoilers=hide` keeps
  it (spoiler-free by construction: the Bridge writes it from the opening chapters' notes only), and
  attribution never counts it. `generated.generated_at` is bridge.json's `generated_at` (the
  characters' and recaps' run), or its `description.generated_at` (`Generated.DescriptionAt`) when the
  description is all the mark names: a description-only job keeps the run's time. An unmatched book
  with only a description answers `{matched: false, work: {title, generated_description, generated:
  {description: true, ...}, ...}}`. `generated.recaps` is set when the recap list or the whole-book summary
  came from the book's `recaps.json` (unchanged for older clients); the additive
  `generated.recap_summary` (always sent, false included: the player falls back to `recaps` only when
  it is absent, an older server) is true only when the summary itself did, so a community work with
  a summary alone (which `fillable` leaves the list to fill) keeps its summary credited to the
  community beside a generated list. Out of scope for now: no cover remote-fallback, no tag-based ASIN extraction.
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
  **Series rules** (`meta/seriesrules.go`): which of a work's series a book of it
  is in. Never a variant reading order (`ordering_of`), a translation of another of
  the work's series (`translation_of`; unless that one's derived `language` differs
  from the work's) or a series whose derived `language` differs from the work's
  (primary subtags; unknown agrees); an EDITION - a series named
  exactly like another of the work's plus a bracketed qualifier or a "narrated by"
  tail ("Discworld (abridged)", "Harry Potter (Full-Cast Editions)") - only for the
  recordings its qualifier names (`editionHolds`: abridged, full cast = >= 4
  narrators or one named "Full Cast", the named narrator; "(Unabridged)", a
  language edition or an ordering note hold none; "narrated by" after a spaced dash
  or a colon). Editions are read from the name
  because the community API links no series to a recording. A name that only
  extends another ("Discworld: Tiffany Aching") is a series of its own. A match
  candidate's `series` is the work's series every recording is in, and each
  recording's additive `series` the book's list as that recording, its own edition
  first (`workSeries.of`), which `matchrun.Propose` and the match dialog's
  `communityValues` read (so the console has no copy of the rule); series carry an
  additive `id`. The facts come from `series/{id}`, each series once per request,
  concurrently through `workSem`, at most `maxSeriesFacts` per match within
  `seriesFactsTimeout` (none for a work they can't rule on: no language, one
  series), cached in the `s:` key space (quota `maxSeriesEntries`); an unreadable
  record drops nothing, and a repick (`MatchQuery.NoSeries`) reads none. Rails take
  the rules too (`railMains`, before the `maxSeriesRails` cap): an edition named for
  a kind of recording only for the recording the lookup named (or the work's only
  one; never `pickRecording`'s first-recording fallback), then first (the book's main
  series, as a match makes it), so the cap goes to the book's series (a Discworld novel in six series kept its Tiffany
  Aching rail); other editions keep their place (pre-v7 rails unchanged); a series
  the cache already knows is foreign is left out before the cap, and a main view
  whose fetched record is foreign once read, handing its slot to the next family (at
  most `maxForeignRails` times; a translation judged with every record read).
  **Persistent meta cache** (`meta_cache`, migration 0024): `meta.Store` is a
  SQLite second level behind the memory cache (`catalog/metacache.go`, adapted to
  `meta.Store` by `api.metaStore`, which logs failures; best effort, a failure
  never fails a lookup). Same keys and TTLs; persisted: enrichments (positive,
  not-found, incomplete) and positive works only - never transport errors, never
  work-id misses. A fresh row warms memory for its remaining TTL; a positive row
  however stale is served when the upstream fails (not on caller cancellation),
  held in memory for errorTTL. Rows carry `storeVersion` and the metaserve
  `source`; others are ignored. Derived, rebuildable, nobody's durable state (its writes
  take the user-state path all the same, as no content cache reads it: see Content
  generation); the launcher's retention keeps the newest `catalog.MetaCacheRows` (20 000), works
  (`w:`, caller-chosen ids) within a share of their own (`catalog.MetaCacheWorkRows`,
  2 000) so they never push the books' enrichments out. A work's later 404 (remote mode; never in
  mirror mode, see Mirror mode)
  replaces its stored row (no new row for an unknown id). Writes are bounded by
  `storeWriteTimeout` (250 ms): a busy writer costs the row, not the response. No config
  key: only the service reads or writes it, so `metadata.enabled` off touches nothing.
  **Mirror mode** (`metadata.mode: mirror`, env `AUDIOSILO_METADATA_MODE`, `remote` the default; read
  at start, a `restart` setting): the server keeps a local copy of metaserve's own database and
  answers the SAME questions in-process with metaserve's own code, so no book is looked up over the
  internet. The copy is audiosilo-meta's data release artifact (`meta.sqlite.gz`, ~450 MB, ~1.8 GB
  decompressed; the CC0 core and the CC BY-SA community layer in one file), and the module
  `github.com/kodestar/audiosilo-meta` is a dependency: `pkg/release` (the ONE implementation of
  the release asset contract: newest non-draft release carrying the asset, allowlisted GitHub hosts,
  stall watchdog, gz digest verified while streaming, temp file + rename) and `pkg/query` (`Open`,
  `MaxSchemaVersion`, and `NewHandler`: metaserve's JSON API handler, byte for byte). **Parity by
  construction**: `meta.Service.SetMirror` swaps the client's transport for `fallbackTransport`
  (`internal/meta/mirror.go`), which serves each request the client already builds through the local
  handler (`meta.Mirror.Handler()`; an in-memory ResponseWriter, `localTimeout` 15 s,
  `maxLocalBody`) while the copy is `Ready`; no copy, or a failed answer (a 5xx, e.g. a query
  this code can't run on an artifact schema newer than `query.MaxSchemaVersion`; an answer too
  large; a timeout; a panic), sends the request to `base_url` unchanged through remote mode's own
  `*http.Client` (its `clientTimeout` covers the body); a failed answer is logged there, at most
  every 10 min (`SetMirror(m, log)`). A 404 is the copy's answer; a retired slug's 301 is followed
  on the same host, so it resolves locally. A `base_url` with a path is stripped before the copy's
  handler and put back on a root-relative `Location`; a caller gone mid-answer is not a fallback. Everything above the client (compose, rails, match, the
  caches) is unchanged and `meta_cache` rows keep `Source = base_url` (switching modes keeps the
  cache warm). Two rules differ in mirror mode: the COPY's "no match" never replaces a stored POSITIVE
  row in `Enrich` or `Work` (`keepStored`: the stored answer is served stale, held in memory for
  errorTTL, the row untouched; a lagging or broken copy must not blank a companion that worked). It
  asks whether the copy answered, not whether one is ready: `fallbackTransport` marks the responses
  it builds with the internal `X-Audiosilo-Mirror` header (`localAnswerHeader`; stripped from remote
  answers, never sent to any client) and `getJSON` turns such a 404 into `errLocalNotFound`, which
  wraps `ErrNotFound` (so `errors.Is(err, ErrNotFound)` holds everywhere else). The remote service's
  "no match" (no ready copy, or a request the copy failed) replaces the row as in remote mode.
  `Ping` is unchanged: its `/healthz` goes through the same client, so the copy answers it once
  ready and the remote service until then (parity by construction; cached a minute as before). The
  runner is `internal/metamirror.Mirror` (`New(dir, Options{Enabled, Now, FreeBytes, Logger,
  SiteURL, Repo, Release})` - it builds the release client itself, adding
  `release.WithBaseSize` - then `Run`, `CheckNow`, `Wake`, `Current`, `Ready`, `Handler`, `Status`,
  `Close`; `Remove(dataDir)`): `<data>/meta-mirror/` (0700; not in backups, which hold the
  database only) holds exactly one `meta-<tag>.sqlite` (0600) and `state.json` (`jsonfile`: tag,
  built_at, schema_version, size_bytes, downloaded_at - the one record of the copy's facts,
  Status reads them there -, etag, checked_at, last_error, and published_at + sha256 as on-disk
  diagnostics only). `New` is cheap (state + folder): `Run` opens the copy first (`query.Open`'s
  integrity checks take seconds over 1.8 GB, which must not hold up the listener), so until then
  the status says `opening` with `fallback` and lookups go remote (a finished download is
  `opening` too while `query.Open` runs, before the swap: over a working copy that copy still
  answers, so no `fallback`). Schedule: durable, at most once a
  day from the `checked_at` on disk (a restart does not re-download), 30 s after start without a
  copy, an hour after a failed check, and on `CheckNow` (which makes `next_check_at` now; a running
  check shows none until it ends); never while `metadata.enabled` is off (a check in flight, its download
  included, stops when it is turned off and records nothing: `checkWhileEnabled`, and a Check now it was
  answering stays due; while off `Run`
  looks again each minute, and the settings PATCH that turns metadata back on calls `Wake`, which
  re-reads the schedule at once but makes nothing due: only a due check runs). The release-list request is conditional (the ETag is sent only while a
  copy is held, and dropped after any failure following a 200, so a 304 can't hide a release that
  failed). `update` is list then `install` (tag check, disk guard, download, open, swap) with one
  ETag reset on its error. A tag that can't name a file is refused (`validTag`); the disk guard
  needs max(4.5 x the gz size, the current copy) + 512 MiB free (`not enough disk space: need X,
  have Y`, decimal units as the console's `formatBytes`); the download goes through `release.DownloadData` (temp `.meta-*.tmp` in the folder,
  `release.WithBaseSize` = the current copy's size), is opened with `query.Open` (integrity checks;
  a file that doesn't open is deleted), then swapped in atomically; the replaced copy is closed and
  deleted after a 60 s grace (a release swapped back in within the grace reuses its name: that
  install closes the retiring handle at once without deleting, and no retire deletes the path an
  install is writing or the current copy's). Any failure keeps the current copy. At its start `Run` deletes temp
  files, a half-written state and any copy but the current one, and forgets a copy that is missing,
  changed size or doesn't open. A copy newer than `MaxSchemaVersion` still opens and answers (flagged
  `schema_newer`); artifact schema changes must stay additive (CROSS-REPO §17). The launcher
  (`pkg/launcher/metamirror.go`) builds it only in mirror mode with a metadata service (release
  client: `release.DefaultRepo`, `User-Agent: AudioSilo/<version>`; tests point it at a fake
  GitHub through the unexported `Options.metaReleases`), builds it right after `api.New` and hands
  it over with `API.SetMetaMirror` (which calls `meta.Service.SetMirror`) BEFORE the background
  jobs start, and a server started in remote mode deletes the folder.
  Covers are unchanged (their own hosts in both modes). Admin wire below (Server settings).
  **Bundle** (`meta_bundle` capability): `?include=previous` adds `previous`
  (`meta.PreviousWorkIDs` / `Service.Previous`: main-view works before this one
  (an omnibus "1-3" only when it ends before this one starts), nearest first, max 5, failures left out) and `?spoilers=hide` gates the current
  work by the CALLER's saved progress (`meta.ChapterAt` + `meta.HideSpoilers`,
  mirroring the player's `meta-gating.ts`; previous works lose only
  `recap_summary.ending`). Both work on a per-request copy - the cached envelope
  is shared and immutable. `MetaWork` also carries `community_description`
  (CC BY-SA, apart from `description`) and `attribution` (present iff the work's
  COMMUNITY content has characters/recaps/recap_summary/community_description:
  `carriesCommunityContent`; generated content, laid in after it by
  `meta.Answer`, never claims the credit); `MetaRecording`
  carries `chapter_count` and, on a LibriVox recording only (`librivox_id > 0`), `librivox`
  `{id, url, zip_url}` (`librivoxOf`: metaserve's `links.librivox`/`links.zip`, each kept only
  as an absolute http(s) URL, never built from the id; LibriVox L5).
  `Service.CachedWorkID` answers an identifier's work id
  from the cache alone: memory, else its enrichment's stored row, fresh or stale
  (never the upstream), so placement does not flip with a restart or an eviction.
  **Owned entries (`local`)**: every rail entry, main view and each
  `orderings[].works[]`, carries `local` `{library_id, path}` when the CALLER owns
  that work. Resolved per request AFTER the cache, on a copy of the rails (never
  stored: the cached envelope is shared): `api.localRails` fetches the caller's
  candidates (`catalog.SeriesBooks` over their `UserScopes`, scope-filtered: books
  whose series - the main one (`books.series_key`), or one their `more_series` names
  (`seriesMembers`, over `book_more_series`, migration 0041) - folds to a
  rail/ordering name by `match.SeriesKey`, the console's `seriesKey` - NFKD, case,
  punctuation and spacing ignored; a book in several series is a candidate in each,
  at its position there) and `meta.PlaceLocal`
  places them: the current work's entry -> the requested book; else the book whose
  work id `CachedWorkID` knows; else (orderings) the main view's book for that work;
  else the series position == the entry's numeric position on a series named like that
  view (a range entry "1-3" holds a book at that range; an entry at one number a book at it,
  else a book whose range starts there; `parseSpan`). A book with a known work id is never placed by index; one book per entry;
  ties: the requested book's library, then library sort order, then path.
  **Next book** (`next_book` capability): `GET /libraries/{id}/next?path=`
  (`handlers_next.go`, `authorizedScope` + `bookForPath` like `item`) answers
  `{source, next?, book?, work?}`; `source` names the step that produced `next`
  (or decided there is none): `community` (metadata on + matched + a rail:
  `meta.NextRail` reads, off the shared envelope, the rail that decides: in the
  book's own order (`railOrder`: the rail named, by `match.SeriesKey`, like its main
  series or one of that rail's orderings, then like its more_series entries in list
  order, then the rest as listed) the first with an entry after the current work
  that doesn't step back decides, with its first such entry (`nextEntry`: on its
  MAIN view, the smallest numeric position (a range "1-3" by its start) above the current
  work's last (a range's end), ties in rail order but a single position before a range
  starting there, passing over each entry that steps back,
  `railStepsBack`: the entry's work at or before the current work's position on an
  earlier-ranked rail, both numeric - after The Silver Chair the publication order
  passes over The Horse and His Boy and The Magician's Nephew, earlier
  chronologically, and decides with The Last Battle); `NextRail` returns the rail
  and entry indexes, and `communityNext` reads that same entry off the placed rails
  by them (`PlaceLocal` keeps every rail and entry at its index; the placed rail's
  plain next entry could be a passed-over one); `localRails` runs once, only when a rail
  decides; its entry placed -> next + book + work. A later rail's placed entry never answers
  instead: an unplaced deciding entry rides along as `work` without `local` beside
  the steps below, since failing to place (untagged, series named unlike the rail)
  proves nothing; current work last on every rail (they can lag the library),
  upstream error/unmatched/no rails/unnumbered -> no `work`), `series`
  (`catalog.NextInSeries`: every series of `Book.AllSeries()` with a position, main
  first; per series one query
  (`laterSeriesMembers`) over its numbered members in scope - same library, books in
  that series (any spelling: by its key, see Series spellings) by their main series or a more_series entry, at their
  position IN THAT SERIES - those above the book's position (a range's end) first, then by
  position, then a range's end (a single book before an omnibus starting with it), ties by path (unbounded: SQLite sorts every member first anyway):
  read in that order, the first row above that doesn't step back is the next book,
  and any row at all means numbered (the first at or below ends the walk); a UNION
  ALL of an `idx_books_series_key` branch and a `book_more_series` branch (its key
  index, then each book by rowid); the first series with a later book that doesn't step back
  answers with the first such book (`stepsBack`: it sits at or before the current
  book (its last position: a book inside an omnibus steps back) in an earlier-ranked series of the current book's, both numbered there; a
  book that steps back is passed over, not the whole series); numbered books in
  some series but none later (or every later one stepping back) in any ->
  `{source:"series"}`), `folder`
  (`library.NextSibling` over the parent's whole listing, `ListDir`, scope- and
  ignore-filtered, annotated by `BooksByPaths`, which reads any number of paths in
  chunks, and by `markPendingNext`: the folders after the book and before the next
  indexed one that a scan would make books of, `pending_book` as the listing marks
  them (server#211), read in name order 16 at a time up to the batch holding one,
  within 2 s, best-effort (a failed index read is logged, the step answers from the
  index alone); the player's `findNextSibling`: names compared as its `localeCompare`
  (numeric, base) does, by `x/text/collate`, a pending folder as a book (offered with no
  `book`), and a bare folder only when nothing in the folder, the current book
  included, is indexed), else `{source:"none"}`. A numbered series' end gives way to a pending
  folder after the book (`folderNext`'s `pendingOnly`: `{source:"folder"}` with no `book`) in the
  series' own folder only (`seriesFolder`: its other indexed books share a series with the book,
  `catalog.SharesSeries`, and there is at least one), so a numbered series no scan has reached is
  walked book by book (server#254); an indexed book after it, or a new folder in an author's folder
  of other books, never does, and with nothing pending the end answers `{source:"series"}`. A
  failure to place the caller's books leaves the next entry unplaced (as `/meta`
  degrades). `book` is the list shape; everything `next` names is in the caller's
  scope. In a podcast library (`episodeNext`) community and series are skipped: `folder` is the
  show's next episode oldest first in the episode order (`catalog.NextPodcastEpisode`: in scope, in
  the feed or kept on disk, never by file name), else `none`.
- **Upcoming releases (Track U1 and U3, capabilities `upcoming`, `upcoming_calendar`, `upcoming_covers`,
  `internal/upcoming`, `catalog/upcoming.go`, `api/handlers_upcoming.go`, `api/handlers_upcoming_calendar.go`,
  `auth/feed.go`, `0059`, `0060`)**: a background job works out, for the series a server's books
  libraries hold, the community's preorders, recent releases and missing volumes, and `GET /me/upcoming`
  (requireAuth, notKid) reads what it stored for one caller. **Layout**: `upcoming/classify.go` is the pure
  `Classify` (slots by the console's `railEntries` rule; a book with a known work holds that work's span,
  else its own position or range; kinds in order preorder (future at the date's own precision,
  `releaseIsFuture`), released (within `upcoming.window_days`, counted from the date's first day), missing
  (a whole-number interior gap below the highest held, plus the first whole non-preorder slot after it;
  never a fractional position); a work in two series is the first's); `runner.go` is the job; `list.go` the
  answer (`List`: a work the caller holds a visible copy of anywhere is dropped, one row per work, a
  preorder whose date has passed reads as released; preorders soonest first, releases newest first, then
  missing by series name and position; `status` `ok`, `pending` before any success, `stale` after 48 h
  without one). **The job**: the first run an hour after the start (or the last success plus a day), then
  daily with +-1 h jitter, a failed run again hourly three times; `Kick` (wired to `OnBookChange`) makes a
  light run once kicks have been quiet 30 s (at most 10 min: books resolved, series never read are read,
  no feed, no status); `Wake` (PATCH /admin/settings switching the job or metadata on) runs at once when
  no run has succeeded within the day. A full run: (1) each book with an ASIN/ISBN gets its work id (the
  meta cache, else `WorkIDs` in batches of 100; answers rechecked monthly, at most 500 a run); (2) each
  local series (library, `match.SeriesKey`) gets its community series from its works' series named like it
  (primaries first; then give or take an article or "Series"), monthly or when it gains a book or a work;
  (3) metaserve's watch feed, 50 series a request (split when full), says which series have news; (4)
  `series/{id}` is read when new, with news, or a week old (2 at a time; a 404 marks it gone, dropped after
  a week); (5) classify; (6) announce. It makes no call while `upcomingOn()` is false (metadata on AND
  `upcoming.enabled`, live), and a run in flight stops within a second when switched off (`whileOn`,
  `errSwitchedOff`: a cancellation, no status recorded, no retry). In mirror mode every call goes to the
  local copy. A failure keeps what earlier runs stored. **Storage** (all derived and rebuildable,
  path-keyed, no FK to `books`, cascaded with the library, written through the user-state path; no
  content cache reads them): `upcoming_owned` (book -> work id and the identifiers it was resolved by),
  `upcoming_series` (local series -> community series ids, `''` for none found, `classified_at`),
  `meta_series` (a community series' members as last read, `ok`/`gone`), `upcoming_items` (one row per
  work a local series lacks, its kind, title, authors and cover, `first_seen_at`, `announced_at`) and
  `upcoming_announced` (server-wide, the works `book_released` named, kept 400 days); the run's status is
  `server_state('upcoming')` (`metadata.upcoming_status` in the settings envelope, read only).
  **Silent baseline**: a released item is announced only when a community series the local series was
  classified against BEFORE lists its work; anything a series' first classification finds (a new server,
  a new library, a series newly resolved) is marked dealt with, never announced. **`upcoming_announced`**
  makes an announcement once per work across series and across an item leaving and coming back (a copy
  added then removed); a work the server holds a copy of anywhere is marked without a word. The mark is
  written even when the run is being stopped (`context.WithoutCancel`). **`book_released`** is one event
  per run (`api.announceReleases`: up to 5 titles as "Title (Series #position)", `upcoming.Release.Label`,
  the count of all, dedupe key a digest of the run's work ids). Config `upcoming.enabled` (live,
  `AUDIOSILO_UPCOMING_ENABLED`, console Settings > Metadata, setting id `metadata.upcoming`) and `upcoming.window_days` (1-365, default
  90, restart, `AUDIOSILO_UPCOMING_WINDOW_DAYS`; 0 or anything outside refuses to load). The route is 404
  `metadata_off`, then 404 `upcoming_off`. `MetaSeriesWork.release_date` (additive, omitted when unknown)
  feeds the console's dated gaps on the Series cards. **The calendar feed (U3, security-critical)**: `GET
  /me/upcoming/calendar` -> `{"active", "created_at"?}` (requireAuth, notKid; an API key may read it; the
  link itself is never answered again); `POST` -> `201 {"path": "/api/v1/me/upcoming/releases.ics?token=<secret>",
  "created_at"}` (`Cache-Control: no-store`; a path, not a URL: the player prefixes the address it reaches
  the server at, since the server knows no reliable public one; `created_at` is fixed-width UTC milliseconds,
  `2006-01-02T15:04:05.000Z` like `generated_at`, stored so on the row and always later than the link it
  replaced, even two resets in one millisecond, so a device can tell its copy is stale); `DELETE` -> 204,
  idempotent. Mint and delete are session only (`denyAPIKey`, plain 403: a key never mints a credential). The mint's refusals in
  order: 401, 403 `kid_account` (notKid, and `refuseKid` inside the write), 403 `demo_account` (notDemo),
  403 for an API key, 404 `metadata_off`, 404 `upcoming_off`; the status read has the same order without
  the two credential refusals; DELETE has no 404s (ending a link only takes access away). The secret is a
  `tokens` row of kind `feed` (`auth.KindFeed`, `IssueFeedToken(user, session)`: `generateToken`, SHA-256
  hash only, no expiry, device name "Calendar feed", and the `created_on_token_id` of the session that
  made it: a member signed in on their owner's device makes a link "on" that device). **One per user**: a
  mint deletes the earlier row in the same durable transaction, and `0060`'s partial unique index
  `tokens(user_id) WHERE kind='feed' AND revoked=0` refuses a second live row whatever writes it. A feed
  row is DELETED, never flagged revoked, when it ends: a reset, Turn off (`RevokeFeedToken`), the switch to
  a kid (`clearKidCredentials`, both the owner's member edit and the admin's assign), a move into another
  household when it was made on another owner's device (`leaveOwners`, with the sign-in made there; made
  independent, it stays, as that sign-in does); the account's deletion cascades; a disabled account's is
  refused by `lookupToken`. A password change or reset, and the admin's sign-out everywhere, leave
  it (server#281, Password ends sessions above): the feed is the person's to end. It is never touched
  (`resolve`, like a bridge token: a calendar's polls are not the person's activity, and no last_ip
  is kept), never a device (the device lists take session and API-key
  kinds only) and is held in the token cache like a session (every end above is a content write). **Acceptance**: `GET
  /me/upcoming/releases.ics` is the ONLY route listing `KindFeed` (`requireFeedAuth` = `mediaAuth` with
  `feedKinds`: `?token=` or the header, the media rate limit per credential) and it lists no other kind:
  a session, API key, cast, bridge or pairing token is 401 there, and a feed token is 401 on every other
  route (the route-table test `TestFeedTokenRefusedOnEveryOtherRoute`, and `TestFeedKindNamedOnce` for
  routes outside api.go's table: the package names `auth.KindFeed` only in `feedKinds`, uses `feedKinds`
  only in `requireFeedAuth` and wraps one route in it). Behind it, notKid (403 for a leftover token of a
  person now a kid), then the two 404s. **The calendar** (`upcoming/ics.go`, `ICS`, pure, golden-tested):
  the token owner's `/me/upcoming` list (`myUpcoming`, the JSON route's scopes and rules read at the
  request, so a share taken away is felt at once), RFC 5545 with CRLF, lines folded at 75 octets never
  inside a UTF-8 sequence, TEXT escaped (`\\ \; \, \n`; CRLF, CR and LF are each a line break; other
  control characters and invalid UTF-8 dropped), a URI with a control character or space left out.
  VCALENDAR: VERSION 2.0, PRODID `-//AudioSilo//Upcoming releases//EN`, CALSCALE GREGORIAN, no METHOD (a
  subscription needs none; with one, RFC 5545 asks the Content-Type for a matching `method` and iTIP's
  PUBLISH an ORGANIZER), `X-WR-CALNAME` "AudioSilo: upcoming releases" (+ " (<name>)" when the server has
  one), `REFRESH-INTERVAL;VALUE=DURATION:PT12H`, `X-PUBLISHED-TTL:PT12H`. One VEVENT per `preorder` or
  `released` item with a `YYYY-MM-DD` date, read as `parseReleaseDate` reads it, whose next day is still a
  four-digit year (a missing volume, a year or month date, no date or a bad one: none): all-day
  `DTSTART;VALUE=DATE` and `DTEND` the next day, `UID:<work_id>@<server_id>` (`config.ServerID`,
  `audiosilo` when unset; a slipped date moves the event), `DTSTAMP` the list's `generated_at` (else now,
  UTC), `SUMMARY` "Title (Series #position)" (`Release.Label`, the bell's), `DESCRIPTION` "By authors",
  "First release: <date>" and the `web_url`, `URL` the `web_url`, `TRANSP:TRANSPARENT`. A server that
  never ran the job, or a list with no such item, answers a calendar with no component (RFC 5545 asks for
  one; calendar apps read it as empty). Headers: `text/calendar; charset=utf-8`, `Content-Disposition:
  inline; filename="releases.ics"`, `Cache-Control: private, max-age=3600`, `ETag` `"ics-<the first 16
  hex of the body's SHA-256>"` (a poll that has it gets a 304; `conditional`); never compressed
  (`compressExcluded` holds `/api/v1/me/upcoming/`). The upcoming reads take the caller's scopes without
  podcast libraries (`upcomingScopes`: the tables join books libraries only), and a read the client gave
  up on is answered without a log line (`writeUpcomingError`). **The Upcoming cover** (`GET
  /me/upcoming/cover?work=&size=`, requireMediaAuth: session or API key, header or `?token=`, never a cast
  or feed token; notKid; then the two 404s): 400 without `work` or with a size outside `thumbSizes`; it
  serves only the `cover_url` the caller's own list hands out for that work right now (`upcoming.CoverFor`:
  `List`'s rule, the first library's entry unless a copy is in scope, read for the one work through
  `catalog.UpcomingWork`; DB only, no meta traffic): a work not in it (another person's series, one the
  caller holds, none) or in it without a cover is 404 `not_found`, before any fetch. Then `/meta/cover`'s tail
  (`serveCommunityCover`: the ETag with `private, no-cache` so a stable work URL revalidates when its cover
  changes, `communityThumbnail`'s guarded fetch, single flight and
  thumbnail cache, 502 `cover_unavailable`, 404 `no cover`). **Flags**: `upcoming_calendar` and
  `upcoming_covers` are exactly `upcoming` (false, never absent, while off); kids and demo accounts are
  refused by the routes, not the flags.
- **Unknown ASIN report (LibraForge C3, `internal/coverage`, `catalog/coverage.go`, `meta/report.go`, `0068`)**:
  an opt-in daily job sends the metadata service the Audible ASINs of this server's books the community
  database doesn't know, so its lane can import them. **Setting** `metadata.report_unknown` (default false,
  live, `AUDIOSILO_METADATA_REPORT_UNKNOWN` locks it; console Settings > Metadata "Help complete the community
  database", disabled while metadata is off); the job runs only while `reportUnknownOn()` (metadata on AND the
  switch, never demo) and a run in flight stops within a second when it goes off (`whileOn`, `errSwitchedOff`: no
  status). It is never built on a demo server (`cfg.Demo.Enabled` at start), so a demo makes no call. **Schedule**:
  the first run an hour after the start (or a day after the last run), then daily +-1 h; a run within 23 h of the
  last is skipped (`server_state('unknown_asin_report')`, so a restart never re-runs); `Wake` (PATCH switching it
  or metadata on) runs at once unless one ran within 23 h. Mirror mode: no run until the local copy is `Ready`
  (rechecked hourly), and the lookups stay local in mirror mode: `KnownASIN` marks them `localOnly`, so
  `fallbackTransport` answers them from the copy or fails them (`errLocalOnly`: the copy failed one, or is no
  longer `Ready`), never asking the remote service, unlike every other mirror-mode request; mirror mode skips the
  Ping (a `/healthz` the copy fails would go remote), so only the POST goes remote. **Candidates**
  (`UnknownASINCandidates`, books libraries only, upper-cased, `match.ValidASIN`): `books.asin` except an ASIN a community match gave a book
  (`book_overrides` asin `source='community'`), plus `tag_asin` only for a book with no ASIN or ISBN, no asin
  override (a cleared one too) and a tag on exactly one book. **A run**: Ping, remote mode only (fails: `failed`, tried the next day);
  drop rows no candidate holds; look up (`meta.Service.KnownASIN`, local-only in mirror mode; a cached hit,
  memory or the persistent store (`CachedWorkID`), counts as known) the candidates never looked up or looked up 30+ days ago, shuffled, at most 500; only
  `ErrNotFound` is unknown, any other error stops the lookups (the rest wait; what was confirmed may still be
  sent); then the unknown ones checked within 30 days and not sent within 30 days, shuffled, 200 a POST, at most 5
  POSTs, and none while `MAX(sent_at)` is under 23 h old. **The POST** (`ReportUnknownASINs`, the `report` client
  built in `NewService`, never behind `SetMirror`, so mirror mode still reports remotely): `POST
  <base_url>/api/v1/intake/unknown-asins`, body exactly `{"asins":[...]}`, headers only `Content-Type:
  application/json` and `User-Agent: AudioSilo` (plus Go's `Accept-Encoding`/`Content-Length`/`Host`): no
  `X-AudioSilo-Client`, server id, auth or version (`TestReportUnknownASINsWire`). Only https, or plain http to a
  loopback host (`CanReport`); any other base_url is `not_offered` with no call at all and one INFO line a process.
  Redirects are never followed (`http.ErrUseLastResponse`). **Answers**: a 202 with `accepted` marks that chunk's
  `sent_at` (even mid-stop, `WithoutCancel`); 404, 405, a 3xx or a 2xx without `accepted` is `not_offered` (one
  INFO line, no call for 7 days: `quiet_until`); 429, 5xx or anything else marks nothing (`failed`, next day). An
  `unknown` row a recheck finds known forgets its `sent_at`. **Storage**: `unknown_asin_reports(asin PK, checked_at,
  unknown, sent_at)`, no path or book (disposable; user-state writes). **Wire**: GET /admin/settings
  `metadata.report_unknown` and read-only `metadata.report_status` = `{"state":"never|ok|not_offered|failed",
  "last_run_at"?, "last_sent"}` (absent without the job: no service, or a demo server). The mounted Metadata
  topic polls status every 30 s while metadata and reporting are on. Trigger by hand: switch it
  off and on in the console (runs when none ran within 23 h; an active 7-day quiet period still applies,
  measured from the last attempt, and toggling never resets it); to run again sooner, delete
  `server_state('unknown_asin_report')` and the `unknown_asin_reports` rows, then switch it off and on.
- **Native deep-link association**: `GET /.well-known/apple-app-site-association`
  and `/assetlinks.json` are served from `config.AppLinkConfig` (`app_links` in
  YAML) and 404 when unset. They only enable auto-app-launch for domains the
  shipped app build claims - self-hosted arbitrary domains fall back to the web
  player + the custom-scheme "Open in app" button.
- **SQLite** runs with a single write connection (writers serialize) plus a
  read-only reader pool, WAL mode. `store.DB.QueryContext`/`QueryRowContext` go to
  the READER (`query_only`): any statement that writes, including `INSERT ...
  RETURNING`, must use `ExecContext` or a `WithTx` transaction, or their user-state
  forms `ExecUserState` / `WithUserStateTx` (there is no other way to the writer:
  `store.DB.ContentGeneration`, which the list cache, the token cache and the library
  cache key on, moves after each content write finishes (a user-state one only when it
  changed more than its tables' rules let through), so a write that bypassed them
  would leave a revoked token honoured; see Content generation). An in-memory DB has
  reader == writer and would hide a misroute, which is why the test stores are
  temp files. Commits are `synchronous=NORMAL` (a power cut can roll back the last
  few); a write that ends a credential (sign-out, device or API key revoke, password
  set or cleared, account disabled/demoted/deleted, auth code revoked, rotated,
  replaced or used, a pairing token used, Bridge pair code issued, pair/unpair), a write
  that takes share access away (a grant revoked, a path rule removed, a share deleted or made
  read-only, a library deleted, a collection's viewer removed, a book taken off a collection
  (a collections-access member's reach) or the collection deleted:
  `catalog.RevokeShare`, `RemoveSharePath`, `DeleteShare`, `UpdateShare`'s `read_only`,
  `DeleteLibrary`, `SetCollectionShares`, `RemoveCollectionItem`, `SetCollectionItems`,
  `DeleteCollection`; S260-REVOKE, server#260)
  or a Bridge claim that leases a job uses `WithDurableTx`/`ExecDurable` (FULL for
  that commit, about one fsync), and a new one of those kinds must too; an account edit
  that ends nothing (`UpdateUser` with only `can_add_members` or re-enabling, `UpdateMember` a
  rename) and a claim that only ends a lapsed lease or a lost request (`claimTx` without a job)
  are ordinary commits; progress,
  heartbeats and other listening writes never do. The readers (never the writer) map the file
  (`mmap_size` 256 MiB, `store/mmap.go`) when `database.mmap` (`AUDIOSILO_DB_MMAP`, any case) is
  `on`, or `auto` (the default) and the database file is on Linux on Unraid's shfs: statfs says
  FUSE (`0x65735546`), then the `/proc/self/mountinfo` line for the file's device (stat's
  `st_dev` against the `major:minor` field, no path matching) is `fuse.shfs`
  (`mappableFSTypes`); any failure, or no line for the device, says off. Measured
  2-9x faster on Unraid `/mnt/user`, little on a local disk; never on other FUSE filesystems (mergerfs: direct_io; sshfs,
  rclone, s3fs, ntfs-3g) or NFS/SMB, where an I/O error under a mapping is a SIGBUS crash. The
  start logs which (`database readers: mmap`).
  Nothing may truncate the database file in place while it is open (backups are `VACUUM INTO`).
  The library is the build's **engine** (`store/engine*.go`): modernc's, pure Go, by default;
  with `CGO_ENABLED=1 -tags "sqlite_native sqlite_fts5 sqlite_preupdate_hook"` the native C
  SQLite (mattn/go-sqlite3's amalgamation, compiled in), where `AUDIOSILO_SQLITE=modernc` picks
  modernc's for the process (A/B on one binary). A native build missing cgo or either mattn tag
  doesn't compile (`engine_native_guard.go`: without `sqlite_preupdate_hook` mattn's pre-update
  hook is a silent no-op, and the user-state watch with it). Every open of the database goes
  through the engine's driver (`driverName()`: Open's pools, `VacuumFile`, `openReadOnly`), never
  `sql.Open("sqlite", ...)`: two SQLite copies on one file in one process don't see each other's
  POSIX locks. What else is a library's goes through the engine: the pre-update and commit hooks,
  SQL functions (`store.RegisterFunction`, from an `init`), `IsUniqueViolation`. The start logs
  which (`database: sqlite engine=... version=...`). SQL holding several statements takes no
  parameters: modernc gives each statement the whole argument list, mattn hands them out statement
  by statement (`not enough args to execute query`); run one statement an Exec. A statement is
  handed only the numbered arguments it has (`argsFor`: one list may serve several statements, as
  in `carryListeningState`), and a connection that runs DDL drops its cached statements (mattn
  reads a query's columns before SQLite re-prepares it for a new schema). On the native engine a
  cached query is read a row a C call (`engine_native_rows.go`), its values as modernc's (int64,
  float64, string, []byte, nil; no time or bool conversion by declared type): mattn's own rows
  cross into C two or three times a column. As modernc's, its first step runs in the query (its
  columns are then the statement's as SQLite re-prepared it after another connection's DDL, and a
  failure is the query's error), and its context's interrupt never outlives its rows (a late
  `sqlite3_interrupt` would end the connection's next statement, another request's). It reads mattn's unexported `SQLiteStmt.s`;
  `TestNativeFastRows` fails if a mattn upgrade drops it (queries would quietly fall back).
  Both pools open the engine's library behind a per-connection prepared-statement cache
  (`store/stmtcache.go`): a statement's SQL text is its key, so SQL
  built per call (a value formatted into the text, an `IN` list sized to its arguments) misses
  and churns the cache (256 statements per connection, least recently used out); bind values as
  parameters instead. SQL holding several statements, and a statement whose rows are still open,
  take the library's own path, and on mattn so does an Exec of anything but plain DML (its Exec
  steps a statement once: one that returns rows would be left mid-run); after a cached Exec the
  native engine resets the statement and clears its bindings, as modernc does, so no copy of the
  last arguments (a cover's bytes) stays held. SQL functions take their arity on both engines. A
  `conn.Raw` gets the cache's connection: the engine's hooks unwrap it; anything else of the
  library's connection is `Unwrap()`'s. A hot join (one run per book or per row: the
  scan's, `catalog/communitychapters.go`, `catalog/overrides.go`) takes
  its parameters as `+?` (`+?1`): SQLite 3.54's estimate of a join loop's rows
  (`whereLoopOutputAdjust`) reads a bare `?`'s bound value, which marks the statement to be
  re-parsed whenever a new value is bound, so every run (the scan's per-book joins: 40-80 µs a
  run, 5 µs as `+?`). A unary plus on the value keeps the plan (an index still serves `col = +?`)
  and the comparison (a parameter has no affinity or collation either way); it only hides the
  value from the planner, which a statement whose plan must not follow its values wants anyway.
  A single-table statement and a correlated subquery are not affected.
- **JSON compression** (`api/compress.go`, PERF-SPIKE S5): `compress` wraps the mux, inside the
  timeout. A request is compressible only when it is a GET (or its HEAD, which so carries the GET's
  `Vary` and `Content-Encoding`, server#270) under `/api/v1/` without Range, outside
  the media (`isStreamingPath`, `/meta/cover`), `/api/v1/auth/`, `/api/v1/bridge/` and `/api/v1/me/upcoming/` (the calendar feed, its token in the URL, and the Upcoming covers)
  (`compressExcluded`); every credential is minted by a POST, so no compressed body holds one
  (BREACH). A new GET that returns a credential joins `compressExcluded`. A new stream (media, a
  download, a long poll, anything that flushes) goes in `isStreamingPath` when it must also leave
  the request timeout, else in `compressExcluded`: `gzipWriter` has no Flush and holds the first
  1 KiB back, so a streamed JSON answer must never reach it. Either way it adds a
  `TestCompressExclusions` row (GET and HEAD) proving it is answered identity. The answer is gzipped (BestSpeed) when it is `application/json`, at least
  1 KiB, has no Content-Encoding or ETag of its own and the client takes gzip; every JSON answer
  to a compressible request carries `Vary: Accept-Encoding`. The list cache stores each answer's
  gzip beside it and sends that on a hit (`writeCachedJSON`), so a hit compresses nothing.
- **List cache** (`api/listcache.go`, server#243): `GET /libraries/{id}/books` (a
  books library), `/books/recent`, `/search` and a books library's browse lists
  (`/libraries/{id}/authors`, `/narrators`, `/series`) keep their encoded answer
  (8 MiB LRU counting each answer's buffers, its JSON and its gzip, and entry, 1 MiB an
  answer) keyed on the path,
  raw query string and the caller's scopes (spelled with `%q`/`%#v`: JSON would
  merge paths whose bytes aren't UTF-8), under the content generation read before
  the request's first read; any content write moves the generation and empties it
  (listening does not: see Content generation). Only
  answers that are a pure function of the database, the query and the scopes may
  use it (no per-listener field on a list book, and nothing a user-state rule lets
  through); a handler that adds one must leave the cache. The admin console's `GET /admin/books` first pages and
  `/admin/books/facets` use it too (keyed like a list, no scopes: an admin sees
  every book), and so does the Health summary `GET /admin/issues` (server#245),
  whose key also takes `cachedList`'s extra inputs: the kinds it counts and the
  offline libraries (a filesystem probe made before the lookup, so a root going
  away needs no write to show).
- **Content generation** (`store/userstate.go`, server#258): `store.DB.ContentGeneration`
  moves after every write (`ExecContext`, `WithTx`, `WithDurableTx`/`ExecDurable`, failed and
  rolled-back ones too, not a transaction that never began) except a user-state one,
  `ExecUserState` / `WithUserStateTx` (on `store.DB`; catalog's db interface has both), which
  moves it only when it changed something outside `userStateTables`. Default to both: a write
  is content unless it goes through the user-state path. User state here is what no content
  cache reads, not the data model's durable per-user state: `meta_cache` (derived) and
  `server_events` are listed, favourites and goals are not. The check is SQLite's pre-update
  hook on the writer's connection for the write's duration, so triggers and foreign-key actions
  count: per table, a rule says whether new and removed rows are user state (`progress`'s are
  not: Health ranks duplicate copies by how many people have progress on them) and, for
  `progress` (every column but its key), `tokens` (`last_seen`, `last_ip`, `client_*`),
  `bridge_status` (`heartbeat_at`, `status`) and `bridge_jobs` (`lease_until`, `updated_at`,
  `progress`), which columns an UPDATE may change by value (the hook reads a row only up to
  the rule's last watched column, `watchedChanged`, so a freed column after them, the
  heartbeat's up to 64 KiB `status`, is never copied; S263-HOOK). Anything else on a listed table
  moves the generation; a table the list doesn't name is a misfiling (logged; a panic in a
  test binary), and a rule's table or column a newer server's schema lacks (a rollback) only
  makes more writes content. Durability (`WithDurableTx`) is orthogonal. The user-state
  writes: `SaveProgress` (an update of the row), `RecordHeartbeat` (listening sessions),
  bookmarks and notes (add, edit, delete), `AddHistory`,
  `SetRating` / `DeleteRating`, the queue (`AddToQueue`, `SetQueue`, `RemoveFromQueue`),
  `SaveReadingProgress`, `SyncListening` (Sync v2: `listening_events`, progress, sessions, history), `RecordServerEvent`,
  the Bridge's Audible title lists (`PutAudibleTitles`: `audible_titles` rows and the account's `last_poll_at`, a
  complete poll's `refused_at` cleared and `refusal_ended_at` stamped), the scheduler's `MarkAudibleQueued` (`last_queued_at`) and
  `MarkAudibleSyncQueued` (`sync_queued_at`), an ended sync's `FoldAudibleSync` (`last_sync_at`, `refused_at`),
  `PutMetaCache`, the upcoming releases job's tables
  (`upcoming_owned`, `upcoming_series`, `meta_series`, `upcoming_items`, `upcoming_announced`: derived,
  read only by `/me/upcoming` and the job), the token touch in
  `auth.Service.resolve` (which then drops that token's cached row, so the next request reads
  its new last-seen and address; a lookup that read the row before the touch never caches it
  after: `store.GenMap.PutMarked`), the Bridge's heartbeat (`bridge.SaveHeartbeat`, whatever
  its status: with an ASR worker attached every beat's differs, `workers[].last_seen`; GET
  /server's `bridge` block reads it through `bridge.Service.LastHeartbeat`'s own cache, not the
  generation; a pairing or an unpair is content) and its job reports (`bridge.UpdateHeld`: a
  report that renews the lease or carries progress, its message and park included, stays user
  state; a new state, result or error, or another job's lease expiring, moves the
  generation). No content cache reads `bridge_status` or `bridge_jobs`. Kept as content on
  purpose: collections, grants, shares, users, libraries, books, scans, matches, covers,
  favourites, goals, household sharing, the pairing touch, retention and prunes (rare, or
  read by what is cached). There is no global "every write" counter: nothing reads one.
  Each consumer, audited against the tables its answer reads:
  the list cache's player lists (`/libraries/{id}/books`, `/books/recent`, `/search`, a books
  library's authors/narrators/series: `books` and its side tables, `libraries`; scopes computed
  per request from shares, grants and members' `collection_shares`, into the key) and the
  admin's `/admin/books` first page and facets read no user state; Health `/admin/issues` reads `progress` row counts (the duplicate ranking), hence
  `progress`'s row rule, and its offline libraries' listener counts are computed per request
  into the key; `Catalog.libs` reads `libraries` only; the token cache (`auth/tokencache.go`)
  reads `tokens` and `users` (owner join), and the touch is the only user-state change to
  either, dropped from the cache by the touching request. A new cache whose answer reads a
  user-state table needs a key of its own, or that table out of the list.
- **Pagination** is keyset/cursor-based (`catalog.ListBooks`); don't switch list
  endpoints to OFFSET for large tables.
- **Path safety**: any filesystem access derived from user input goes through
  `library.SafeJoin`, which rejects traversal outside the library root.
- **Path-addressed API**: content endpoints are `GET /libraries/{id}/{item,
  chapters,cover,stream}?path=` and `{GET,PUT} .../progress?path=` etc. The path
  is the handle (a query param, to avoid encoded-slash issues). Every one resolved through
  `pathInScope` (`authorizedPath`/`authorizedScope`/`scopedPath*`; not the browse, the list-remove, the podcast or
  the admin folder-override route, which takes its path on its own) checks it first: over `catalog.MaxPathBytes` (4096 bytes as sent) is 400 `path is too
  long`, before it is cleaned, looked up or stored (server#289). `item`/`chapters`/
  `cover`/`meta`/`next` resolve `(library, path)` to a book in ONE place, `bookForPath`:
  `GetBookByPath`, then the indexed folder book holding the path
  (`GetBookHolding`: a part, a disc folder of a joined book), then indexing on
  demand (`Scanner.IndexPathWithin`) if the scan hasn't reached it. A book found
  above the requested path must be in the caller's scope too (`library.ErrNotAllowed`,
  answered as the out-of-scope 403 `no access to this path`, nothing read or
  indexed for that caller). Concurrent on-demand indexes of one book (a book page's
  `item` and `chapters` at once) share one read (`Scanner.indexShared`, keyed by
  library and book path), each request scope-checked before it joins. The on-demand index takes a path only in its on-disk
  spelling, each name as its parent's listing has it (`resolveBook` via `dirPresent`): on a
  case-insensitive filesystem a case variant opens the same folder, and indexing it would add a
  duplicate book everyone sees, so it is a 404. `stream` serves an
  audio file path directly, or transcodes it with `?transcode=1` (see below), or with
  `?rendition=watch` serves its watch rendition whatever else the query says (see Watch renditions). The
  `/fs` view lists **audio files and directories only** (non-audio like `.jpg`/
  `.nfo` are filtered in `BrowseFS` so a click is always playable, and so is an `.mka` a book of
  the folder leaves out: `library.shadowedMKA`, shared with `folderAudio`; `ListDir`, the next
  book's listing, the same), directories first, then files, each in **natural order**
  (`naturalLess`, so a book's files list in the order they play; a joined book's discs play in
  `discOrder`, which ignores spaces, hyphens and underscores, so only a set named unevenly,
  `CD1` and `CD 2`, lists in another order), and annotates
  book entries with metadata (`is_book` + title/author/…); the client acts on the
  entry's `path`. A folder the scanner would read as a book that the index doesn't hold yet
  is `pending_book: true` (#193, `library.MarkPendingBooks`, from `annotateWithBooks`), by
  `resolveBook`'s rules (`joinRoot` first, as discovery decides a join): a folder with a `book`
  override that is a disc set (walked only then, bounded) is the joined book and its discs are
  none; otherwise a folder holding audio of its own (hidden and ignore rules applied) with no
  `collection` override is one, a disc folder below a `book` override that joins nothing
  included; never with `is_book`, never in a podcast library, for any caller but only on a folder
  their scope `Allows` (a folder seen only on the way down to a grant is none); absent otherwise,
  no capability (an old player ignores it). Cost: a folder with an indexed book at or below it
  is not read from disk (`catalog.FoldersHoldingBooks`, two index seeks a folder), so only
  unindexed folders cost a directory read; such a folder that has audio of its own too is not
  flagged until a scan indexes it (opening one of its files indexes it on demand). The pass has
  `pendingTime` (2 s, walks included) and the request's context: a folder it doesn't reach in
  time stays unmarked. The player opens such a folder as a book (indexed on demand by
  `bookForPath`). Favouriting one indexes it too (server#211, `indexFavourite`, the caller's scope
  deciding as `bookForPath`'s does), detached from the request like a rescan (the probes don't stop
  for a context, so a request's deadline would only throw the read away at its last write): the
  POST waits for it up to `favouriteIndexWait`, 5 s, under the player's 15 s request timeout, and it
  is saved when it ends.
  **Progress is the book's** (#199): `GET`/`PUT`/`PATCH .../progress?path=` key the row (and
  PUT's listening session) on the path of the indexed book the path is or names a file of
  (`api.progressPath` over `catalog.BookPathOfFile`: the book at it, else the innermost folder
  book one of whose files it is), when the caller's scope allows that book; otherwise on the
  path as given, as before (a navigation folder, a book not indexed yet, a grant reaching only a
  part, a disc folder of a joined book: a download of that disc saves there, on the disc's own
  timeline). No on-demand index on a save, and a failed lookup is a 500 (a timed-out one a 503,
  `writeCatalogError`), never a save under another key. Rows old clients already wrote
  under a part path stay where they are (not merged), and bookmarks, notes, history,
  favourites and the queue are not canonicalized (follow-ups). `POST /cast/token` already grants
  the book's path (`bookAt`).
  **The ebook** (Bridge Phase 3b, capability `ebooks`, `api/handlers_ebook.go`): `GET
  /libraries/{id}/ebook?path=<the book's own path>` serves the file the scan recorded for the book
  (`books.ebook_path`, see Auto book/folder detection), opened fresh by `library.OpenEbook`: joined
  again through `ContentJoin`, checked a regular file BEFORE it is opened (a FIFO's open would wait
  for a writer; a folder book's must not be a link now either, `Lstat`) and, once open, still the
  very file the path leads to inside the library (`os.SameFile` against a second look), so a link
  swapped in meanwhile is never followed out. Gone, or anything but such a file now
  (`library.ErrNoEbook`): 404; one that is there but can't be read right now (a permission, a flaky
  mount): 500, logged. Served with `http.ServeContent` (Range, a size+mtime
  ETag, Last-Modified), `Content-Type: application/epub+zip`, `Content-Security-Policy: sandbox;
  default-src 'none'`, `Cache-Control: private, no-cache`, the site-wide nosniff and `same-site`
  CORP. **Header auth only** (`requireAuth`: a session or API key; `?token=` is never read and a cast
  token is refused either way; there is no URL token for it, see Cast tokens). The path must be the
  book's own (`bookAt`, then `book.RelPath == path`): a track, the epub's own path, a folder above,
  a hidden or `.audiosilo/` path, a podcast library, a book with no ebook (none, several, unreadable,
  a link leading out) are 404; a book outside scope 403. Its bytes outlive the request timeout
  (`isLibraryMedia`, in `isStreamingPath`, matches `/ebook` as it does `/cover` and `/stream`), but
  its lookup (`bookAt`, which may index the book on demand and so wait for the one writer) is
  bounded by it: past it, a 503 like any timed-out lookup (`writeCatalogError`'s
  `context.DeadlineExceeded`; `TestEbookLookupBounded`). It is limited with the authenticated
  API, per credential (`requireAuth`, no media route). The player's
  `Book` carries `ebook: {path, size}` (omitempty; `catalog.Ebook`, the size from `books.ebook_size`,
  `0058`, recorded with the path: server#228, never parsed out of `ebook_sig` per row) on
  every endpoint returning a Book, so a client never asks for a book with none. **Reading
  progress**: `GET`/`PUT /libraries/{id}/reading-progress?path=` (`readingBook`: from the index
  alone, `catalog.BookEbookAt`, a part path its book's, on demand only when no indexed book is there;
  404 for no book or no ebook) keyed on the BOOK's path (never the epub's or a track's), table
  `reading_progress` (`0057`, carried by `carryListeningState`), `{"reading_progress": row | null}`;
  PUT `{cfi, progression, device_id, updated_at, base_version?}` answers the row stored after the
  save plus `other_place` on a diverged save (see Progress reconciliation); 400 for a CFI not
  starting `epubcfi(` or over 2048 bytes, a missing or null progression (never read as 0) or one
  outside [0,1], an updated_at that isn't RFC3339 (or falls before year 1 in UTC, whose stored form
  would be none), a missing or empty device id (two saves with none would read as one device's and
  never diverge) or one over 200 bytes, a negative base or an unknown field (`device_name` is
  accepted and ignored: the name is the token's, `Credential.DeviceName`, never the body's; an
  echoed `other_place` is accepted, a whole stored row isn't). No listening heartbeat on a save.
  **The read-along map** (Bridge Phase 3c, capability `readalong`, `api/handlers_readalong.go`): `GET
  /libraries/{id}/readalong?path=<the book's own path>` serves the book's `.audiosilo/readalong.json`
  (see Bridge attachments) as `application/json`, gzipped for a client that accepts it
  (`spa.AcceptsGzip`; `Vary: Accept-Encoding`), `Cache-Control: private, no-cache` and a strong ETag of
  the file's size and mtime, `-gz` for the gzip answer (chosen by the request's Accept-Encoding before
  any gzipping, so a 304 gzips nothing; server#265; 304 on a match). **Header auth only**, as
  `/ebook` (`requireAuth`; `?token=` is never read; it stays under the request timeout: a map is
  at most 16 MiB, no media route). Each request reads the map from disk (`library.ReadReadalongMap`:
  a real `.audiosilo` folder that isn't ignored, a regular file never through a link, read once
  more when the Bridge renamed a new map in as it was opened: `metadata.readRegularFileInfo`'s
  `retrySwap`, which every attachment, `feed.json` and `library.json` read shares since server#265) and
  checks it again (`metadata.ParseReadalongHead`) and against the book now (`catalog.ReadalongState`,
  the ebook's digest the stored one only while the file on disk still has the signature it was
  computed for, one stat against `ebook_hashed_sig`: `library.Scanner.EbookDigestNow`, else hashed now and
  recorded with the new signature, so an EPUB replaced since the last scan is never answered with
  the old file's digest and is hashed once, not on every ask); the indexed head never
  vouches for the bytes sent. A part's path, a podcast library, a book with no ebook, no map, or a
  map that fails its checks: 404 `no_readalong`; a map made from another ebook or other audio: 409
  `readalong_stale` with `reason` (`ebook_changed`, `audio_changed`); outside scope 403. The player's
  `Book.readalong` (`{state: ready, coverage, generated_at}` or `{state: stale, reason}`;
  `catalog.Readalong`) is on `GET /libraries/{id}/item` and the console's book detail only, never a
  list row, omitted with no ebook or no valid map (`api.readalongOf`, from the indexed head; a failure
  is logged and reads as none; its digest checked the same way, so an ebook edit shows before any scan).
- **Recognized audio** is `metadata.AudioExtensions` (`IsAudio`), including `.mp4`
  (AAC-in-MP4 audiobooks); `media` serves it as `audio/mp4`. `.mka` (Matroska audio, #161)
  has its chapters read by ffprobe; stored, it is `audio/x-matroska` (also sniffed from its EBML
  header `1A 45 DF A3`) and never plays in browsers or on iOS (`media.DirectPlayable(format, codec)`'s
  `containerFormats`; `Book.PlaysInBrowsers` checks every file's container, since a folder book's
  format is its first file's; the SQL twin `directPlayableExpr`, the console's Playback column, stays
  "as stored"). **Remux (server#174)**: while ffmpeg is configured (`API.remuxOn`: `a.ffmpeg != ""`
  and the cache opened; no capability flag), `GET stream` of an `.mka` (with or without `download=1`,
  not `transcode=1` or `rendition=`) serves, at the SAME URL, its lossless copy to MP3 (`media.NewRemuxer`, a `FileCache`: `-f matroska`
  forced, `-protocol_whitelist fd`, `-xerror` so a read error fails the run rather than leaving a short
  copy, `-c:a copy`, `+bitexact`, chapters as ID3 CHAP frames), as
  `audio/mpeg` with Range and its own validators (a strong ETag `"remux-<key>"`, Last-Modified the
  copy's mtime, never the source's: a 2.4.x client's If-Range / If-Modified-Since for the stored
  Matroska gets the whole MP3, never a 304 or a spliced range), `download=1` named `<stem>.mp3`; `original=1`
  serves the stored bytes. Old native builds stream it with no change (an old iOS build saves a
  download under `.mka`, which AVFoundation won't open offline). The cache is
  `<data>/cache/remux/<sha256(abs, size, mtime)>.mp3` (0700, made again if it went; `.part` synced then
  renamed, never under the key when the source changed while read; `.part` swept at
  start; LRU by mtime, a hit (no lock) touches it at most every 10 min, 8 GiB cap swept after each remux, the new copy kept; skipped
  under 1 GiB free after it and the other runs still writing, the rendition cache's too (one reservation,
  `FileCache.ShareDiskGuard`), `internal/diskspace`); single-flight per key, 2 at once, run under
  `baseCtx` with a 30 min timeout from when it has a slot (a waiter that leaves doesn't stop it). ffmpeg's refusal (not one
  MP3 stream: AAC/Opus/FLAC in `.mka`; an exit status before it wrote anything, so a run killed or
  failed partway, a full disk or a read error, is not one, nor one whose log names an I/O error before any
  output: `readFailed`, server#212; a run its own timeout killed is, server#289, the server shutting down is not)
  is remembered per key in memory for an hour (`cacheRefusalTTL`: what looks like the file's fault may be the
  environment's, an ffmpeg missing an encoder; a changed file is a new key at once) and the stored bytes served,
  as they are for no regular file at the path (a 404),
  so those still play on web only, through `?transcode=1`. A remux that can't be had now (still
  running after `remuxColdWait`, 2 s, under the player's 3 s stall timer and Android's 8 s read
  timeout; the disk guard; a failure other than that refusal) answers the plain URL with the live
  transcode instead (`serveRemuxed`; `download=1` still named `<stem>.mp3`), the remux going on for
  the next request, since `direct_playable` told the web player not to ask for one (a 503 while the
  server stops). Two requests wait longer for a remux still running (server#212): a `download=1`
  (its saved copy would otherwise be the lossy transcode) and a stream that finds every transcode
  slot busy (else a 503), up to `remuxDownloadWait`, 45 s, sending nothing meanwhile, so under the
  60 s an idle answer gets from Android's downloader and a reverse proxy; past it they are
  transcoded after all (the busy 503 when no slot freed). The remux's own timeout (30 min) is a
  failure, logged, not this wait. `/item` and `/chapters` warm the book's MP3-in-MKA files (`API.warmRemux` ->
  `FileCache.WarmAll`: in the background under `baseCtx`, one file and one warm at a time across the
  server so warms hold at most one of the 2 slots, a book warmed once while queued, at most 16 queued
  (more dropped), the cached (not marked used) and known refusals skipped, never a podcast library;
  never blocks the answer; the launcher waits for them at shutdown), so the first
  stream is usually a hit. `/item` and `/chapters` say
  `direct_playable: true` for a book `PlaysInBrowsers` or, while remux is on, `PlaysRemuxed` (codec
  `mp3`, every file a browser container or an `.mka` whose own codec, else the book's, is MP3):
  `API.directPlayable`. An `.mka` beside a file of the same name in
  another audio format (`Book.m4b` + `Book.mka`) is that file's source or copy and stays out of the
  book, as an `.aax` does (`library.folderAudio`), and out of the `/fs` listing too
  (`library.shadowedMKA`, the one rule both use); a lone `.mka` is listed. An `.epub` is never
  audio: `/stream` refuses it (404) and `/fs` doesn't list it; a book's ebook is served only by
  `GET /libraries/{id}/ebook` (see Path-addressed API).
- **In-process probe (server#261, `metadata/fastprobe*.go`)**: `metadata.ExtractTimeout` opens a
  file once and reads its tags, the iTunes narrator and the probe through one 64 KiB block cache
  (at most 4 MiB kept); the probe answers what ffprobe would (format duration as its `"%f"`
  string, first audio stream's codec, chapters, the format tags `applyProbe` reads:
  `probeTagKeys`) for MP3, M4B/M4A/MP4, FLAC, Ogg Vorbis/Opus/FLAC and MKA, as ports of FFmpeg's
  demuxers. **Parity is the contract**: values equal the configured ffprobe's, by its release's
  rules (`ffRules`, read once per path from `-version`: 7.1 and 8.1 are known, they differ in MP3
  gapless durations and empty UTF-16 ID3 strings; any other release, a master build or a failing
  `-version` turns the probe off and ffprobe reads every file, as before; the start-up log says
  which, and `toolfetch` downloads BtbN's n8.1 branch build on Linux and Windows, never master (unless that
  asset is gone, a 404, and no tools are cached: master's build then, the probe off, warned), and
  replaces a master build an older server cached without holding up the start: the cached build serves
  that run, the release build is fetched in the background into `<data>/tools/next` and swapped in at
  the next start, ffprobe first (a rename that fails keeps the stage, finished at the following start),
  a failed fetch retried no sooner than 24 h later (`.release-fetch-failed`), server#269; a download
  unpacks in a `.download-*` folder inside the one it fills, removed at the next start if a stop left it). Anything a reader does
  not model exactly is `unsure` and that one file goes to ffprobe, silently (a debug line,
  `metadata.SetLogger`'s logger, names the file and the reason, a probe limit's included): VBR MP3 with no
  Xing/VBRI (decision 1: ffprobe's estimate kept), a concatenated MP3, a FLAC CUESHEET or ID3
  prefix, fragmented/compressed MP4, an unknown handler or sample entry, multi-stream Ogg, an MKA
  with no duration or a second SeekHead, another container named .mp3 (WAV, FLAC, Ogg, MKV, MP4),
  a chapter track whose one edit needs keyframes or a DTS shift (stss/ctts; its samples are cut
  at the edit's end as mov_fix_index does, so a trailing zero-length sample is no chapter), a
  track `mov_read_trak`'s sanity check calls a broken header (chunks but no stts/stsc entries or
  no sample size or count, or samples but no chunks: FFmpeg then reads its duration in 1/90000,
  server#276), an mvhd/mdhd timescale past INT32_MAX, a rescale past int64, an ID3v2.2 `CHA`
  frame (FFmpeg's three-letter match reads it as CHAP),
  a crafted table past a bound (a chapter track over 65536 samples or with counts past its
  tables, MKV SimpleTags over 4096 or 8 deep, a compressed ID3 frame over 16 MiB), a read error,
  a reader's panic, no duration, or a probe limit: each file's probe stops at a deadline (the
  caller's context, and `min(timeout, maxProbeTime)`: at most 5 s, under the timeout ExtractTimeout
  gives ffprobe) or after `maxProbeBytes` (64 MiB; the real-library p95 is ~0.9 MiB) read, checked on
  every block read, before inflating an ID3 frame or growing a repeated Vorbis key
  (`blockFile.check`) and, every 256 ticks, in the walks that loop without reading
  (`blockFile.tick`; a `probeLimit` panic `fastProbe` recovers). A file neither reads keeps
  ffprobe's error as `ProbeErr` (Health's text, `ErrProbeRejected`; FFmpeg's `[mp3 @ 0x...]`
  address dropped, server#272). The tag read and the narrator walk before the probe are bounded
  too (`readFile`): the ffprobe run's timeout and the caller's context, and the byte budget; a
  tag read out of time skips the probe (ffprobe, killable, answers; one that finished just before
  its deadline passed keeps its tags), one past the budget or panicking leaves dhowden's tags
  unread and the probe still runs (a narrator walk past it keeps them). While armed every read from the file runs on its own goroutine and is waited for
  only until the deadline (`blockFile.waitRead`), so a pread on a share that stopped answering
  costs a goroutine (and the file open) until it returns, never a stuck reader (`os.Open`/`Stat`
  are not bounded); once `maxAbandonedReads` (16) such reads are outstanding process-wide
  (`abandonedReads`; reads already under way can take it past that) a new read fails fast (`readsSaturated`, `reasonSaturated`), the file
  not opened and no ffprobe run, its tags unread, logged once until they have all returned. A re-index
  where any file's read was cut short or refused (`Metadata.TagsUnread`, `ReadRefused`;
  `Book.TagsUnread`) rewrites nothing of the stored book (`catalog.recordReadProblem`: names,
  length, codec, chapters, cover flag, mtime and size stay as the last full read left them) but
  its read problem, `unreadable` ("reading it timed out", or for a refused read "too many earlier reads
  are stuck", on this library or another, with ffprobe or without), so the next scan reads it again (the stored
  mtime and size are the old ones, else `problemCleared`); a new book read so is written with the
  path's names. With no
  ffprobe (`--ffprobe ""`, or none found) the probe still runs, by FFmpeg 8.1's rules
  (`noFFprobeRules`, decision 2, server#269): a file it is unsure of has no duration and no
  `ProbeErr`; a read out of time says so (`Metadata.ProbeTimedOut`) and the scan reads it again
  alone, as an ffprobe time-out; a scan reads an unchanged book with no stored duration or codec
  again once per server run (`Scanner.rereadUnprobed`), so a server that ran without ffprobe
  gets its books' lengths from its first scan since. `metadata.ExtractTags` reads tags alone. A
  key `applyProbe` starts reading joins `probeTagKeys`. A CBR MP3's bitrate-estimated duration counts each ID3 picture read_apic
  keeps (its stream's 1/90000 can round past the audio's). Tests: `testdata/probe` (~100 fixtures, < 3 MB, made by
  `scripts/gen-probe-fixtures.sh` with `cmd/mkfixtures`) and `golden-<release>.json` per known
  release (`cmd/probediff -golden`; the script writes the Docker image's via docker), checked with
  no ffprobe (`probe_golden_test.go`); both views come from one mapping (`viewOf` of a
  `probeResult`: `probe()`'s for ffprobe, `readFile`'s, Extract's own opener, for the probe), so
  the checks compare what Extract stores; `probe_live_test.go` compares the installed ffprobe
  (`AUDIOSILO_PROBE_DIRS` adds folders), and CI's `probe-drift` workflow runs it against the
  image's ffprobe (and weekly), failing on drift or a release with no rules. Adding a release:
  its rules where they differ, its golden, a `knownRules` entry. `cmd/probediff <dirs>` is the
  dev diff/timing tool. A test whose ffprobe is a stand-in script on valid files calls
  `metadata.ForceFFprobeForTests(path)` (`library`'s `stubProbe` does).
- **Transcoding**: `GET /libraries/{id}/stream?path=…&transcode=1` pipes the file
  through ffmpeg to MP3 (`media.Transcode`) for codecs browsers can't decode;
  `&t=<seconds>` starts mid-file (transcoded output isn't byte-seekable, so seeks
  re-request). Direct serving + Range stays the default. A podcast episode's verified descriptor
  (`media.TranscodeOpened`, server#181) is ffmpeg's stdin read seekably as `fd:` (input seek, and a
  non-faststart m4a demuxes) when the ffmpeg lists the fd input protocol (`inputFor`: probed once
  per ffmpeg path, `-protocols`, 5 s, asked again after a probe that couldn't run), else the old
  `pipe:0` with an output seek. A HEAD gets the transcode's headers without an ffmpeg run. The scanner records each
  book's audio `codec` (ffprobe `codec_name`, `0008` migration); `item`/`chapters`
  expose `direct_playable` (`Book.PlaysInBrowsers`: `media.DirectPlayable` of the book's format and
  codec, and every file's container) so a client knows when to
  transcode. Gated by the `--ffmpeg` flag (the `transcode` capability reflects it).
- **Watch renditions (Track W2, capability `renditions`, `media/rendition.go`, `api/handlers_renditions.go`)**:
  a small copy of each audio file of a book for a watch to download, made by ffmpeg into
  `<data>/cache/renditions/watch/<key>.m4a`, never into the library (read-only). The encode (`renditionArgs`) is
  AAC-LC (ffmpeg's own `aac`; HE-AAC needs the non-free libfdk_aac), mono, 48 kbps, 44.1 kHz, MP4 with
  `+faststart`, no video, metadata or chapters, `+bitexact` (format and encoder), the input only the descriptor
  (`-protocol_whitelist fd|pipe`, the demuxer ffmpeg's guess); no `-xerror` (it fails a whole encode on one damaged
  MP3 frame, ffmpeg 7.1 tested). Its duration is the source's (within 0.1 s on MP3 and M4B), so positions and
  chapters (CROSS-REPO §6) hold; since a read error partway still exits 0, a copy more than 1 s short of the
  source's exact duration (`metadata.ExactDuration`, the in-process probe; skipped when it is unsure, as for a VBR
  MP3 with no Xing/VBRI header, or can't measure the copy) is not cached and is remembered as a refusal (422
  `rendition_failed`, state `failed`), since a source short itself (a truncated MP3 whose Xing header names the
  whole length) would otherwise be encoded again on every request; like every refusal it is forgotten after an
  hour (`cacheRefusalTTL`), so a read error partway is tried again then. The cache is the remux's `media.FileCache` over another `cacheSpec`: key
  sha256(abs, size, mtime, `watch-v1`), so a new encode version (`renditionVersion`) is a new key; 10 GiB LRU
  of its own `.m4a` files, one encode at a time, 2 h timeout from its slot, the same 1 GiB disk guard (one
  reservation with the remux's: both under `<data>/cache`), `.part` synced then renamed, never cached when the
  source changed while read, ffmpeg's refusal (an exit status before anything was written, or a copy with no
  audio track: ffmpeg exits 0 with one for an M4B whose index is at its end read through `pipe:0`, an ffmpeg
  without the fd protocol; `metadata.MP4HasTrack`; or a short copy) remembered for an hour or until the file
  changes. A failed run's error carries the first and last
  2 KiB of ffmpeg's log. The remux is unchanged (same dir, keys, ETags). `GET
  /libraries/{id}/stream?path=<file>&rendition=watch[&download=1]` (same auth as the stream: session, API key,
  `?token=`, a cast token for its book) is checked in this order: scope (403/404), kind (400 code `bad_rendition`
  for anything but `watch`, and for a podcast library: the watch has no podcasts, and a watch client never
  sees one anyway, `auth.SeesPodcasts`), `renditionsOn` (404 `rendition_unavailable`: no ffmpeg or no data dir),
  audio and a regular file (404); then the copy, served with Range (`http.ServeContent`), `audio/mp4`, a strong
  ETag `"rw1-<key>-<size>"` (If-Range resumes only the same encode), Last-Modified the copy's mtime,
  `download=1` named `<stem>.m4a`. A cold encode is waited for up to `renditionWait` (45 s, the remux's
  download wait), then 503 `rendition_pending` with `Retry-After: 15` while it goes on under `baseCtx`; never a
  live transcode. 422 `rendition_failed` is a remembered refusal (ffmpeg's, or a short copy; for an hour), with
  `Retry-After` the seconds it has left (`FileCache.RefusedFor`, server#289);
  503 `rendition_error` (`Retry-After: 60`) a failure not the file's (disk guard, a run killed partway, the
  source changed). `POST
  /libraries/{id}/renditions?path=<book>` `{"kind":"watch"}` queues every file of the book whose copy is missing
  (`FileCache.Queue`) and answers 202; at most 256 encodes wait or run across the server, a stream's included
  (`cacheSpec.queueMax`): past it a prepared file stays `missing` for the client to ask again and a cold stream
  answers 503 `rendition_error`; `GET .../renditions?path=<book>&kind=watch` queues nothing; both `{kind,
  files: [{path, state: ready|pending|failed|missing, bytes (ready only), retry_after_seconds (a remembered
  refusal only: not a failed file that isn't there)}]}` in the book's file order
  (`Book.AudioFiles`: a single-file book, which has no `book_files` rows, is its own file), the book resolved as
  `/item` does (a part path names its book; `requireAuth`, so no `?token=`). A download is not a
  listen: none of it marks a session or a transcode. A watch client is `audiosilo-watch/<ver> (watchos|wearos)`
  in `X-AudioSilo-Client`, shown as a watch in the console. Tests: `media/rendition_test.go` (args, keys,
  a real encode's codec, channels, rate, bit rate, duration and moov-first, Range/If-Range, queue and status,
  refusal, a trackless copy refused, the bound on Queue and Open, eviction per cache, the shared disk guard),
  `api/renditions_test.go` (capability, stream, 503 pending, 503 rendition_error, refusals, scope allowed and
  denied, the routes, a loose root file).
- **Sharing & access (shares)**: access is via `shares` = named sets of path
  rules. `catalog.UserScope`/`UserScopes` build a `Scope` per library
  (`AllowAll` or specific `Paths`); `Scope.Allows` gates item endpoints,
  `Scope.VisibleInBrowse` filters `/fs` to a navigable subtree, and
  `pathFilterSQL` scopes `ListBooks`/`Search` and the player's browse aggregates
  (`People`/`Series` given a `*Scope`, which also pins them to the scope's library;
  nil = the admin's unscoped view). Every content handler authorizes
  the path against the caller's scope (`authorizedPath`). Admins are `AllowAll`, except in a
  podcast library, where the old-client gate and private shows apply to them too (see Podcasts).
  Whole-library access is sugar (`GrantWholeLibrary` → a `""`-rule share, marked with
  `shares.whole_library_id`). `wholeLibraryShare` reuses the share marked with the library, else an
  unmarked one with the description "Whole library" named "Library: <name>" (a grant made before
  0015) or "Library: <name> (<id>)" (one an interrupted grant left), and either only while its sole
  rule is that library's `""`, or it has no rule and no holder, since the grant adds that rule and hands
  the share out: anything else (an admin's own share with the name, server#223, a narrowed grant, a
  rule-less share someone holds, a share marked for another library) keeps its rules and holders, and
  the grant gets a share of its own, "Library: <name> (<id>)" when the name is taken (then numbered
  from 2, server#230), made whole and marked in one transaction that looks for the library's grant
  share again first: write transactions run one at a time, so first grants sent together share one.
  A marked grant's rules change only by granting or revoking the library: `POST`/`DELETE
  /admin/shares/{id}/paths` on one is 409 `library_grant` (`catalog.ErrLibraryGrant`, read inside the
  edit's transaction; server#254), as the console never offers it.
- **Move-tracking**: the scanner fingerprints files; when a path vanishes and a
  new path with a matching fingerprint appears, `Scanner.detectMoves` migrates
  durable state old→new (`catalog.MoveDurableState`). A folder book's fingerprint is its
  first file's; one indexed in byte order before #160 still matches by the file that order
  put first (`byteFirstFingerprint`, in `detectMoves` and `IndexMoved`). The on-demand index of a
  book not indexed yet (a renamed folder opened from Folders before a scan, server#209) looks too:
  `Scanner.movedFrom` takes an indexed book with its fingerprint that is gone from disk
  (`catalog.MoveCandidates`, `bookOnDisk`) as `IndexMoved`'s from (`indexBookFrom`, which, once the
  state has moved, finishes the index and drops the old row past its caller's cancel or deadline, so
  the state never sits on a path with no row). Re-tagging keeps state via
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
  by exact name (so a case-only rename, of it or a folder above it, counts). The on-demand move
  (`IndexPath`'s `movedFrom`, `IndexMoved`) does the same from one listener's request: a move is a fact
  about the disk, so every listener's folder favourite follows it (owner decision on server#254,
  `TestIndexPathMovesEveryListenersFolderFavourite`).
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
  in exactly the order a book of that disc alone has them (`sortFileOrder`, as
  `audioEntries`/the walk read them), since the state carry-over maps a disc
  position as offset + position. Every folder book's files play in natural name order
  (`sortFileOrder`, #160: `naturalLess`, audiosilo-meta's `scan.NaturalLess` copied and held to
  it by `TestNaturalLessIsMetas`, as the Bridge and the community metadata number a book's files,
  and so its sidecars' chapters, in that order: digit runs by value, "2-" before "10-", the rest
  byte by byte case folded, so "01 - Prologue" plays before "01a - Interlude"; ties byte-wise); a
  book indexed in the old byte order is re-indexed by the next scan (`Signature.FileOrder`, the
  stored order's key, differs),
  which moves a listener's whole-book place on it: no remap. The key is stored on the row
  (`books.file_order`, `0055`, written by `UpsertBook` with the upsert's `indexed_at` in
  `file_order_indexed_at`, NULL for a single-file book), so a scan reads no `book_files`:
  `Signatures` falls back to them only for a folder book whose key isn't recorded (NULL: indexed
  before `0055`; or recorded for another index: an older server, run against the database, has
  re-indexed the row since, its files in its own order: `fileOrderExpr`,
  `Signature.FileOrderUnset`), and a scan that finds such a book unchanged records the key
  (`SetFileOrders`, in `flushBackfills`) with no re-index. Files and chapters keep real paths in the discs
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
  **A book's ebook** (Bridge Phase 3b, `0056`, `library/ebook.go`): `books.ebook_path` is the one
  `.epub` the reader opens, by the Bridge's own rule (`generate.epubBeside`), so the reader and the
  Bridge's "auto" source agree: a folder book's one regular, non-hidden
  `.epub` directly in its folder (a link never counts, several is none); a single-file book's
  `<stem>.epub` beside it as the disk opens that name (the Bridge's `os.Stat` of it: exactly that name
  on a case-sensitive disk, any case of it on a case-insensitive one, recorded under the computed name;
  `ebookNamed`, one `Lstat`), widened for the reader to that name spelled in another Unicode form
  (server#228: an NFD copy from macOS beside NFC audio on a disk that tells the forms apart; recorded
  under its own name; in another case too only where the disk opens the name), which the Bridge
  doesn't find, so the estimate (`library.BookEbook`) keeps the Bridge's lookup as spelled and never
  quotes an ebook run the Bridge won't make; a link counting while it leads to a regular file inside the library;
  readable (`readableEpub`: a zip listing `META-INF/container.xml`; DRM is not judged, as the Bridge
  doesn't judge it). Ignore rules don't apply (the Bridge's rule doesn't read them). One rule
  function, `ebookCandidate`, serves both callers: the scan feeds it the `.epub` entries its discovery
  walk already lists (`sidecarFiles.epubs`, `ebooksOfWalk`; a single-file book only the entries named
  in some case and Unicode form of its `<stem>.epub`, `ebookNameKey`, through a name index made once
  per folder), an on-demand index (`indexBook`: `IndexPath`, `IndexMoved`) what is on disk now
  (`ebookOnDisk`: a folder book's folder listed, a single-file book's name looked up, then its
  folder's epubs listed only when the name has another Unicode form, `hasOtherForm`, and isn't
  there as spelled); `TestScanRecordsEbookByBookEbookRule`
  and `TestEbookCaseVariantBesideSingleFile` hold them together. `books.ebook_sig` is the named file's
  size, mtime and name (`catalog.EbookSig`; a link's target's), recorded whether or not it is readable
  (a corrupt file, a link out of the library or to nothing, a FIFO: none, recorded by its signature,
  `TestScanEbookLinkToNothing`), so a scan that finds the book unchanged opens it again only when the
  signature changed (an index, a re-index and `BookEbook` always read it). A book found unchanged
  whose signature differs (an epub added, replaced, renamed, removed, or a second one added; every
  book after the upgrade, as 0056 starts them at '') is recorded by `SetEbooks` with the scan's other
  backfills, with **no re-index** (`TestScanBackfillsEbookWithoutReindex`), and only over the row as
  the scan read it (`EbookReading.Was`: its `ebook_sig` and `indexed_at`, so an on-demand index since,
  newer than the walk, stays; `TestSetEbooksCompareAndSet`). A book's page (`GET /item`) stats the
  recorded ebook too (`library.Scanner.RefreshEbook`, one stat and one row read): a file replaced in
  place (another signature) is checked by the same rule (`checkEbook`) and recorded the same way
  (compare-and-set), so `ebook.size` follows it before the next scan (BRIDGE-16,
  `TestItemEbookReplacedInPlace`); one that isn't a readable epub at that moment (a copy still being
  written) answers no ebook and isn't recorded, so the next request reads it again; a new, renamed or
  removed file still waits for the scan. A file that can't be read right now (a
  folder that won't list, a permission, a flaky mount: `EbookReading.Unread`) keeps what is stored, in
  a backfill and an index alike (`UpsertBook`; a new row gets none), so the next scan looks again
  (`TestIndexKeepsEbookItCantRead`). Every open, the scan's and the route's, is `library.OpenEbook`: a
  regular file inside the library checked before it is opened (a FIFO's open would wait for a writer,
  `TestOpenEbookFIFO`) and, once open, still the very file its name leads to (`os.SameFile`).
  `books.ebook_size` (`0058`, backfilled from the signature) is the recorded file's size, written
  with `ebook_path` (`UpsertBook`, `SetEbooks`). Only a book with a read-along map has its ebook
  hashed (Bridge Phase 3c: `books.ebook_sha256`, computed for the `ebook_sig` in
  `ebook_hashed_sig`, so a digest whose sig isn't the row's is stale): the scan's post-pass
  (`Scanner.hashReadalongEbooks`, after the walk; `RefreshAttachments` for its one book) hashes the
  books `catalog.ReadalongEbooksDue` lists (a stored valid head, an ebook, a digest not current) through
  `library.EbookDigest` (`OpenEbook`), skipping one whose stored digest is already of the file on disk
  (`Scanner.EbookDigestNow`: a request recorded it before the scan), and `SetEbookDigest` records it
  with the signature of the file it hashed, only while the row still names that ebook (`ebook_path`):
  a request may record one for a file newer than the last scan, which that scan then finds current. Hashing every ebook would read a
  library whole over a network share.
- **Metadata overrides (admin redesign Phase 2a, `catalog/overrides.go`)**: an admin's
  edits never touch files. A `books` row holds the **effective** values - what the scan
  found (kept in `books.scanned`, JSON field -> value, stamped with the upsert's
  `indexed_at`: a snapshot whose stamp isn't the row's was left by an older server, so
  `loadLayers` reads the scan's values off the row instead), then an Audiobookshelf
  `metadata.json`'s values (`books.abs_file`, below), then `book_enrichment`
  (asin/isbn), then `book_overrides`. `bookLayers.resolve` is the ONE statement of that
  precedence (and of each field's source); `refreshEffective` writes its values to the
  row (plus chapter titles from `chapters.scanned_title` + `chapter_overrides`, and FTS,
  each column in `ftsText`'s form: Han, Hiragana and Katakana a character a token, since
  unicode61 splits only on spaces and punctuation, server#178; `buildMatchQuery` makes a CJK
  token a phrase of its characters, the last a prefix, and `ftsPhrase` writes the credit
  filters' phrase the same way; `Catalog.IndexSearchText`, run by the launcher after
  `KeySeries`, rewrites once the rows an older server wrote, gated by `server_state`
  `search_text` = `searchTextVersion` (rules + `unicode.Version`), no migration)
  and the book page shows it as provenance, so the two can't disagree. `UpsertBook` runs
  `refreshEffective` in the scan's own transaction, which is what makes an edit a lock (a
  rescan rewrites the scanned values and re-applies the edit before anything can read
  the row) and why there is no separate post-scan enrichment pass any more.
  `BackfillASIN` (the Bridge's enrichment, `setEnrichmentTx`) and `EditBook`/`EditBooks` call it too. Players, search, `/fs` and
  export read the row, so they see edits with no join. The player's book JSON carries
  `published` on every book and `description` only on `GET /libraries/{id}/item` (api's
  `itemBook` adds it; `catalog.Book` keeps it `json:"-"` so list, search and recent pages
  stay small); `has_cover` and per-file codec are admin-only (`json:"-"`). Every player book (list,
  search and item alike, all `omitempty`) also carries `subtitle`, `publisher`, `language`,
  `abridged` (a bool, absent while unknown) and `genres` / `tags` (lists). Validation: `normalizeOverride`; sources: a scanned value is `path` when
  it equals what `DeriveFromPath` (or, path-first, `FromPathLayout`) yields, or for the title
  `metadata.PathTitle` (the path's title `TitleNotSeries` may take), else `tag`; one an ABS
  file gives is `abs`; an override is `edited` or
  `community`; an enrichment-attached ASIN/ISBN reads as `community`. Revert = delete the
  override + `refreshEffective` (restores the scanned value, the ABS file's where it gives one; no
  reindex, no disk).
  **Audiobookshelf metadata.json** (`metadata/absfile.go`, `catalog/absfile.go`, migration `0046`):
  ABS keeps a `metadata.json` in a book's folder ("Store metadata with item"), its record of the book
  with its matches and its users' corrections, often holding the ASIN/ISBN the tags don't. A folder
  book's file (a regular file named exactly that, not ignored: an ignore rule `metadata.json` turns it
  off for a library; never for a file book, nor a loose file at the root) is a layer OVER the scanned
  values, tags and path alike, path-first libraries included (ABS's own default ranks it above the
  tags): `bookLayers.overABS`, in `scannedFields` before `titlePosition`. `metadata.ParseABSFile` is
  lenient (a wrongly typed value costs that value; <= 4 MiB): title, subtitle, authors and
  narrators joined with ", " (strings or `{name}`), series `"Name #seq"` (the first is the main series
  with its position, the rest `more_series`; a series without a position keeps the tag's only while
  it is the same series, by `seriesKey`), `publishedDate` else `publishedYear` as `published`, the
  HTML description as plain text, publisher, language, `abridged` (only `true` says anything: ABS
  writes false for every book nobody marked), genres and tags (lists of strings, a non-string item
  dropped; `CleanLabels`), asin, isbn; chapters are not read. An empty value says nothing (never clears a tag's). `absFields` stores each as an edit of its
  field would (`CleanOverride`; a refused value left out), so an ABS ASIN makes a book matched (Health,
  `/meta`, the community chapter check, a refresh run). `books.abs_file` holds those fields (JSON,
  `''` for none), `abs_sig` the file's signature (`metadata.ABSSignature`: size + mtime ns, `''` for
  none) and `abs_indexed_at` the `indexed_at` it was read for (NULL before `0046`), narrow columns
  first so `Signatures` reads no JSON; `UpsertBook` writes all three (`absColumns`; nothing for a
  file book). Discovery records each folder's file signature from the walk itself (`audioDirs`'
  `absFiles`, the walk's own entry, no extra directory read; `IndexPath`, with no walk, stats it: `absSigAt`), and `enrich` reads
  the file only when there is one (`Scanner.readABS`). An unchanged book whose signature differs,
  or that has a file not read for this index (`abs_indexed_at` NULL, or another index's: an older
  server re-indexed it), is read without a re-index: `Signature.ABSSig`/`ABSUnchecked`,
  `Catalog.SetABSFiles` in batches of 250 beside the path backfill (`eachPathBatch`, shared with
  `SetPathReading`); a book with no file and none before is left unwritten, so the first scan
  after the upgrade touches only books that have one. `setABSFile` re-resolves a book unless the
  reading equals the one recorded for this very index (an older server's re-index resolved the
  row without it). `metadata.ReadABSFile` reads a regular file only, never through a link (one
  swapped in after the walk included: `os.SameFile` on the opened file). A file that can't be used
  (not JSON, too large, not a regular file: `metadata.ErrABSFileInvalid`) keeps its signature (read
  again once it changes); one that can't be read right now (gone since the walk, a flaky mount) is
  left as it was by the backfill and recorded as none by a re-index, so the next scan reads it
  again (`readABS`' `read`); both log a warning. Known gap: a position read from an ABS title (`titlePosition`) shows as `tag`.
  **Bridge attachments** (`metadata/attachments.go`, `library/attachments.go`, `catalog/attachments.go`,
  migrations `0048`, `0050`, `0051`, `0058`; BRIDGE-PLAN.md Phases 1a, 1c, 1d and 3c): the Bridge writes a book's generated `characters.json`,
  `recaps.json`, `description.json`, `chapters.json` (chapters found in the narration), `bridge.json` (provenance) and
  `readalong.json` (Phase 3c's read-along map) into `.audiosilo/` in a folder book's own folder, or beside
  a single-file book with its stem before each name (`Book.characters.json`). They are NOT a metadata
  layer (nothing in the effective row reads them, but `chapters.json`, a chapter source: see Generated
  chapters, so `SetAttachments` re-resolves a book (`refreshEffective`) and fires `changed()` when its
  stored `chapters` column changes, or when its stored reading wasn't made for this index (an older
  server re-indexed it, rebuilding its chapter rows without the stored list), and only then): the scan stores them checked (audiosilo-meta's
  `model.Characters` / `model.Recaps` / `model.Description`, bounded, a regular file only, never through a link) in
  `book_attachments`, and `/meta` serves them (below). A description (`metadata.ParseDescription`) is
  its text trimmed, 200-1500 code points (meta's `description.schema.json` bounds,
  `TestDescriptionBoundsMatchTheSchema` reads them from audiosilo-meta's embedded `SchemaFS`, imported
  as `metaschema`), no control character but a line break; a blank text is none, anything else fails
  the file. A signature lists only the names a book has, in `AttachmentNames` order (characters, recaps,
  description, chapters, bridge, readalong), and adding a name kept the others in order, so a book without one keeps
  its stored signature byte for byte. **A read-along map** (`metadata/readalong.go`) has its own bound,
  `MaxReadalongFile` = 16 MiB (one span per sentence: about 1.2 MB for 23.5 h); only its head is
  stored (`book_attachments.readalong`, never the sections): format exactly `audiosilo-readalong/v1`,
  `ebook.sha256` 64 hex (stored lowercase), 1 to 5000 `audio.files` each with a path of 1 to 1024
  bytes and a duration above 0, `coverage.text`/`coverage.audio` present and in [0,1], `sections`
  an array; anything else fails the file (a map that fails is none). Generator values and
  `generated_at` are cleaned like bridge.json's (a value lost, never the file). `catalog.ReadalongState`
  compares the head with the book now: the ebook's sha256 first (`ebook_changed`), then its audio
  files (`catalog.BookAudioFiles`: book-relative paths, a single-file book its base name; another
  count, another set, or a length off by more than 0.05 s: `audio_changed`), else ready. A map is not
  a generated part (`Book.generated` stays the same).
  bridge.json's `description.generated_at` is read as `AttachmentProvenance.DescriptionAt` (the rest of
  that object is the Bridge's; an object of another shape costs only that time, never the file). The discovery walk lists each `.audiosilo` folder
  ONCE (never walks into it; no audio lives there), and a book's signature (`books.attachments_sig`:
  name:size:mtime_ns of its own known names) is compared like `abs_sig`: an unchanged book whose
  attachments are new, changed, gone or not read for this index is read without a re-index
  (`SetAttachments`, compare-and-set against the state the scan began with, so a scan never lays an
  older reading over the Bridge's refresh since). A file that can't be used keeps its signature (read
  again once it changes); a folder or file that can't be read right now keeps what is stored (`Unread`),
  never taking a book's generated content away. `Scanner.RefreshAttachments` re-reads one indexed
  book's attachments, never its audio (the Bridge's `POST /bridge/rescan` mode `attachments`). The
  player's `Book.generated` (list, search, item, recent, the next book) is true when the signature names
  a characters, recaps or description file (`sigParts`, an allow-list: bridge.json, chapters.json and any name a later
  server adds are none; `Book.GeneratedParts`, off the wire, says which; the narrow column, no join), so the player asks `/meta`
  for a book with no ASIN/ISBN too. Derived from the files: rebuildable, never carried by a move.
  **Series ranges** (an omnibus of books 1-3): the `series_index` field may hold a range,
  canonical `"1-3"` (input `1 - 3` and the en dash too; `metadata.ParseSeriesRange`: 0 < start <
  end <= 100000, a span of at most `metadata.MaxSeriesSpan` = 100, since the slots a range covers
  are counted one by one); `refreshEffective` writes its start to `series_index` (what every
  client reads) and its end to `series_index_end` (`ParseSeriesPosition`; `FormatSeriesRange` the
  stored text). A `more_series` entry carries one as `position_end` (`SeriesRef.PositionEnd`,
  omitempty; `book_more_series.position_end`). Player `Book` JSON: `series_index_end` and
  `series_list[].position_end`, both omitempty; the admin book row `series_index_end`. `seriesSwap`
  carries a range both ways; the series aggregate's `positions` hold every whole number inside a
  range (`heldPositions`), so a player marking gaps sees none inside an owned omnibus.
  An admin edit that makes the main series a series its `more_series` lists (any spelling, `match.SameSeries`) -
  by setting `series`, or by reverting it when the series the revert leaves
  (`newMainSeries`: `loadLayers` + `resolve` without the series override) is listed -
  without setting or reverting `more_series` itself, swaps them (`catalog.seriesSwap`,
  per book in `editTx`, from the book's effective row): the old main series takes the
  entry's place in the list and `series_index` becomes the entry's position (unless the
  edit names `series_index`), written as the edit's own overrides; so reverting a swap
  swaps back. A derived value equal to what its field resolves to without an override
  (`bookLayers.unedited`: the scan's or folder layout's value; layers loaded only when a
  swap happens; a position only when the new main series is the series it numbers, by
  any case) is written as a revert of that field instead (`seriesSwap` returns sets
  and reverts, disjoint, never a field the edit names), so swapping back to a
  path-derived Sherlock Holmes #5 leaves `series_index` read off the path (unlocked,
  source `path`), while a list that was an edit before the swap stays one, and a
  listed Other #5 made main on that path-numbered #5 is still an edited 5. An old main
  series that can't be listed (name too long, position out of range) refuses the edit:
  `invalid(series)`, a 400, failing a bulk edit whole (`EditBooks` names the refusing
  book's path in the reason). A community edit never swaps: `matchrun.planSeries` is
  the one statement of how a match lays out series. The console drafts the same swap before saving (`book-model.ts`
  `commitField`), so its save sends the values and the server's swap doesn't fire;
  `saveRequest` sends the saved `more_series` when the admin took the drafted swap back
  (only for a swap `seriesSwap` can make, so an unlistable old main still gets the 400);
  as that save names `more_series`, a swap the console drafts writes its values as
  edits even where one equals the file's (the values shown are the same),
  and the undo of a series revert that swapped (`undoRevertRequest`) sends the list and
  position it had. `MoveDurableState` carries overrides and custom covers as one set: when the moved book
  has any, the new path's own rows in all three tables are dropped first; it moves
  them (with enrichment) in a transaction of their own, so a failure carrying the per-user
  state can't strand them. `detectMoves` doesn't pair a folder reclassified as a collection (or
  back) with its own first part, nor a joined book renamed away from its override with its first disc (`reclassified`).
  **Metadata source** (`libraries.metadata_source`, `0032`; `catalog.MetadataFromTags`, the
  default, or `MetadataFromPath`; admin-only on the wire as `metadata_source`, PATCH 400
  `invalid_metadata_source`): a path-first library resolves the scanned layer with the folder
  layout over the tags (`bookLayers.scannedFields`: `metadata.FromPathLayout`, where the top
  folder is the AUTHOR and disc/track folders are parts, unlike `DeriveFromPath`'s two nearest folders; a layout value goes
  over a tag's wherever it says anything and replaces the baseline's own reading even with
  nothing (a leaf named `Author; Series; Book NN[; Title]` says its series, position and title part, `LeafSeries`); the title always keeps one, and a tag title that IS the folder's name, number and all ("13 Reasons Why"), stays whole with no position read from it (never a `LeafSeries` leaf, whose parts the layout reads even when the title is the whole leaf, as an untagged book's is); the position goes with the series: the layout's own, else
  the scanned one only while the series it numbers stays). It is a resolve rule, not a scan one: the snapshot is unchanged,
  and `UpdateLibrary` re-resolves the library's books (`refreshLibrary`) in the edit's own
  transaction, no rescan (about 1 s per 5,000 books).
  `has_cover` holds whenever there is a sibling cover (`UpsertBook` enforces it) and is
  NULL until checked; the scanner backfills unchanged pre-0016 rows in one transaction
  with a tag read (`media.EmbeddedCover`, no ffprobe). A `scanned` snapshot carries `@rev` (`scannedRevKey`), the path baseline's revision: one
  without it (any older server's) was read when a lone folder above the book was its series.
  `Signatures` classifies such a book one folder deep (`pathCheckExpr`): settled by the snapshot
  alone (a tag's series and no author, missing or 0016's "": the folder is the author; the folder
  as series and author: the series was the path's), or only by its tags (the folder as series,
  another author or none: "Discworld/Mort" may be tagged Discworld; and any stale or blank
  snapshot, whatever revision it names). The scan reads the latter's primary file once
  (`readNames`, shared with `enrich`; not while ffprobe newly fails), and `Catalog.SetPathReading`
  rewrites both kinds' snapshots (skipping one re-indexed meanwhile, keeping an older server's
  chapter titles) and re-resolves them in batches of 250, flushed every 500 during the scan and
  on a Stop; no re-index. Until then `bookLayers.pathBaseline` resolves a snapshot against the
  baseline it was read with, so a path-first library still drops the folder as a series.
  **A title that is only the series** (`@rev` 2, `metadata/seriestitle.go`): many libraries tag
  the series as the album and the book as the title ("Mark Tufo/Mark Tufo; Indian Hill; Book 04;
  From The Ashes", album "Indian Hill"; "Eric Vall/Without Law/Book 10", album "Without Law", title
  "Without Law 10"), so `bookTitle`'s album over the title named every book of the series alike (the
  LibraForge benchmark: 305 of one library's 1,050, and wrong matches to book 1). `readNames` passes
  its title through `metadata.TitleNotSeries`: a title that is the album (`Metadata.AlbumTag` /
  `TitleTag`, the pair `bookTitle` chose from) and names a series the book is in (the series tag, the
  folder holding it unless the book is a disc or part folder, "Bastion/CD1", whose folder is the book,
  or `LeafSeries`' part of a leaf named `Author; Series; Book NN[; Title]`; series
  key, brackets out), unless the leaf names the book so too ("Frank Herbert/Dune/Dune", also beside only
  the author, a year, a volume marker or the series and 1: "Frank Herbert - Dune", `leafNames`), takes the
  title tag or the path's title (the leaf's title part, else `DeriveFromPath`'s; neither when a
  volume marker, a part label, only bracketed, or the series and the number 1, "Arena Road 1"):
  the two agreeing; else the path's for a book of several files (and a title tag that is the album
  and a track marker never counts), with no title tag that counts, or beside a tag carrying the
  series' name; else a book at position 1 keeps the album (a series named for its first book, whose
  title tag can be a chapter's, "End Credits"); else the title tag, except for a book of several files,
  whose lone first-file tag ("Prologue" in "Indian Hill/Book 3") counts only when it carries the
  series' name. It is the stored title, so the
  player, search and the match's query (`matchrun.QueryFor`) all see the book's own.
  **A leaf that states a series** (`@rev` 3, server#319): a leaf named `Author; Series; Book NN[;
  Title]` (`metadata.LeafSeries`, which returns a `Leaf`) states the book's series and position in
  `DeriveFromPath`, over any folder's: "Book 04" is 4, "Book 01-05" / "Book 10-12" a range
  (`ParseSeriesRange`, the start and `SeriesIndexEnd`), "Book XX" and "Book 00" the series with no
  position. As the path baseline it fills only a series the tags leave out (a series tag still
  wins, `readNames`), and it reads as the path's source; a path-first library's
  `FromPathLayout` reads the leaf too (its series, position and title part). The leaf's title
  stays the whole name in `DeriveFromPath` (`TitleNotSeries` reads the title part). The LibraForge
  benchmark's library of these (about 600 books): +39 auto-right, auto-wrong 8 to 7.
  **The tag reading** (`@rev` 4, server#175): a snapshot written before 2.4.0 and 2.5.0 read tags
  as they do now (a series tag carrying its position, `metadata.SplitSeriesTag`; a title in any
  script not generic, `IsGenericTitle`; the MP4 `\xa9nrt` item and a narrator tag over composer)
  still holds the old reading; the snapshot carries the narrator too, so a backfill rewrites it.
  **Backfill of revisions 2 to 4**: a snapshot older than today's that a newer revision may read
  otherwise (`Signature.NamesCheck`, `namesCheck` by `revNumber`: before revision 2 a title
  `metadata.MaybeSeriesTitle` flags, over the snapshot's title and series; before revision 3 a leaf
  `metadata.PathLeafSeries` reads; before revision 4 `tagReadingMayDiffer`: an MP4, a narrator, a
  series `SplitSeriesTag` splits, a non-ASCII letter in the title or path)
  is read once by the next scan with the PathRead read and rewritten by `SetPathReading` (which
  skips only a snapshot already at `scannedRev`), no re-index and no migration (a
  `PathFromSnapshot` one waits for that read too, so an unreadable file never records today's
  revision with its names unread). The reads run `readSlots` wide (`tagRead`, batched
  `tagReadsPerSlot` per slot, applied before each backfill flush; one a Stop cuts short is dropped
  for the next scan), so a large revision-4 backfill (every narrated book) costs one parallel tag
  read each, once. A current `@rev` 1 to 3 snapshot it doesn't flag
  (`Signature.RevStamp`: its stamp the row's `indexed_at`) is stamped at today's revision by that
  scan (`Catalog.StampScannedRev`, the revision alone, compare-and-set on a current revision 1 to 3,
  `revOlderOwn`), so later scans don't test it again (`MaybeSeriesTitle` is about 2 µs a row); a
  snapshot of no revision stays until its next re-index or path reading. A stamped book is never
  tested again, so a later change that makes the reading change more books (`TitleNotSeries` more
  titles, `MaybeSeriesTitle` or `LeafSeries` more flags) needs a new `scannedRev` and its own
  `NamesCheck` term, not a change within a revision.
  **Labels** (`catalog/labels.go`, migration `0049`): `genres` and `tags` are override fields whose
  value is a canonical JSON list (`""` for none; `normalizeLabels`: trimmed, spacing collapsed, each
  label once by `labelKey` (lowercased, spacing aside) keeping the first spelling, <= 64 characters,
  <= 50 labels; an edit over a bound is a 400, a file's list is cleaned leniently instead,
  `CleanLabels`, which every file source goes through). The value is `encodeCanonical`'s JSON (no
  HTML escaping), as `more_series`' is. Genres come from the genre tag (`metadata.SplitGenres`: split
  on `;` and NUL, never `,` (a store category holds one), blanks and placeholders such as "Audiobook", "Speech", "Spoken Word" or
  "Other" dropped, any case; the rest left to `CleanLabels`; dhowden's then ffprobe's, ffprobe's when
  it gives any, in the `scanned` snapshot) and the ABS file; tags only from the ABS file and edits.
  `subtitle`, `publisher`, `language` (<= 64 characters) and `abridged` (`"true"`/`"false"`/`""`,
  stored 1/0/NULL) are plain override fields from the ABS file, a match (`matchrun.Propose`: the work's
  subtitle and language, the recording's publisher; community genres are not offered, a match
  candidate carries none) or an edit. A source's abridged false says nothing (`AbridgedClaim`: ABS and
  the community write false for every book nobody marked), so only an edit says unabridged.
  `refreshEffective` writes the lists to `books.genres`/`tags`, to `book_labels` by key
  (`writeLabels`, beside `keyOtherSeries`) and to the FTS `genres`/`tags` columns (a label a line), so
  a search finds a book by a label. `labelFilters` (in the library, `book_labels.library_id`; any
  library for an admin list across them and for the library facet) is the player's `GET
  /libraries/{id}/books?genre=&tag=` (one value each, any case; both narrow) and the admin
  `BookFilter.Genre`/`Tag` (`?genre=`/`?tag=`); `BookFacets` counts `genres`/`tags` (the 100 most
  common by key, then the one the list is filtered by when it is rarer, each named by the spelling
  most of its books use, `seriesName`; a semi-join over `book_labels`). A bulk edit (and the book PATCH) takes `add` / `remove` (`{"tags": ["Owned"]}`:
  field -> labels) besides `set` / `revert`: `labelSets` merges them into each book's own EFFECTIVE
  list and writes the result as an override of the field only where it changes the list; a book the
  edit changes nothing on (adding a tag it has) is left as it is, its row not even rebuilt (`editTx`).
  A field can't be set or reverted and added to or removed from at once, nor one label added and
  removed (`normalizeLabelEdits`).
  Books indexed before `0049` get their genres from the next scan (`genres_checked`, the primary
  file read once beside 0037's date read, `Catalog.SetGenres` writing the snapshot and re-resolving,
  flushed with the path readings) and the ABS fields from its re-read (`abs_indexed_at` NULL), no
  re-index. Path-keyed like every override, so moves and joins carry them unchanged.
- **Series spellings (`catalog/moreseries.go`)**: a series is every name with one
  `seriesKey` (`match.SeriesKey`, the rails' key: case, accents, punctuation and spacing
  aside; a name with no letter or number is its own key), so "He Who Fights With Monsters",
  "...with..." and "...WIth..." are one series and nothing fuzzier merges ("The Expanse" and
  "Expanse" stay two). `catalog.Series` groups by it (counts, positions, lengths and authors
  merged; `SeriesList` and `normalizeMoreSeries` keep a series once however a list spells it again) and names the group `seriesName`: the spelling its
  matched books use most, else the most books', ties alphabetical. **The key is stored**
  (migration 0041): `refreshEffective` writes `books.series_key` (`seriesKey`, '' for no
  series; index `idx_books_series_key (library_id, series_key, series, series_index)`, which
  replaced `idx_books_series`: spellings adjacent, so Health's `series_spellings` is two range
  seeks per book) and the book's
  other series to `book_more_series` (`keyOtherSeries`: `SeriesList` less the main series, one
  row per key with its position; PK `(book_id, series_key)`, index `(series_key, library_id,
  position)`, cascades with the book). Both are written by Go, never computed in SQL (the key
  needs NFKD; the catalogue's SQL functions stay out of the schema), and every write of
  `books.series` ends in `refreshEffective` (`UpsertBook` runs it in the scan's own transaction),
  so every path that changes a series keys it. Every series
  filter asks by key: `seriesFilter` (an `id IN` over `idx_books_series_key` in every library,
  with memberships a UNION ALL of the list rows too; the caller's library term narrows them),
  `seriesOrderKeys` (the key as a literal), `numberedSeriesMembers` (the next book),
  `SeriesBooks`/`seriesMembers` (the rails) and `positionIn` (Go, by key), so the SQL and a
  keyset cursor agree; each is an index seek (`TestNextInSeriesPlan`; about a millisecond for a
  25-book series in 50,000 books, no slower than in 5,000, `TestSeriesKeyScale`). Rows indexed before 0041 are keyed by
  `Catalog.KeySeries`, which the launcher runs synchronously before the listener whenever the
  stored keys are missing or out of date (server_state `series_keys` holds `seriesKeysVersion`:
  the key rules' number, bumped when `seriesKey`/`match.SeriesKey` changes, plus Go's and
  x/text's Unicode versions, so a toolchain upgrade re-keys); about a second per 50,000 books, and
  a failure stops the start, as a migration's does (the next start tries again): it first puts every stored `more_series` override through
  `normalizeMoreSeries` (a list written before #123 could name one series twice) and rebuilds
  those books, then keys every book in one transaction. The JSON
  is unchanged: an older player sees fewer, fuller series. **A position from the title**:
  `bookLayers.scannedFields` gives a book with a series and no position the one its title gives
  (`titlePosition`, `metadata.PositionInTitle`), read as the title's source (path when the title is
  the path's, else tag); an edit or a tag's own position wins. Rows indexed before the rule get it
  from `Catalog.PlaceByTitles`, which the launcher runs in the background at start, once per database
  (server_state `title_positions`): the books with a series, no position and a digit in the row's, the
  snapshot's or the path's title (a match may have replaced the row's) whose resolve gives one,
  re-resolved by `refreshEffective` in batches of 250.
- **Admin catalog API** (`api/handlers_catalog.go`, admin-only, transport-only):
  `GET /admin/books` (keyset over the named orderings in `catalog.adminSorts`, sorted and
  paged on ids before the per-row columns are computed; the cursor names its ordering;
  filters in `catalog.BookFilter`, unparseable ones 400; `media.DirectPlayableSQL` is
  `DirectPlayable` in SQL) + `/admin/books/facets` (each dimension counted without its
  own filter, the unfiltered yes/no ones in one pass);
  `POST /admin/books/bulk` (one edit over <= 1000 books, all or nothing);
  `GET /admin/authors|narrators` (each person a credit names, `catalog.creditNames`
  over `names.Split`, so a co-credit counts for both; `merge_suggestions` stay over
  WHOLE credits, from `personKey`, keyed by `match.Fold`, which keeps every script's
  letters, with `other_books` = the books a merge rewrites) and
  `/admin/series` (`catalog.People`/`Series` with a nil scope; the player's
  `/libraries/{id}/authors|narrators|series` take the same aggregates within the
  caller's scope, without `merge_suggestions`); `GET`/`PATCH /admin/libraries/{id}/book?path=` (book
  page: per-field provenance, chapters, files, listeners, shares, folder override, and
  `match_query`, the match dialog's search box prefill set by the api: `meta.SearchPrefill`, the
  title + author, except that swapped tags (the title is the author folder, or the cleaned author
  is the path's title; never a tag that matches its own path fact, "Dune/Dune") or a junk title/author
  (`metadata.IsGenericTitle`, "Unknown", "Various Artists")
  take the folders' facts, `metadata.FromPathLayout`);
  `GET /admin/libraries/{id}/book/match?path=` (`meta.Service.Candidates`: metaserve's
  STRUCTURED match `works/match`, then up to 6 works expanded, uncached, bounded by
  `workSem` via `fetchWork`. The match gets the book's facts separately
  (`matchParams`): the path's LAYOUT (`metadata.ReadPathLayout`: disc/track folders dropped,
  top folder = author, holding folder = series, leaf = a title guess sent RAW -
  metaserve is the one place that reads `SW06 - `/`Sharpe - 08 - `/`02. `
  numbering and takes the position from it) then the tagged title, path + tag
  authors, the series folder AND the tagged series when they differ (folder first;
  metaserve judges each, the better counts), the tagged position (only when no
  DIFFERENT series folder goes up: one position applies to every series guess and
  replaces the leaf's numbering), runtime, `?q=`, and the asin/isbn (typed;
  the book's only when nothing was typed; a book with neither sends its tag hints, below), which
  metaserve looks up itself, plus `language` (the book's, else its tags' (`meta.NormalizeLanguage`),
  else its library's: `matchrun.LibraryLanguage`, DERIVED from `catalog.LanguageCounts` (one grouped
  read, once a library per run, once per dialog request for a book saying none: `matchrun.LibraryLanguageOf`): the language at least 90% of the books
  saying one agree on, at least 20 of them, spellings read as one 639-1 code (`languageCode`), else
  none, as a wrong guess scores the book's own work as another language's. Never the marketplace's: `metadata.region` names the store whose ASIN a match takes, not
  what language a library is in, and with it `de` on a mostly English library the benchmark's
  auto-right halved and German works were auto-applied to English books. `NormalizeLanguage` cuts a
  tag to the first of several and its primary subtag and reads a placeholder (`und`, `xxx`, `N/A`) as
  none, the names and 639-2 codes are metaserve's to read) and up to 4 `narrator` (the narrator
  credit's `names.Split`, less a name that is the author, mostly a composer tag: `matchParams`), both
  set by `matchrun.QueryFor` and ignored
  by an older metaserve; candidates carry metaserve's `score` and `reasons` (`narrator`, `language`
  passed through too), best first, an identifier hit first at one score (metaserve breaks a tie on
  the title). A typed `?asin=`/`?isbn=` with no text is a plain `lookup`.
  Fallback, FROZEN until the production metaserve serves `works/match` (then delete
  it): when the route is missing (404 through `works/{id}`, 405, or a 200 without
  `results`) `searchHits` runs the old `lookup` + `works/search` unchanged (typed
  text, else `CleanTitle` + author) scored by the tag-only `scoreCandidate`, and
  the missing route is remembered for 15 min (`matchUnsupportedUntil`); a 503/5xx
  from works/match is an outage (502), never a fallback; identifiers normalized for
  the exact lookup; metadata off -> 404 `metadata_off`). Each recording keeps its
  ASINs' marketplaces (`asin_refs`) and `asins` is ordered by `metadata.region`
  (`orderASINs`: the preferred store's, then `us`, then the rest; metaserve lists
  them by region, so without it `au` beat `uk`; the ASIN an identifier hit was found by leads its
  marketplace's, `toCandidate`, so a book the store lists under two keeps its tags' when that store
  is the preferred one), `asin_region` names the first's;
  `meta.DefaultRecording` (never a dramatization for a book that doesn't state it is one (its text, folders, author or narrator credit, or a genre such as "Radio Drama" or "Hörspiel": `statesDramatization`), unless an identifier or a narrator names it, or the work holds only dramatizations and one is within max(1%, 2 min) of the book's length (`lengthSays`): `isDramatization`, `MatchCandidate.PlainBook`, a run's `dramatization_only`; identifier's, else among the recordings one of the book's narrators
  reads, `ReadBy`: a name folding alike or every word of one in the other, either way round, that
  fit its length (`PlausibleRuntimeOff`; all when none does) the closest runtime, the
  preferred marketplace breaking a tie) sets each candidate's `default_recording_id`, which
  the dialog starts from (the console has no copy of the rule). A match run then takes
  `meta.FitRecording`: a recording more than 10% off the length that none of the book's
  narrators reads (a tag hint's, metaserve's guess) gives way to one of the work's that fits
  (one a narrator of the book reads stays, even beside that narrator's recording that fits: server#325),
  a plain book's never a dramatization DefaultRecording's rule refuses (the hint named only its
  own recording); never in a refresh or a repick, whose identifier is the book's own. **Bulk matching**
  (`internal/matchrun`, `handlers_match_runs.go`, migration `0033`):
  `GET/POST /admin/match-runs`, `GET /admin/match-runs/{id}[/items]`,
  `POST /admin/match-runs/{id}/apply|cancel`. One run at a time (`matchrun.Runner`,
  its own goroutine under `baseCtx`, NOT the scan job queue: it waits on the
  network, and `library` must not import `meta`); 6 workers (an apply 2), `maxFailStreak` (10) failures in a row
  stop it (`metadata_unavailable`; past the workers, as one moment's failure meets them all at once):
  the run is `ready` with `error` `metadata_unavailable` (server#306: before, `failed` with no
  review; a run recorded then stays so), the books it answered kept to review and apply, the console saying it stopped early and how
  many it checked; the books it never reached are still unmatched (a refresh or repick re-checks them
  anyway), so the next run takes them. `metadata_off` and `internal` stay `failed`. Metadata
  off stops it (an apply too: back to ready, as a cancel; every endpoint, cancel
  included, 404s `metadata_off` then). What a run costs the service: a match sets
  `MatchQuery.BestOnly` and `NoDetailsBelow` (`ReviewFloor`), which the dialog never does, for
  `expand=work` on works/match (one wait a book: `bestCandidates` reads `work` and `series_facts`
  off the result `compareMatches` calls best, an identifier hit winning a tie, so maybe `results[1]`;
  the facts fill the `s:` cache). It reads the whole of `cands[0]` and of `cands[1]` only `Score` and
  `IdentifierHit()` (`Classify`), so every other candidate is a stub. `work` absent on the best (an
  older metaserve, its budget spent) is the fetch path for the best alone, and nothing for a weak
  best no identifier found; a best that is gone (404) gives way to the next, but one that can't be read
  for any other reason is an outage at once (server#306), unlike `Candidates`: the next standing in
  would be the only candidate `Classify` sees, so a weaker work could apply unasked, or a stub answer
  for a service that is down; the book is an `error` item. The inlined
  facts are read as a fetch's are (`factSeries`, `inlinedFacts`). A
  `Limit: 2` still applies; refresh and repick are identifier lookups, never works/match. Each run
  has a `meta.Pacer` in its context (`WithPacer`, `maxRequestsPerSecond` 60 against the service's
  100, no saved-up burst), which `pacedTransport`, the remote leg's transport, spaces requests by,
  so a mirror's answers take no slot and whatever fails over to the remote service does. A run takes no
  `workSem` slot (`workSlot`): its Pacer and workers bound it, and a Retry-After it sits out must not hold up a
  listener's read or the dialog. Concurrent misses of one series share one read (`seriesFacts`). In a run a
  503 is waited out like a 429 (`getJSON`: Retry-After, `rateLimitRetries`, `maxRetryAfter`, not past
  the deadline), each wait logged at INFO with the run's total; outside one a 503 is an outage at
  once (the dialog's 502). `getJSONOnce` reads every answer to its end (`maxDrain`), and
  `maxIdleConnsPerHost` keeps connections for the workers over HTTP/1.1. A run only records (`match_run_items`: outcome auto/review/none/error,
  the best candidate's `catalog.MatchProposal`); apply writes community overrides
  via `EditBook` and the cover via `saveMatchCover` (the dialog's fetch + keep
  checks) per `matchrun.Plan(scope)` (ids|fill|overwrite; never an `edited` field,
  never a position beside another series, a cover only for a book with none), which
  also computes each item's `changes` for the review, so the two can't disagree. `Plan`
  returns `Writes` (set, revert, cover, anchor: the match to record, see Recorded community
  match below): overwrite REPLACES a book's series with the
  community's for its recording (the series rules), reverting a `series`/`series_index`/
  `more_series` value the community wrote earlier that it no longer offers (a series it
  deleted or merged, an edition or translation the rules leave out) back to the files'
  (a position only beside the files' series, other series only when none are offered);
  fill and ids never revert; an admin's own series stay, and a community edit's revert
  deletes only community rows (`editTx`), so an edit made since the run looked survives
  a revert (a set still overwrites it, as the match dialog's ticked edited field must).
  `Writes.after` is the one rule for a field's value once written (both `planSeries`
  and the review use it). A refresh records only books a scope would change (as a
  repick records only books with a new ASIN), so a clean library leaves no items. A refresh
  proposal names the identifier it was found by (`found_by`, "asin:..."/"isbn:...", or
  "work:<work id>[/<recording id>]" for a book looked up by its recorded match, a fallback's
  followed by the identifiers whose lookups missed: below), as a
  repick's names the ASIN it looked up, and applying either is a no-op for a book that no
  longer has it (matched again since, to another record or another recording of the work:
  `stillFoundBy`; a repick recorded before 2.4.0 has none), except a refresh whose heal went in
  before a stop cut its cover short (`healedTo`: the book's match is already the proposal's;
  a fallback's identifiers must still agree too).
  Likewise a match run's book that is matched now (`matchedSince`: an ASIN or ISBN, or a recorded
  community match; matched by hand from the review or anywhere, or a rescan found one) has no changes, reads `matched_since`
  in the review and is skipped with detail `matched_since`, under any scope; a match whose
  fields went in before a stop cut its cover short stays to apply, detail `cover_pending`, its
  ASIN and recorded match the run's own rather than ones matched since (unless the book was
  matched again by hand since, to another record), so the next apply takes the cover; it never
  records the match again, and one unlinked since (no recorded match now) writes nothing, its
  cover included: the Unlink took back the fields and cover with the match. The review's
  items page best match first (`score DESC, id`), the cursor still an item id whose score
  the next page reads on from.
  Each scope's change carries `series` {from, to} (main first, position 0 = none) when it
  changes the book's series, which the review shows as "old -> new". A retired work slug
  is a redirect the client follows, so the names written are the merged record's.
  A community position that is a range ("1-3") is written as one (`series_index` "1-3", a
  `more_series` entry's `position_end`); one the field refuses (a span over 100) is none.
  **Recorded community match** (LibraForge E6, server#320, `0069` `book_matches`: `library_id, path,
  work_id, recording_id, how (auto|accepted), run_id, set_by, set_at`, PK `(library_id, path)`, no FK to
  the index or the runs): the work (and recording) a book was matched to, whether or not the match gave
  it an ASIN or ISBN. `/meta` answers from it when the book's identifiers don't match
  (`EnrichMatch`, see Community metadata lookup). Written only by a community edit, in its own
  transaction (`catalog.BookEdit.Match`, `MatchAnchor`; `EditBooks` refuses it): the match dialog's
  accept (`PATCH .../book` `match: {work_id, recording_id}` with the ticked fields, or alone for a book
  with no ASIN or ISBN whose pick isn't its recorded match; the dialog starts from the recorded match
  when the search lists it; how `accepted`; refused in a bulk edit) and an apply (`Writes.Anchor`, which `Plan` sets under every
  scope when the proposal's work or recording is not the book's: how `auto` for a confident item,
  `accepted` for an included review item, a refresh's heal keeping the book's). An empty work id
  (`{"work_id": ""}`, from any edit; the book aside's Unlink, behind a confirm) unlinks the book
  (an id of only white space or other invisible characters (`catalog.invisible`: format characters
  such as U+200B/U+FEFF/U+2060, variation selectors, default-ignorables such as U+3164, U+2800;
  both ids are trimmed of them at the ends, interior ones kept), or a `match` naming no work, `{}`
  or a null `work_id`, is invalid, 400 `invalid_override` on field `match`, never an Unlink):
  `ClearCommunityMatches` for that one book (`clearCommunity`, in the edit's transaction, before the
  rest of the edit), so its community values and cover go with the row and the next run asks about it
  again (removing only the row would leave the values to match it to the same work); an admin's own
  edits and uploads stay. Unlink is refused only while a run that covers the book's library (or
  every library, `library_id` NULL) is applying (`catalog.ErrMatchRunBusy`, HTTP 409
  `match_run_busy`, "a match run is applying to this library"; stop it or wait for it to finish;
  server#341). A run that is only matching records items and doesn't refuse it, nor does a run
  over another library: every apply checks each item against the book as it is then (a refresh
  item no longer has what it was found by, `stillFoundBy`; a repick's community ASIN is gone; a
  match item reads `matched_since` or, as `cover_pending`, writes nothing), and a cover goes only
  to the book it was planned for (below). The check and clear share the edit's writer
  transaction, serialized with `StartMatchRun` and `BeginApply` (one that commits first is seen;
  one after reads the unlinked book). Applying stays busy through the last worker's cover write,
  even after a stop request (`FinishApply` runs only once every worker has returned); ordinary
  field edits still work. A run left marked applying with nothing behind it (below) is settled
  and the Unlink tried once more (`handleAdminEditBook`: `Runner.SettleOrphans`).
  **A community cover is written conditionally** (server#340): an apply's cover goes through
  `catalog.SetCoverIf` with a `CoverExpect` (`matchrun.coverExpect`: the match the apply records,
  else the book's own, and the ASIN and ISBN after its edit, `Writes.after`, in stored form);
  in the write's own transaction the book must still have no cover (`hasCoverExpr`: no custom
  cover and no art of its own, as the plan found it), that recorded match and those resolved
  identifiers, else `catalog.ErrCoverSuperseded` and nothing is written.
  `saveMatchCover` asks `CoverExpected` before it fetches, then `SetCoverIf` after, so an upload,
  art a rescan found, a match accepted by hand, an identifier edit or an Unlink while the cover
  was fetched keeps what the book has. The item reads detail `cover_superseded` (`DetailCoverSuperseded`:
  applied when its fields went in, else skipped; final, never turned into `cover_pending`).
  `SetCover` (uploads, the dialog's explicit cover tick) stays unconditional.
  No Undo: the cleared values aren't kept. A store listing the
  community holds with no ASIN or ISBN (Guild books) is matched by this row alone: `catalog.matchedExpr`
  (`b.asin <> '' OR b.isbn <> '' OR EXISTS(book_matches by its key)`) is the list's `matched` and its
  facet, Health's Not matched (so `UnmatchedBooks`, a run's books), Activity's `identified` (the wire
  name stays), the series' naming and a refresh's `MatchedBooks`. What needs an identifier tests
  asin/isbn itself and stays identifier-only: community chapters, the unknown-ASIN report, upcoming,
  a repick (`CommunityASINBooks`), the Bridge's estimate, `/next`'s community rail and the console's chapter check,
  Series rail and Suggest corrections. `/meta` reads the row when the book has no identifier or its
  lookup is a not-found (`meta.Service.EnrichMatch`, see Community metadata lookup). Path-keyed state: a move carries it with the book's edits as one
  set, a join copies the first part's, a rescan or rebuild keeps it, `ClearCommunityMatches` drops it
  (counted in `books`); the book page carries it as `community_match {work_id, recording_id, how,
  matched_at, web_url}` (omitted when none), a review's change as `match` (the console lists it when no
  ASIN/ISBN is written beside it). The console's Community card shows a recorded match whenever the
  book has one, with or without an ASIN or ISBN (work, recording, how and when, Unlink; server#338); its
  Unlink confirm says the book stays matched when it keeps an identifier of its own (one not from the
  community, or one its files name). No backfill: a book matched before E6 has none until matched again.
  Confident = score >= 90, >= 10 ahead, with an ASIN/ISBN, and the recording within 10% of
  the book's length when both are known (`meta.PlausibleRuntimeOff`, `lengthFits`), or within 20%
  when the book's narrator reads it (`meta.NarratedRuntimeOff`, `narratorReads`: metaserve's
  `reasons.narrator` is `full` and the proposed recording's credit names them, `readsRecording`
  over `meta.ReadBy`; in a refresh, whose lookup sends no narrator, the credit check alone
  (`readsRecording` with the book's own narrator credit), so a book a run applied on its narrator's
  say-so stays up to date; a placeholder credit ("Full Cast", GraphicAudio, "Various", "Unknown":
  `meta.PersonNarrator`) names no reader and never widens it, nor does a plain book's reader in a
  dramatization's cast (`meta.DramatizesPlainBook`: another production; a refresh by recorded match
  too: the recording it names passes `DefaultRecording` as an identifier's does, `PlainBook`
  unchanged); a review
  item whose recording doesn't fit carries detail `length_differs`, which the review words with
  both lengths). An identifier hit (`reasons.identifier`: the ASIN/ISBN the query sent found it, in
  a run always a tag hint) needs no lead unless the runner-up is one too (review; a guard, as metaserve
  names one work per request today), but only while the book agrees with it (`identifierAgrees`,
  server#306: a tag ASIN can name another book, Chamber of Secrets carrying Philosopher's Stone's
  scores title 0.62 and series `conflict`): metaserve's `reasons.title`, when judged, at least
  `IdentifierTitleAgree` (0.65), and with `reasons.series` `conflict` a title of at least
  `IdentifierTitleSame` (0.9; none judged, a title that is only the series' name and number, is held),
  as the same title at another position is the series numbered another way (The Witcher counting its
  story collections, Halo, a chronological order). Else review, detail `identifier_disagrees`.
  Calibrated on the LibraForge benchmark (the highest title value holding at most 0.2% of its right
  automatic matches: 3,806 auto-right / 4 auto-wrong before, 3,799 / 4 after, none of its wrong ones
  an identifier hit); a series conflict alone held 19 right ones (the renumbered series) and nothing
  wrong, hence the title beside it. With neither ASIN nor ISBN to attach (a store listing), confident
  is firmer (`anchorFits`, LibraForge E6): score >= 90 and a plain 10-point lead (no identifier-hit
  lead, no narrator widening), metaserve's `reasons.author` `full` and `reasons.series` `position`, and
  both lengths known with the recording within `AnchorRuntimeOff` (2%), an identifier hit (scored 100
  whatever its title) also agreeing (`identifierAgrees`; none on the benchmark); the apply records the
  match (`how` auto). On the LibraForge benchmark this took 186 books from review to confident, none wrong.
  A match run's best candidate under `matchrun.ReviewFloor` (60)
  on metaserve's scale is no match, detail `weak_match`, its score kept and its proposal empty (the
  dialog still lists everything; an older metaserve's `scoreCandidate` stays for review). Fill (the
  default apply) also tidies a
  title that is not an admin edit (`matchrun.tidyTitle`): when the file title, less the community
  title, its subtitle, the series names (community's and the book's, as `match.SeriesForms` spells
  them), clutter (book/vol, unabridged, litrpg, ...) and the numbers the book's positions or the
  community title state (`statedNumbers`: digits, ordinals, words, roman numerals), leaves nothing,
  the community title is written; any other word or number keeps the file's ("part" names a split
  of the book, "abridged" another edition, "Book 2" of a work at position 1 another volume), and a
  title of the community's words and nothing more only when it is in one case. The review lists it
  like any planned change.
  **Tag identifiers are hints** (`0067`: `books.tag_asin` / `tag_isbn` / `tag_language`, read from
  `asin` / `audible_asin` / the iTunes freeform ASIN / an ID3 TXXX ASIN, `isbn`, `language` / TLAN in
  both tag passes, an ASIN or ISBN only in its proper form (`match.ValidASIN`/`ValidISBN`: `N/A` beside
  a real tag or a repeated tag's joined `A;A` still reads the real one), `hints_checked` backfilled
  once like `genres_checked`, waiting while ffprobe fails; never on the wire, only `GetBook` and a
  run's subjects read them): a
  book's own ASIN/ISBN is never set from its tags. Why: in the LibraForge benchmark 272 of 1,677
  tagged books (16%) carry an ASIN that is not their confirmed listing (another store, another
  edition, or wrong); a stored ASIN takes the book out of match runs, and a refresh never re-matches
  it by title, so a wrong tag would lock the book out of matching; and every community feature hangs
  off the ASIN. So `matchrun.QueryFor` sends the hint as `BookASIN`/`BookISBN` when the book has
  neither of its own: a hint the community knows whose length fits is an identifier hit (confident
  while the book's title and series agree with it, above) and becomes the book's ASIN only by an apply; one it doesn't know costs nothing. An
  Audiobookshelf `metadata.json`'s ASIN differs: ABS wrote it after a match, so it stays an
  identifier. The matcher's benchmark harness is `internal/matchrun/benchharness_test.go` (build tag
  `benchharness`, never in CI; its scan hooks `metadata.ProbeTagsForBench` and
  `library.SetNamesForBench` likewise): the LibraForge books through the run's own `matchBook`
  against a local artifact in mirror mode, or through HTTP with `LF_BASE_URL` (the run's workers and
  pacing, its time reported, the right answers read afterwards from the artifact so a proxy counts
  only the run's requests), counted per outcome and judged by the reference ASIN, else (a reference
  with no ASIN) LibraForge's hand-checked pick, else v2.5.1's audited verdict on the same work, else
  title and author, each its own bucket (run line in its doc comment); the title-only judge
  (`sameBook`, in the untagged `titlejudge_test.go` so its tests run with every `go test`) also takes a
  work at the position the reference title states whose title agrees without that number, "Book"/"Vol."
  or bracketed notes ("Rise, My Minions! 3" = "Rise, My Minions! Book 3"). `LF_DIAG=1` adds each book's
  diagnostics to `LF_OUT` (`diag`: identifier, `lengthFits`, the query, the run's candidates and the
  copy's top 10 with reasons, series and runtimes, the right work's rank). A
  repick only touches an ASIN whose override is `source=community`. A **refresh** run
  (`mode: "refresh"`, `catalog.MatchedBooks`: every matched book) looks each
  book up by its own identifier first (the ISBN when the ASIN finds nothing) and takes
  that record as confident, subject to the recording's length guard (`lengthFits`, above:
  otherwise held for review). A successful identifier lookup keeps precedence even when
  it names a different work from the recorded match or its recording is held for review.
  Only when every identifier lookup returns no candidate and neither nonempty identifier
  is an admin edit (`BookMatchState.Fields`, `source=edited`) does refresh fall back to the
  recorded work; tag/scan/community identifiers still allow it, and an outage stays an
  error, never a fallback. With an admin-edited identifier or no recorded work, those
  misses remain `none` / `identifier_unknown`, never a fuzzy match by title.
  A book with neither identifier is looked up by its recorded work directly (`meta.MatchQuery.WorkID`:
  works/{id} alone, a retired slug's 301 followed online and in mirror mode, its recorded recording
  kept while the work lists it, `found_by` `work:<work id>[/<recording id>]`, `stillFoundBy`
  against the anchor's work and recording; a 404 is no match, detail `work_unknown`, the anchor
  KEPT). The fallback uses the same lookup, recording choice and length guard, and heals
  the match and refreshes its details under the selected scope. Its `found_by` appends
  `;asin:<ASIN>;isbn:<ISBN>` to the work source, including empty values: both identifiers
  and the recorded work/recording must still agree at apply, with neither identifier an
  admin edit (`planFor` checks the identifiers and their sources,
  `splitFallback`, before `stillFoundBy`). A stopped apply that already healed the match can
  resume its cover only while those identifiers still agree (the same check, then `healedTo`).
  A book with neither identifier may fill an ASIN/ISBN its recording gained, never the one its
  recorded match offered (server#330, `0070`: `book_matches.offered_asin` / `offered_isbn`,
  `MatchAnchor.OfferedASIN/ISBN`, the identifiers the match offered when recorded, ticked or
  not, in the fields' stored form): the book has neither, so the admin unticked it in the match
  dialog or reverted it since, and `planRefresh` drops it (such a book records no item). One the
  recording gained since (another value) fills, and the apply then records it as offered (the
  anchor rewritten with the same work and recording, so the review's `match` is true beside the
  identifier), so a revert of that one stays too. Written by `Plan` (the proposal's, written
  or not), the dialog's accept (`PATCH .../book` `match: {asin, isbn}`: the match's identifier
  whether a checkbox or already the book's; one the field would refuse is 400 on field `match`;
  an Unlink ignores them; a cover-only accept of the book's recorded match, work and recording,
  sends it too, `isRecorded` in match-dialog.tsx, while another work beside the book's own ASIN
  or ISBN still sends none), a heal or a refresh's identifier fill (each identifier the
  proposal's when it names one, else the match's own) and a repick of a book with a recorded
  match (`planFor`: the anchor kept, its offered ASIN the one the repick writes);
  copied by a join, moved with the book. A refresh's or repick's anchor write keeps the
  match's `how` and is conditional (`BookEdit.MatchIf`, the anchor the apply read: work,
  recording and offered identifiers, checked in the edit's transaction; else the anchor is
  left and the rest of the edit applies), so a match made by hand mid-apply stays. No backfill: '' (a match recorded before 0070, or an
  accept from an older console) fills as before.
  The anchor is only healed (`planRefresh`): a merged work's new slug is a change by itself (`match`),
  a book matched by its own identifier with no anchor never gets one. It fills or
  overwrites the details but never writes an identifier for a book that has one (an
  identifier lookup's proposal carries none, and `planRefresh` drops a by-work proposal's
  once the book has one of its own; a fallback's proposal also carries none).
  Unlink removes the recorded match and community values.
  `matchrun.ScopesFor` (the one statement of which scopes a
  mode takes) allows only fill/overwrite, a refresh being for the details (else `ErrScope`, 400); audited
  `book.match_refresh`. The server leaves out a refresh item with nothing to change under the
  scope (server#332), in two ways. One that had nothing to change under it when the run
  recorded it (`0070`: `match_run_items.unchanged`, the scopes whose `planRefresh` was empty,
  `MatchRunItem.Unchanged`, never on the wire) is never read: `MatchItemsToApply(scope)`
  filters it in SQL, so an empty refresh's Done starts at `apply_total` 0, and a book that
  changed since so planning now would write something the review never showed is left
  alone; the review (`Runner.Items`) shows no change under that scope either, so its
  `leftOut` count and the apply agree. One that had a change then but none now: `applyItem` answers the internal
  `detailUnchanged` for an empty `planFor`, and the apply counts these up and drops them with
  `catalog.DropApplyItems` (`apply_total - n`, never below `apply_done`), `dropBatch` (100) at
  a time and the rest before the run settles, instead of marking each. Either way the item
  stays `applied` '' as an exclusion leaves it (pending, not skipped), and `apply_done` ends
  at `apply_total`. That includes one whose book no longer has what it was found by. An item
  recorded before 0070 (`unchanged` '') is planned as before. A match or repick item with nothing to change is still `skipped` /
  `nothing_to_change`. The review counts refresh rows with no changes under the selected
  scope (a healed recorded match counts as a change) as left out (`picksForScope`'s
  `leftOut`, `applyCount(run, picks, leftOut)`) and never sends them, reading all confident
  pages with a separate `useAllMatchRunItems` query (200 per page, no focus refetch)
  before enabling Apply; the shown list keeps manual pagination. Gone items are still
  excluded, and an admin's own exclusion stays (counted once). With no picks, Done
  finishes the run through an apply with only the admin's own exclusions: the server leaves
  out every item recorded with no change under the scope unread, and plans the rest again,
  so it changes no book the review showed no change for. Apply waits only for the run list to refresh, not
  the loaded item pages.
  Apply caps manual includes at 20,000 ids; exclusions use the existing 1 MiB
  request-body limit (an older console still sends every unchanged book).
  `InterruptMatchRuns` (launcher, at
  start) settles runs a stopped server left: matching -> interrupted, applying ->
  ready (`catalog.SettleMatchRuns(ctx, 0)`, one transaction; a failure is a warning). A run
  whose finishing write failed (`FinishMatching`, `FinishApply` only log it) or that the start's
  settle missed is settled too (server#339): `Runner.settleLocked` at the top of `Start` and
  `Apply` (so a stuck applying run is ready, and can be applied, again), `Runner.SettleOrphans`
  on an Unlink refused as busy, and Stop (`Runner.Cancel` on a run that isn't the active one:
  `catalog.SettleMatchRun`; `POST .../cancel` 204, audited `book.match_stop` with `"orphan":
  true`; 409 `match_run_not_running` only for a run not marked working). Safe only under the
  runner's lock, keeping the active run: `Start` and `Apply` hold it across `StartMatchRun` /
  `BeginApply` and `begin`, and `end` clears the active run only after every worker, cover
  writes included, has returned, so no settled row has a worker behind it. Newest 10 runs kept. `DELETE /admin/community-matches[?library_id=]` (`matchrun.Runner.Clear`,
  409 `match_run_busy` while a run works; works with metadata off) undoes the community matches so
  books can be matched from fresh: `catalog.ClearCommunityMatches` drops the `source=community`
  overrides and covers and the recorded matches (`book_matches`), rebuilds each book (`refreshEffective`, `refreshCoverArt`) and drops the
  runs (a library's clear: its runs, and its items in runs over every library), one transaction;
  edits, uploads, tags and `book_enrichment` stay. Audited `book.match_run|asin_repick|match_refresh|match_apply|match_stop|match_clear`;
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
  fall back to `series_index`, then to an unheld entry its title is (`sameTitle`: numbering prefix
  and a trailing bracketed part ignored) (display only: no book is changed). A card's rail
  (`pickRail`) is the one named like the series (or one of its `orderings`), then one named like
  it give or take an article or a trailing "Series", then the rail holding >= 2 and more than half
  of the series' resolved works (never the rail named like the series' name before a colon or a dash: the parent of a
  sub-series; ties to the smaller rail), else none: no gaps rather than another series' (the rail
  comes from one matched book's `/meta`, so a work in more than `maxSeriesRails` real series can
  still lack one);
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
  **Community covers** (`api/handlers_community_covers.go`): a match candidate's `cover_url` is
  an image on its own host (Audible's CDN, Open Library), which the console's CSP can't load, so
  the server fetches it (`meta.Service.FetchCover`: http(s) only, every connection's resolved
  address checked public at dial time, redirects included (`netguard.PublicAddr` through
  `netguard.Dialer`, shared with podcast discovery; no proxy), <= 3
  redirects, 15 s, `maxConcurrentCoverFetches`, never holding `coverReads`).
  `POST /admin/meta/covers` (`{urls, size}`, <= 12) returns thumbnails as `data:` URLs in
  order (`""` = couldn't be fetched or decoded; cached in the thumbnail cache by URL, a failed
  fetch not; each holds `communityReads` from its fetch to the end of its decode; asks for the
  same thumbnail at once share one fetch, `communityFlights`); `PUT /admin/libraries/{id}/cover/community?path=` (`{url}`) keeps one as the book's
  custom cover through `saveCustomCover`, the upload's own path, once `keepableCover` has read its
  header (413 past `media.MaxThumbnailSourcePixels`), re-encoded within 1600 px first when it is
  over 5 MiB (audited `book.cover_set`, `source: community`); 400 for a URL
  that isn't http(s), 502 `cover_unavailable`, 413/415 as an upload. Both 404 `metadata_off`
  while metadata is off (`metadataOff`). Tests swap the fetch through `API.fetchCover`.
  **The player's community covers** (rail entries, "Previous books", the recording) come
  through `GET /libraries/{id}/meta/cover?path=&url=&size=96|160|320|640` (`handleMetaCover`,
  capability `meta_covers` = `metadataOn()`; media auth, so `?token=`): the web player's
  CSP takes images only from the server, which stays so (`TestHTMLCSPImagesStaySameOrigin`),
  and a listener's device never contacts a cover host. Not an open proxy: the path is
  scope-checked like `/meta` (the same `bookEnvelope` lookup), and `url` must be one the book's cached envelope hands out
  (`meta.Enrichment.HandsOutCover`: the recording's or any rail view's entry, exact match),
  else 404 before any fetch. That beats a remembered-URL set (lost on restart, needs a
  bound) and a signed URL (a new wire field on every cover, a key to keep): the envelope
  is already cached in memory and in `meta_cache`, so the check is a lookup. Served only as
  a JPEG thumbnail through `communityThumbnail` (same cache key as the console's batch,
  `FetchCover`'s guards, `communityReads`), so upstream bytes and content types never
  reach the client; ETag `"community-<size>-<CoverVersion(url)>"`, `coverCache` (a day).
  404 `no such cover` (unmatched, untagged, URL not handed out), 502 metadata unavailable,
  502 `cover_unavailable` (fetch failed, not cached but remembered per URL for
  `communityRetryAfter`, 1 min, so a broken or hung host isn't fetched on every render; the
  console's batch ignores that and always retries), 404 `no cover` (not an image, over
  the pixel bound). It stays under the request timeout (`isStreamingPath` matches
  only `/libraries/{id}/cover|stream|ebook` exactly, `isLibraryMedia`).
  `POST /admin/covers` (`api/handlers_covers.go`, Phase 2b) is how the console shows
  covers: `{books:[{library_id,path}], size: 96|160|320|640}` (<= 60; 96 for a table row) returns JPEG
  thumbnails as `data:` URLs in request order (`""` = no art; each with the `version`, the
  `cover_version` its admin row has once the request's colours are recorded), resolved like
  `/cover` (custom, sidecar via `SafeJoin`, embedded; never on-demand indexing), from
  one `catalog.CoverSources` query per library for the whole batch. Admin book rows carry
  `cover_version`, and the console keeps the thumbnails it got in a bounded IndexedDB cache
  keyed by library, path, version and size (`admin-ui/src/api/thumb-cache.ts`), a thumbnail kept
  under the version the answer names (never under an older row's version that asked for it),
  cleared on sign-out.
  One request per page of covers instead of one per cover, no token in any URL,
  ~20 KB a cover instead of full art.
  `media.Thumbnail` refuses sources over `MaxThumbnailSourcePixels` from the header
  (decompression bombs), `media.ThumbCache` is a byte-bounded LRU keyed by the art's
  version (custom `updated_at`, file size + mtime) holding the raw JPEG (the admin
  batch base64-encodes it), and
  `coverReads` bounds the art being read or waiting to be decoded and `thumbSem` the
  decodes, both across requests. The player gets
  the same thumbnails from `GET /libraries/{id}/cover?size=96|160|320|640` (capability
  `cover_sizes`; one code path, `coverArt` + `coverThumbnail`; ETag = size + art version,
  304 on a match; custom `no-cache`, file art `max-age=86400`; any other size 400).
  `cover_version` (`catalog.CoverVersion`, a 10-char hash of `books.cover_art`) is set
  whenever a book is indexed: its custom cover's stamp, else the art file's own version
  (`catalog.FileArtVersion`: sidecar or first audio file, size and mtime, the scan's
  `Book.ArtVersion`), so a re-index of unchanged art keeps the version and its colour,
  and art replaced in place gets a new one; a scan that finds the audio unchanged still
  records a replaced sidecar (`SetArtVersions`, compare-and-set). `SetCover`, `DeleteCover`,
  moves and joins recompute it, keeping a file version the row holds; a row with none
  takes the index form (mtime, size, sidecar path) plus the removed custom cover's
  identity, so a version is never reused for other art (ADMIN-01). Every
  thumbnail (a cache hit too, a 304 revalidation included) records on a book what
  it lacks for the art it read: `cover_color` (`media.CoverPalette` on the scaled image:
  dominant bucket = `bg`, the most vibrant bucket nudged in HSL lightness to WCAG 4.5:1
  against it = `accent`, else none; `on_accent` white/black) and, with it, the art's
  own version as `cover_art`, so `cover_version` follows a sidecar replaced in place
  and equals the thumbnail ETag's hash; `catalog.RecordCoverColors` is
  compare-and-set on the identity it was read under and bounded (250 ms, detached
  from the request). Both are on the player `Book` JSON (`omitempty`). Admin book rows (list and book page) also carry
  `cover_color` (the Series spines) and `matched` (the `matched=` filter's rule, `catalog.matchedExpr`: an
  ASIN or ISBN, or a recorded community match, `book_matches`, see Bulk matching), and `POST /admin/shares/{id}/paths`
  also takes `{"rules":[...]}` (<= 1000, one transaction) for adding a selection.
- **Rate limiting by route class** (`rateLimit` in `api/middleware.go`, buckets in
  `api/ratelimit.go`, values from `config.rate_limit.*` read at start through
  `RateLimitConfig.Effective`: a rate or burst left at 0 is the current default and is
  never saved, so a changed default reaches existing installs), read off the
  handler `mux.Handler` picks: static files (`web.IsStatic`) are not counted; media
  routes (`mediaAuth`, behind `requireBookMediaAuth` for cover/stream,
  `requireMediaAuth` for `/meta/cover` and the Upcoming cover, and `requireFeedAuth`
  for the calendar feed, whose `mediaHandler` marks them) and the
  authenticated API routes outside `/api/v1/auth/` (`requireAuth`, whose
  `authHandler` marks them; `tokenKeyed`, server#246) limit themselves: refused
  while the address's general bucket is empty (`Ready`, checked in rateLimit too, so
  the refusal comes before the timeout layer), a failed authentication (missing or
  refused token) charges it (`Charge`: even past empty, down to -burst, so requests
  that passed `Ready` together all pay; `Ready` reserves nothing, so a concurrent
  flood is looked up up to its concurrency before it pays), an authenticated one spends
  from its credential's bucket (`mediaLimiter` ~200/s burst 2000, `tokenLimiter`
  ~50000/s burst 100000, per credential id) and charges the address's bucket when that
  refuses it (the lookup was already done), except that a demo account's API and media
  requests spend from the address's bucket (`allowCredential`: anyone can mint a demo
  session, so a per-token budget would be none); everything else (public routes, the
  Bridge's `/api/v1/bridge/*`, and `/api/v1/auth/*` with or without a token: sign-in,
  pairing, password, API keys) spends from `ipLimiter` (~50/s, burst 200, per IP).
  Each limiter's idle TTL is two refills, from a minute up to a day. rateLimit
  marks a request it charged (`addressCharged`), so requireAuth never charges it
  again and limits per credential any request rateLimit didn't. The brute-force
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
  (`problemCleared`: a fixed permission changes neither mtime nor size). A one-file book with a
  problem counts as unchanged while `problemCleared` finds the problem there (`soleFileProblem`;
  its metadata files are still read), and a file ffprobe rejected (`ErrProbeRejected`: its last
  stderr line is ffmpeg's invalid-data words) is not probed again while its own mtime and size are
  as they were then (`noteRejected`, by absolute path, for the server's life; any other reading of
  the file forgets it). A folder book
  whose parts are all >= 1 h with >= 2 distinct titles (tag, else file name) gets
  `suspect_parts`; rows from before 0017 are checked once with a tag read. `PATCH
  /admin/libraries/{id}` rescans only when the root or ignore rules change.
- **Health issues (`catalog/issues.go`, `api/handlers_health.go`)**: computed from the index on
  request (the summary is kept in the list cache until the next content write, see List cache); each book kind is one SQL predicate (`issuePredicates`) shared by the counts and by
  `GET /admin/books?issue=` (and `&issue_ignored=true`), so they can't disagree: `no_cover`
  (checked rows only), `no_chapters` (<= 1 chapter, > 2 h; the console's fix is the Bridge's Detect
  chapters, see The Bridge), `unmatched` (no ASIN, ISBN or recorded community match: `NOT matchedExpr`; left
  out of the summary while metadata is off), `transcode`, `suspect` (not a folder with a `book` override, the
  category's own fix), `scan_error`, `series_spellings` (`catalog/seriesspellings.go`: another book of
  its library spells its main series another way, same `series_key`; listed per series by `GET
  /admin/issues/series-spellings[?library_id=&ignored=true]` -> `{groups: [{library_id, suggested,
  spellings: [{name, books}], books}]}`, from the same predicate, `suggested` being `seriesName`;
  open = a book not ignored, ignored view = a book ignored; the console's fix is the author merge's bulk
  `series` edit), `split_discs` (one book split across disc folders, listed by
  its first disc: `books.split_parent` set, no override on that folder; the console's fix sets
  `book` on it, which joins them; its discs never also group as duplicates). `split_parent` is
  worked out during discovery (`library.markSplitDiscs`: a folder book whose folder is a disc
  set, the same `discSets` predicate the join uses, so the fix always joins; `IndexPath` the
  same from the folder's subtree) and stored by `UpsertBook`; an unchanged book whose value
  differs (a sibling came or went, a pre-0022 row) gets it via `SetSplitParent` without a
  reindex. `duplicate` is groups within one
  library (`DuplicateGroups`: same fingerprint AND size, same ASIN/ISBN (whatever the lengths),
  or `metaKey` with a length within 1 min / 2% (`sameLength`), an unknown length matching only
  another unknown one; copies across libraries are usually deliberate, so they group only with
  `?across=1` on the duplicates list, never in the Health count: the signals are then not kept
  per library, and with `?library_id=` only the groups holding one of its books are kept). Each join marks its kind on one
  copy and a group's `reason` is the strongest its joins support (`dupSet.reason`):
  `id_length_differs` (a group a shared ASIN/ISBN joined whose shortest and longest known
  lengths `sameLength` rejects, the whole group's so an unknown-length book can't bridge two
  recordings; an unknown length is no evidence either way); outranks even identical audio, as the group holds a different
  recording, and the console then calls no copy the one to keep) > `same_files` (only when every
  copy holds the same audio: a group identical audio joins in part takes the next reason, ADMIN-04) > `same_id` >
  `same_book`. A group is hidden while every member is ignored, so a new copy brings it back. Endpoints (admin only): `GET /admin/issues` (counts + samples + offline
  libraries with books/listeners + `checked_at`), `GET /admin/issues/duplicates` (the open
  groups, or with `?ignored=true` only the ignored ones; `catalog.DuplicatePage`: `?limit=` groups
  (<= 500, the default) after `?cursor=`, keyset on the kept copy's library + path, a paged one
  ending early past 300 copies; `{groups, total, next_cursor?}`), `POST`/`DELETE /admin/issues/ignore` (<= 1000), `POST
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
  same-origin on web). `authenticate` calls `auth.ResolveRequest` (or, for the `?token=` of a book's
  cover or stream with no `Authorization` header, `auth.ResolveCastRequest`; both are `auth.resolve`),
  which records the app (only when
  the request names one, so a headerless `<audio>` fetch keeps it) and the newest IP on the token, and
  puts the `auth.Credential` (token id, kind, device id, device name, app; a cast token's book) in the context (`credentialFrom`; every
  context key lives in one iota block in `respond.go`, since the IP key once collided). CORS allows the
  header (and `Range`, and exposes Retry-After and the OPML counts: see Podcasts). **Sessions are derived from progress saves**
  (and, since Track W1, from Sync v2's listen spans at their corrected device times: `recordSpan`, see Listening sync), not from the client-posted
  `listening_history` spans (those arrive only on stop, are dropped offline and carry no device):
  `handlePutProgress` passes every save to `catalog.RecordHeartbeat`, which extends the token's
  session on that book (`latestSession`: the one with the latest last save among those begun by then,
  not the latest id, since a span synced late opens a session dated when it happened) when the save
  comes within `SessionGap` (10 min), or later when the position
  advanced by about the time that passed (`continuousPlayback`, within `resumeWindow`, 12 h: Android
  pauses the player's save timer while the screen is off), and otherwise starts a new one; listened
  time is the position advance / speed, capped by the server time between saves (only server time is
  used for a save, so a wrong device clock or an offline replay can't inflate it; a span counts
  `listenedBetween`'s clip of its own corrected times); a session with nothing
  listened yet (a single save, such as "Mark finished") is shown nowhere
  (`listenedSQL`) and deleted by retention; a save's recording is best effort (a failure is logged, the
  save still succeeds), a span's is part of its batch's one transaction. `catalog.StreamMarks` (in memory) remembers `?transcode=1` streams per token for 10 min
  so the session is marked transcoded. Session times are fixed-width millisecond UTC strings
  (`sessionTime`) so they compare as text; hours are taken with `hourOf` (not `time.Date`, which
  loops on a daylight-saving fall-back). Retention: `pkg/launcher.retention` runs
  `PruneSessions` at startup and daily, rolling sessions older than the live
  `activity.session_days` setting (Settings > General `general.session_days`, env
  `AUDIOSILO_SESSION_DAYS`, 30-3650, default 400; `API.SessionRetention`, read at each run) into
  `listening_daily` per local day, listener and book, blanks `last_ip` on signed-out or expired tokens
  (`auth.ForgetRevokedAddresses`), and deletes dead cast tokens with their grants (`auth.PruneCastTokens`:
  expired, or the sender's token signed out; a cast row's own `revoked`/`expires_at` don't say that) and
  the pairing tokens that can never pair again (`auth.PrunePairingTokens`: revoked, expired, or linked to
  a used-up or expired code). Migration 0021 backfilled listening from before sessions were
  recorded: `listening_sessions.backfilled` rows (from the players' `listening_history` spans; no
  device, app or playback mode, so left out of those breakdowns) and `listening_daily.estimated` rows
  (one per book, in totals and tops only, never in a day, calendar or hour; `Activity.estimated` says
  how much). Book counts (`totals.books`/`finished`, a top user's, a person's own `/me/stats` totals):
  `finished` is the distinct books finished in the period (`finishedIn`; a book two people finished
  counts once on the Activity page) and `books` the books listened to OR finished in it
  (`bookCounts`), so a book marked finished or imported with no listening recorded still counts and
  finished is never more than books. `SaveProgress` stamps `progress.started_at` on insert and
  `finished_at` when `finished` turns on (cleared when it turns off), both from the save's own
  `updated_at`; a save's own `started_at`/`finished_at` are ignored. The player's progress JSON
  carries them as `started_at`/`finished_at` (`omitempty`; `catalog.Progress`, player redesign
  Phase 1b, capability `progress_edit`), and the listener edits their own with `PATCH
  /libraries/{id}/progress?path=` (`handleEditMyProgress`: the admin's `catalog.EditProgress`, its
  body decoded by the shared `decodeProgressEdit`, with the caller's own scope from
  `authorizedScope`: 403 outside it, even for an existing row; 404 `book_not_found` with no row and
  no book; 400 for a bad body or `ErrInvalidProgressEdit`, and for an exact (RFC3339) date in the
  future, admin edits too (a day-only start keeps the catalog's day of slack); an edit that sets
  nothing writes nothing, answering the row as it is or 404 with none; no listening session
  recorded). An edit's `updated_at` keeps its sub-second time, and `SaveProgress` compares and
  writes in one writer transaction, so an older device save never overwrites a newer edit. Endpoints
  (admin only): `GET /admin/sessions/live` (one per device, with chapter and IP; `chapter` is `metadata.ChapterTitle`, the player's filename tidy plus "" for a title that names nothing ("024", "Track 01": `metadata.IsGenericTitle`, which reads every script; "Chapter 10"/"Part 7" kept), with `chapter_index` so the console says "Chapter N" (`chapterLabel`, shared with the drop-offs); a one-chapter book has neither; drop-offs use the same `ChapterTitle`), `GET
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
- **Listening imports from Audiobookshelf (admin only, `internal/importer`, `catalog/imports.go`,
  `api/handlers_import.go`, migration `0034`)**: `POST /admin/imports/abs/users {url, token}` checks the
  server (`GET /status` must say `audiobookshelf`, sent without the token) and lists its users (a
  non-admin token: just its own, via `/api/me`) with `suggested_user_id` (same username, any case);
  `POST /admin/imports/abs {url, token, users:[{abs_user_id, abs_username?, user_id}], cutoff?}` records one import per
  mapping (`abs_username` names its source until the fetch reads ABS's own) (status `fetching`; 409 `import_running` while that user has one fetching or applying) and
  fetches in the background on `baseCtx`; then `review` (or `failed` with `error_code`
  abs_unreachable | abs_unauthorized | not_abs | fetch_failed | interrupted). `GET /admin/imports?user_id=` (each import with `cutoff_utc_offset`: the server's offset at the
  cutoff, so the console shows and compares it in server time),
  `GET /admin/imports/{id}` (+ `unmatched_items`, most listened first, <= 500), `PATCH` `{cutoff}` (re-plans
  from the stored payload, `import_payloads`, gzipped and versioned; deleted once applied), `POST .../apply` (one transaction: the person's previous applied ABS import is
  undone first, so a re-import never double counts), `POST .../undo`, `DELETE` (not while applied: 409
  `import_applied`). The token lives only in the fetch's memory (never stored, logged or answered); `Connect` and
  `Start` refuse an empty one, one over 8 KiB or one with CR/LF/NUL alike (400 `invalid_import`,
  `checkToken`). The ABS client: GETs only, http/https only, no credentials, query or fragment in the
  URL, `/status` (sent without the token) must identify ABS before the token goes anywhere, redirects
  only to the same host (at most 5, never https -> http), every response size-capped, header and
  whole-request timeouts, every error a fixed sentence (never an ABS body). It does NOT block private
  or loopback addresses (an ABS on the LAN is the normal case); acceptable because every import route
  is admin-only. Matching
  (`match.go`): path components from the end (same relative path, or a unique suffix either way; the
  ABS side uses its absolute path; a folder vs the lone file in it), then ASIN, ISBN, then
  `pkg/match.Best` among a matching author's books, every non-identifier tier with the length check
  (5% + 2 min); only books the target user can access (`no_access` otherwise); two items still in ABS
  on one book are both `contested` (each has progress), while gone items (deleted in ABS: sessions only;
  `buildPayload` drops any progress/bookmarks left on them) match it with the one present item, if any,
  their sessions merging; an ABS item at a folder this server splits into disc books is `split_discs`
  (join the discs first). Applying writes sessions with `backfilled=1` and `client_app` `Audiobookshelf`
  (`imported: true` on the wire; never in `LiveSessions`), except a session already past the session
  retention (`activity.session_days`, read at apply via `importer.New`'s retention func, the zone as the
  prune's), which goes straight into the `listening_daily` rows `PruneSessions` would make of it
  (`ImportWrite.RollUpBefore`/`Zone`, `catalog.daySums` shared with the prune; summary counts unchanged),
  plus one `listening_history` span per session
  (start/end position over its start and clamped end, `import_id` set) so the book's History tab and
  `/me/history` (the Journal) show it; their ids are ordinary, so
  `ListSessions` orders by `started_at` (then id; indexes `idx_sessions_started` /
  `idx_sessions_user_started`) and resolves its `before` session-id cursor to that pair (the console also sends
  `before_at`, the session's start, so a gone cursor continues in place; without it a gone cursor
  continues from its nearest recorded neighbour, `goneCursor`), only sessions starting before the cutoff (default: the user's first
  listening here, `catalog.ListeningStart`; null = none), long-open sessions clamped (`last_at` =
  start + listened when the span is over 3x it); progress fill-only (`importer.mergeProgress`: never
  rewound or un-finished, moved on only when ABS's last update is newer, earliest start); one estimated
  `listening_daily` row per book where ABS's position outruns its sessions (0021's rules; none for a
  book finished in ABS with no listening session there, whose position ABS puts at the end); bookmarks
  deduplicated (same text within 2 s); an undo deletes only the imported bookmarks still as written
  (`note = import_note`, no label: `bookmarkUntouched`) and hands the edited ones to the person
  (`import_id` 0). A re-import's undo of the previous import restores its progress rows exactly
  (their own updated_at, version bumped: `progressPrior.restored`, what the review's `importState`
  simulates), so the new import moves them on again like a first one; a row it leaves where the old
  import had moved it is then stamped now (`keepRewound`), as a standalone undo stamps it, so a
  device holding the old import's row adopts the rewind. `catalog.InterruptImports` (launcher, at start): fetching ->
  failed `interrupted`, applying -> review. `PruneSessions` rolls imported sessions up per import.
  Audited `import.start|apply|undo`.
- **Your listening (player redesign Phase 1b, capability `user_stats`, `catalog/userstats.go`,
  `goals.go`, `api/handlers_userstats.go`)**: the caller's own stats, from the same accumulator as the
  Activity page, built for one user by `newUserListenAcc` (the one user-id guard; its reads take the
  per-user query spellings, served by `idx_sessions_user_last` / `idx_daily_user` / the progress key).
  `GET /me/stats?range=` → `{"stats": catalog.UserStats}` (totals/previous without
  `listeners`, `days` as `{date, listened}`, hours, top books/authors/narrators/series, `finished_books`
  ≤ 100, playback, clients; none of the admin-only steps run). **Privacy**: the types have no field that
  can carry another user, user 0 is refused (`errNoUser`: 0 is "everyone" to the
  accumulator), and the rows naming a book pass the caller's CURRENT access (`UserScopes` +
  `ScopesAllow` / `scopesFilterSQL`): a revoked share's book leaves `top_books`/`finished_books` and the
  authors/narrators/series (ranked from in-scope books only, `listenAcc.topPeople`), while the totals,
  days and hours keep all the caller's time. `GET /me/listening?range=` → `catalog.UserListening`
  (period + `days`, no `by_user`). `GET /me/goal` → `{goal: {books_per_year, updated_at (ms UTC)} | null, year,
  finished}` (this calendar year in server time, the caller's finishes, counted like `totals.finished`),
  `PUT /me/goal` `{books_per_year: 1..1000}` (else 400) answers as GET, `DELETE /me/goal` 204, idempotent.
- **Listening in the house (player redesign Phase 8, capability `household`, `catalog/household.go`,
  `api/handlers_household.go`)**: people on one server who opt in see what the others who opted in
  are listening to (the player's Home row and the book page's "In the house" card). `GET /me/household`
  -> `{"share_listening": bool}` (no row = false); `PUT /me/household` `{"share_listening": bool}`
  (strict decoder, the field required: `{}` is 400) answers the same shape; demo accounts 403
  (`nonDemoAccount`; GET stays allowed), an API key may set it (the person's own credential).
  `GET /household/listening` -> `{"sharing": bool, "listening": [Row]}` (the envelopes are the
  handler's): the handler asks `HouseholdSharing` first and answers `{false, []}` before loading any
  scope (non-sharers poll it too), then `catalog.HouseListening`, ONE query driven from
  `household_sharing` (the caller's own row, so it is reciprocal by itself; each other sharer; a
  scalar subquery takes their newest session through `idx_sessions_user_last`; `books` joined only
  for those winners; `CROSS JOIN` pins that order, the verified plan is in the comment).
  **Privacy rules (all in catalog; each has an allowed and a denied test)**: reciprocal (`listening`
  is `[]` unless the CALLER shares); other people only (never the caller), enabled, not demo, sharing
  on; per person ONE row, their newest session with listening in it (`listening_sessions`,
  `import_id = 0`, `backfilled = 0`, `listened > 0`) within `houseWindow` (24 h) and started at or
  after they last turned sharing on (`household_sharing.updated_at`, which moves only when the choice
  changes, so a repeated PUT of the same choice keeps it; stamped inside the write transaction, once
  the writer is held, so a save that committed before the switch is never dated after it;
  `RecordHeartbeat` never continues a session started before it, so no session holds listening from
  both sides of the switch and a save after it with no listening in it can't bring older listening
  into view; listening done while sharing was off never shows, server#221); that book must pass
  the CALLER's current scopes (`ScopesAllow`), else the person is dropped, never shown on an older
  in-scope book. A row is `{user_id, username, library_id, path, title, author|null, state, position,
  duration|null, chapter|null, chapter_index|null, updated_at}` (newest first; `state` `playing`
  within 60 s, `paused` within `SessionGap` (`SessionPlaying`/`SessionPaused`), else `recent`; `title` the index's, else the path's last
  segment; `duration` the index's, else the session's, null for 0; chapter fields as the live
  sessions', via the shared `chapterCache`; `updated_at` the newest save, fixed-width ms UTC). It
  NEVER carries a device, address, app or token: `HouseListeningRow` has no such field, unlike the
  admin-only `LiveSessions` (never reuse that for anything a non-admin sees). Polling only (the
  player refetches every 60 s while focused); no realtime channel.
- **Household members (player redesign Phase 8b-1, capability `household_members`, `auth/members.go`,
  `catalog/members.go`, `api/handlers_members.go`; security-critical)**: any non-demo person (the owner)
  adds a member from the player, with no admin: a real account (role user, no password) whose
  `managed_by` is the owner, with a `member_kind` (`kid`|`grownup`) and `member_access`
  (`all`|`collections`) only the owner sets. One level only: an owner is never a member, a member never
  has members, never gets the admin role (`UpdateUser` 409 `member_not_admin`). **Access** (the member
  rule, in `UserScope`/`UserScopes`/`AccessibleLibraries`, decided per request, no cache): the member's
  own grants UNION, for `all`, the owner's CURRENT scopes computed with the owner's role (an admin
  owner gives every library as content scope), or, for `collections`, the items of collections the
  owner owns and shares with the member, each kept only while the owner's scopes allow it
  (`relpath.Within`: a folder book covers its subtree; an `""` item never grants a library); a
  member-owner (bad data) or self-managed row derives nothing; a disabled owner changes nothing.
  **The rule rides on the request**: `auth.lookupToken` reads `managed_by`, `member_kind` and the
  rule (`auth.User.MemberRule`: owner, access, owner is an admin) through a primary-key `LEFT JOIN`
  of the owner in its one statement (`memberColumns`/`memberScan`, which `getUser` shares), and the
  transport passes it as a `catalog.Caller` (`api.callerOf`) to `CallerScope`/`CallerScopes`/
  `CallerLibraries`, so a non-member's scopes cost the one grants statement they did before 8b-1
  and a kid's refusal reads nothing; `UserScope`/`UserScopes`/`AccessibleLibraries` (id + isAdmin)
  read the rule themselves (`memberRuleOf`, the same conditions) for a caller known only by id
  (the importer, tests); `TestMembers_RuleReadsAgree` holds both readings to the same scopes for
  every kind of account, bad data included. New transport code builds scopes from the request's user with `callerOf`,
  never from an id. Scopes over 32 paths filter SQL in
  `pathsJSONFilterSQL`'s single-parameter form (a row's path and its ancestors looked up in one JSON
  list; same rows as the OR form, which can't hold 1,000 arms at all; a path that isn't valid UTF-8,
  which JSON can't carry exactly, keeps an OR arm of its own beside the list). `BenchmarkMemberScopes`
  (M1 Ultra, 3,000 books, a 1,000-item kid vs a whole-library listener, the `Caller` path): browse
  2.4 ms vs 0.26 ms, search 9.1 ms vs 5.6 ms, progress list 4.9 ms vs 1.7 ms, the scope load
  1.05 ms vs 0.017 ms (0.034 ms by id, which reads the rule).
  **Routes** (`/me/household/members`; API keys 403 everywhere, demo 403 `demo_account`, a member
  403 `member_cannot_manage`, someone else's member 404): `GET` -> `{allowed, reason?, max, members}`
  (200 for everyone signed in: `reason` `demo`|`member`|`feature_off`|`not_allowed`; at the cap is not
  a reason), `POST {name, kind, access, sign_in_here?, collection_ids?}` (strict; the account limiter, as
  `gateSelfService`'s, charged after the API-key, demo and member refusals;
  403 `feature_off`/`not_allowed`, 409 `member_limit`, 400 `invalid_name`/`invalid_collections`;
  a name is 1-40 characters with no control or invisible character (`auth.invisibleRune`: every
  format character, so bidi controls and marks, zero-width spaces, the BOM and tags, plus the Hangul
  fillers and the Braille blank; nor a line or paragraph separator, nor a variation selector selecting
  nothing (first, or after another); ZWJ/ZWNJ, an emoji flag's closed tag sequence of 1-7 lowercase
  letters and digits (`hidesText`) and right-to-left scripts are fine) and something visible, server#190;
  username derived from the name, then `-<owner>`, then `-2`, `-3`, ... with no cap) -> `201 {member, session?}`,
  `POST {id}/session` (201 envelope), `POST {id}/pair` (unlinked, single use, 10 min; both 409
  `member_disabled` for a member the admin disabled, whose tokens would never work), `GET
  {id}/devices` (a bare `[MyDevice]`), `DELETE {id}/devices/{device_id}`, `PATCH {id} {name?, kind?,
  access?, collection_ids?}` (`{}` 400), `DELETE {id}`. Only adding checks the switches (the server's
  `household.members_enabled`, the person's `can_add_members`) and the cap (`household.max_members`,
  1-50, default 6); the other routes keep working on existing members. `collection_ids` is the full set
  of the caller's own collections shared with the member (`catalog.SetMemberCollections`, run inside
  `auth`'s transaction through `auth.ShareFunc`). A session made here records `created_on_token_id`
  and copies the caller's device name. **Kids**: 403 `kid_account` on `POST /auth/password|tokens|pair|
  recovery`, `PUT /me/household`, `PUT /me/collections/{id}/shares` and every `/me/upcoming` route (the
  list, the calendar link, the calendar feed, the Upcoming cover), from one route wrapper
  (`api.notKid` in `api.go`, before the handler; wrap any new route a kid can't use), and the writes
  refuse a kid themselves inside their transaction (`auth.refuseKid` in `IssueAPIToken`,
  `GenerateRecoveryCode`, `IssueOwnPairingToken`, `IssueFeedToken`; `catalog.SetHouseholdSharing`, `SetCollectionShares`;
  `SetPassword`'s `ErrKidNoPassword`, which `/auth/password` answers as 403 `kid_account`), so a request
  that passed `notKid` just before the owner's switch to kid committed can't leave the kid holding one (403
  `kid_account`); the admin can't mint or rotate a kid's invite either (`POST /admin/users/{id}/authcode`,
  `POST /admin/authcodes/{id}/rotate` answer 409 `kid_account`: `auth.refuseKid` in `CreateInvite` and
  `RotateAuthCode`, the latter so a used-up invite kept as history can't be revived into a kid's account),
  since only the owner sets up a kid's devices; switching a grown-up to a kid (or the admin assigning a kid) clears the password,
  API keys, pairing tokens, the calendar feed token,
  recovery code, still-redeemable invites (`supersedeActiveInvites`), the collections shared between
  the kid and anyone but their owner (both ways, so no sharer is left with a kid in a list the server
  refuses back) and the person's `household_sharing` row (`clearKidCredentials`), sessions stay, so
  a kid made a grown-up again starts with house sharing off and opts in again; `ShareTargets` lists
  a kid only to their owner and `SetCollectionShares` refuses one from anyone else
  (`ErrUnknownUser`, `kidOnlyToOwnerSQL`); `HouseListening` and `HouseholdSharing` leave kids out
  both ways because a kid has no row (no kid filter of their own). `/me` and the session envelope carry
  `managed_by` (`{id, username}`|null), `member_kind` and, for a member only, `display_name` (omitted otherwise); `/me/devices` rows carry `member_sign_ins`
  (always `[]` for a member, with no read) and `DELETE /me/devices/{id}?also_members=1` signs those out in the same transaction
  (`{current, members_signed_out}`; each member signed out is audited `member.device_revoke`). The list's `reason` is `auth.MayAddMembers` (the one may-add ladder,
  which `CreateMember` runs too). Admin: user objects add `member_access`, `can_add_members`,
  `members_count`, `display_name` and, on `managed_by` only there, the owner's own `display_name`
  (`api.adminUser`/`adminOwner`; the player's `managed_by` never carries it, and `/me`'s
  `display_name` is `api.meUser`'s, a member's only); the detail adds `members` (each an
  `auth.Member` plus `disabled`) and `member_collections` and, for a member, `owner_libraries` and
  `owner_is_admin`; the settings envelope's `household` section carries `max_members_default`,
  `max_members_min` and `max_members_max` as read-only facts; `PATCH`
  takes `can_add_members` and `managed_by` (an id with `member_kind` + `member_access`: "Assign to
  someone", 409 `owner_is_member`/`owner_is_demo`/`self_managed`/`member_is_demo`/`member_not_admin`/
  `has_members`, 400 `kid_no_password` for a non-empty `password` on a kid or beside assigning one,
  which `""` (clearing) is not; null: "Make independent"); the whole PATCH is one `auth.UpdateUser` transaction that checks every field before
  writing, so a refused field changes nothing; `DELETE ?keep_members=<id>,<id>` (or the parameter repeated) deletes the person's
  other members with them (400 `invalid_keep_members`). Joining another owner's household signs the
  person out of what another owner set up for them (`auth.leaveOwners`: the sessions and API keys made
  on anyone else's devices, by `created_on_token_id`, which would otherwise carry the new owner's
  access to the old owner's phone, the calendar feed link made on one (deleted; `IssueFeedToken`
  records the device of the session that made it), and open unlinked pairing tokens); leaving one ("Make
  independent", or kept by `keep_members`) takes the open pairing tokens only; their own devices stay
  signed in. Audit `member.create|update|delete|session|pair|device_revoke`
  (actor the owner, target the member; tested in `members_test.go`, not `audit_coverage_test`).
- **Cast tokens (player redesign Phase 8, capability `cast`, `auth/cast.go`, `api/handlers_cast.go`;
  security-critical)**: a Chromecast receiver plays from URLs the player hands it, so the player
  never hands over its session token: `POST /cast/token` `{"library_id", "path"}` (strict) mints a
  cast token -> `201 {"token", "expires_at"}` (fixed-width ms UTC). The path resolves through
  `scopedPath` + `bookAt` exactly like the media routes (400 empty path, 403 outside scope, 404 no
  book; any path in a podcast library is 403 `not_a_book`, as episodes are never cast; a part path
  resolves to its book, whose own `rel_path` is stored, so the token covers every
  file of it); demo accounts may mint (they can play); an API key may not (403, `denyAPIKey`'s
  containment: a key never mints another credential, even a cast token, which would die with it). A cast token is a
  `tokens` row of kind `cast` (`auth.IssueCastToken`: SHA-256 hash only, `expires_at` 24 h in whole
  seconds, `castTokenTTL`) plus a `cast_grants` row (the book, and `parent_token_id`: the caller's
  own token); one transaction deletes every dead cast token (`pruneCastTokensSQL`: expired, or no
  grant, or its parent gone, signed out or expired) and keeps the person's newest
  `maxCastTokens` (20); the daily retention (`pkg/launcher.retention`) runs the same prune
  (`auth.PruneCastTokens`), so a dead token's row (the receiver's `last_ip`) and grant (which book
  was cast) never outlive it by more than a day. **Validity** is `lookupToken`'s one check (unknown, revoked, expired,
  disabled user) plus, for kind `cast`, a grant and a live parent (`castGrantOf`, a second query run
  only for a cast token, so every other token's lookup is one two-table query, and a valid session,
  API key or bridge token's is answered from `Service.tokens`, a `store.GenMap`, until the next
  content write or its own touch: every change that could end a token is a content write, and
  expiry is checked on each hit): **signing out the device that
  minted it** (logout, My devices, the console's sign-out of that device or Sign out everywhere, a
  password change or reset that ends that session) **ends it**, as do the account's delete or
  disabling, its scope and the 24 h. **Acceptance**: `ResolveRequest` never accepts a cast token;
  `auth.ResolveCastRequest(secret, presence, libraryID, path, kinds...)` does, only when its
  grant covers the path (`castGrant.covers`: same library, the book's path itself or below it on a
  segment boundary via `relpath.Within`, `Book` never reaches `Book 2`; an empty path on either side
  covers nothing; it cleans the path itself with `relpath.Clean`, which `catalog.CleanRelPath` is, so
  `Book/../Book 2` never passes whoever calls). `api.authenticate` calls it only on the routes wrapped in `requireBookMediaAuth`
  (`GET /libraries/{id}/cover` and `/stream`, NOT `/meta/cover`, which keeps `requireMediaAuth`),
  only for a `?token=` with NO `Authorization` header (a cast token is never a bearer credential,
  nor accepted beside one, a `Basic` one included), with `catalog.CleanRelPath(?path=)`; ONE lookup decides session, API key or
  cast. A path below the book's folder can belong to ANOTHER book nested there (a folder book's
  folder can hold more books: `Series` holding `Series/Book 2`), so for a cast credential
  `api.castOwnBook` then requires the path to address the token's own book in the index
  (`catalog.BookPathAt`: the book at the path, else the innermost folder book holding it; a path the
  index places in no book is refused too). Any refusal is the uniform 401. The
  context then carries the token's user and a `Credential{Kind: auth.KindCast, ID: <its tokens id>,
  DeviceID: <the sender's token id>, Book: <its grant's path>}`, and the handler's `authorizedPath` checks the person's
  CURRENT scope as for every request (a revoked share is a 403). `mediaAuth` sets
  `Cross-Origin-Resource-Policy: cross-origin` when the credential is a cast token (the
  receiver, a gstatic.com web app, loads them as no-cors `<audio>`/`<img>`; `secureHeaders'`
  `same-site` would make the browser block them), and on every authenticated media answer once
  `cors_origins` is set (server#188: a player on another site loads covers the same way; a no-cors
  request has no Origin to check, and media takes only an explicit token, never a cookie, so the
  policy's ambient-credential guard has nothing to guard; a failed auth and every non-media route
  stay `same-site`); the media limiter keys every credential by its
  token id (one id space, so a cast token has its own bucket). A cast token is never a device:
  `ListDevices` (`/me/devices`, `/admin/devices`) and the device sign-outs take only session and
  API-key kinds, like pairing tokens. **Transcode**: a cast token authenticates `?transcode=1&t=`
  like a session (the sender asks for it on codecs the Default Media Receiver can't play: ALAC, WMA,
  AC-3, or a book the web player would transcode); each such stream holds one of the 4
  `transcodeSem` slots while the receiver reads it, and a full semaphore is the usual 503 +
  `Retry-After` (the receiver reports a load error; the sender should say "busy, try again"); it
  marks the SENDER's session transcoded (`StreamMarks.Note` keys on `Credential.DeviceID`, the
  parent token, whose progress saves make the session). **Not the ebook** (Bridge Phase 3b): `GET
  /libraries/{id}/ebook` takes no URL token of any kind and there is no `reader` token kind. The web
  host fetches the epub with its bearer header and posts the bytes to the reader; the native host
  (3b-2) downloads it with the header into a local file. Nothing needs a URL carrying a credential,
  so none exists. Only if 3b-2's proof shows a WebView can't read a local file does a `reader` kind
  on `cast_grants` (own kind, `/ebook` only, 10 min) get planned and reviewed.
- **The Bridge (BRIDGE-PLAN.md Phases 0, 1a, 1c, 1d, 2b and 3c, capability `bridge`, `auth/bridge.go`, `internal/bridge`,
  `api/handlers_bridge.go`, `handlers_bridge_estimate.go`; security-critical)**: the optional companion (repo `audiosilo-bridge`,
  private) that writes to the libraries and runs the heavy jobs, so this server stays read-only. **The
  server never connects to the Bridge**: it stores intents (jobs) and the Bridge's last heartbeat, and
  the Bridge calls in. **Pairing**: an admin's `POST /admin/bridge/pair` (an API key 403, `denyAPIKey`)
  mints a code (`generateAuthCode`, hashed in `bridge_status`, 15 min, replacing one still waiting; a
  paired Bridge keeps working until it is exchanged) -> `{code, expires_at, server_url}` (`server_url` is the
  home address when there is one: plain http on the LAN, since a Bridge can't verify a self-signed
  certificate); the Bridge's
  public `POST /bridge/pair {code}` (the redeem limiter) trades it in ONE transaction
  (`auth.ExchangeBridgeCode`: the code, its expiry, and that the admin who made it is still an enabled
  admin; every earlier bridge token revoked, the old status cleared) for a `tokens` row of kind `bridge`
  (`201 {token, server_id, server_name}`; one active Bridge per server). `DELETE /admin/bridge` revokes it
  and forgets the row (`UnpairBridge`). **The bridge token is narrow**: routes opt in to token kinds and only
  the Bridge's own list `auth.KindBridge` (`requireBridge` = `authenticate` with `bridgeKinds`,
  `/api/v1/bridge/*`, Authorization header only, never `?token=`; only a missing or refused token is a 401,
  which stops the Bridge calling in until its 5-minute retry, so a failure to check one is a 503); `resolve` refuses it once its admin is
  demoted (lookupToken already refuses a disabled one); it is never a device, and never touched (its calls
  are not its admin's activity, the newest `last_seen` of their tokens). "Paired" has ONE statement,
  `auth.BridgeLiveSQL` (a live bridge token of an enabled admin, partial index `idx_tokens_bridge`), which
  both `GetBridgePairing` and the heartbeat reads use; `auth.BridgeTokenLiveSQL` is the same for one token,
  which the heartbeat's write asks inside its transaction and a claim asks first on the reader pool
  (`claimDue`), then again inside its transaction when it has something to write (a call authenticated
  before a pairing or an unpair revoked its token, a long poll above all, saves nothing and claims
  nothing: 401).
  A route-table
  test sends it (header and `?token=`) to every other authenticated route: all 401. **Heartbeat** `PUT
  /bridge/status {version, kinds, libraries[{library_id, mount_path, mapped, writable, checked_path,
  error}], workers, ui_url, estimates?}` (`estimates.sidecars`: the Bridge's costing figures,
  `asr_seconds_per_audio_hour`, `agent_usd_per_audio_hour` / `agent_usd_per_ebook` (null before any
  history), `samples`, `agent_backend`, `agent_ready`, `subscription`, `can_contribute`,
  `monthly_budget_usd`, `month_spent_usd`; read LENIENTLY, an unknown key ignored and a block that doesn't
  decode or is out of its bounds dropped, never refusing the heartbeat, since a refused heartbeat takes
  the Bridge offline; bounded and checked: `family.name` kinds, library ids, and `ui_url` http(s)
  only since the console links it, are refused otherwise; free text is cleaned to printable bounded text,
  `cleanText`, never refused, as is a job report's `error` (line breaks kept): it is partly the outside
  world's, and a refused report is retried for ever while its job is re-run; one row, never a history)
  answers `{server_time,
  libraries[{id, name, root}], accepts}` (`accepts: ["setup"]`, `bridge.HeartbeatAccepts`: the optional
  top-level blocks this server reads, since the strict decode refuses an unknown field; the Bridge sends
  one only after a reply listed it, and drops them all and resends once on a 400). **Setup** (Phase 2b,
  `bridge/setup.go`): `setup {items[{id, done, required, detail?, library_ids?}], can_contribute,
  github_login?}`, the Bridge's onboarding checklist, read leniently like `estimates` (an unknown key
  ignored, a block that doesn't decode dropped, never the heartbeat) and bounded (at most 16 items, an
  item with a bad (`^[a-z][a-z0-9_]{0,31}$`) or repeated id left out, an unknown id kept, `detail` cleaned
  to 256 bytes, library ids positive and once each, a login not GitHub's shape dropped); `GET /admin/bridge`
  carries it (omitted when absent), `GET /server` never does. `Status.CanContribute()` is the setup
  block's `can_contribute` when one was sent, else `estimates.sidecars.can_contribute` (the Bridge sends
  both); the `contribute` feature, the estimate's `can_contribute` (`Status.EstimateSidecars`; the
  handler also needs the contributions module on) and the `cannot_contribute` refusal all read it. **Capability**: `capabilities.bridge` is true only while the last
  heartbeat is under `bridge.OnlineWindow` (2 min) old (`bridge.Service` caches the parsed heartbeat for
  `heartbeatTTL`, 5 s, so `GET /server` mostly reads no database; `Forget` drops it on a pairing or an
  unpair, and a read that began before a save or a `Forget` never replaces its answer: `hbGen`), with a
  top-level `bridge: {version, features}`
  beside it for a signed-in caller only (a session or an API key of any role but a demo account's,
  `api.signedIn` over `auth.Peek`, which records nothing on the token, so the public route never waits on
  the writer; asked only while the Bridge is online; an anonymous, refused, demo or bridge token sees the
  flag alone) (features = the families of the kinds the server knows, plus `contribute` while the last
  heartbeat says it can contribute (`Status.CanContribute`; `bridge.FeatureContribute`), `[]` for none;
  `capabilities` stays all booleans).
  **Jobs** (`bridge_jobs`): kinds allow-listed twice, inside `bridge.Service.Enqueue` (the server queues
  only a kind it knows, with per-kind params: 400, AND the Bridge advertised: `ErrKindUnavailable`, 409
  `kind_unavailable`); Phase 0's
  kind is `bridge.verify` (`library_id` required, optional book `path`, params `{hold_seconds 0-600}`),
  Phase 1a's `sidecars.generate` (a book `path` required, params `{source, contribute, work_id}`; it writes,
  so it is refused 409 `library_not_writable` for a library the last heartbeat doesn't report mapped and
  writable, and `contribute: "issue"` needs the contributions module (see Community jobs) and is refused
  409 `cannot_contribute` unless `Status.CanContribute()` says the Bridge can; the open job is still
  returned), and Phase 1c's
  `sidecars.describe` (a description from the notes the Bridge kept from generating the book, no new
  ASR: a book `path` required, params `{}` or none, any field 400 (`noParams`), stored as `{}`;
  it writes, so the same `library_not_writable`; refused 409 `generate_open` (`ErrGenerateOpen`, checked
  inside Enqueue's transaction after the dedupe) while a `sidecars.generate` job is open for the book,
  which writes the description itself, while a generate job is never refused for an open describe; its
  own dedupe key; an older Bridge doesn't advertise it, so it is the usual 409 `kind_unavailable`), and
  Phase 1d's `chapters.detect` (feature `chapters`: the Bridge transcribes a book with no chapter markers and
  finds the chapters in the narration, no agent, then writes `.audiosilo/chapters.json`: a book `path`
  required, params `{}` or none (`noParams`, shared with describe), writes; refused 409 `generate_open` while a
  `sidecars.generate` job is open for the book (that run finds them itself), 409 `describe_open`
  (`ErrDescribeOpen`) while a `sidecars.describe` job is (both rewrite the book's bridge.json on the Bridge;
  a describe is refused 409 `detect_open` while a detect job is open, the other way round), and 409
  `chapters_chosen` (`ErrChaptersChosen`, checked by the api's queue handler from `catalog.ChapterChoice`)
  for a book whose chapter source an admin chose (files or community: a found list stands in only with no
  choice, so it would never be used); a NEW `sidecars.generate` is refused 409 `detect_open` (`ErrDetectOpen`)
  only while a detect job is queued, claimed or running, since cancelling it would throw its transcription
  away and the generate run after it reuses it, while a PARKED one (`chapters_not_detected`, a person's
  decision) is cancelled by the new generate job in its transaction ("it finds the chapters itself"), as
  an open describe job is. `kindSpec`'s `refusedWhileOpen` and `cancelsOpen` are the one statement of which
  open kind refuses or gives way to which (`Service.blocker`, `givesWay`: a job with `cancel_requested`
  refuses nothing, as it ends without writing, so a detect job queues behind a generate job being
  cancelled; a blocker the new kind also cancels refuses only while working, or queued and not held
  (`Service.held`, the claims' own rule: a queued job whose module is off); kinds asked in name order), and
  `Service.Blocker` names the open job that refuses a new job of a kind now). States queued -> claimed -> running -> done | failed | needs_attention | cancelled; the Bridge PARKS
  a job it keeps running (`needs_attention`, the park's code in `progress.park.code`, its message in
  `error`) and unparks it (back to `running`) under the same claim: a held state, so its reports still
  land, but a parked job's lease is dropped (only claimed and running leases expire) and a running
  report renews it. A report that keeps a parked job parked and names no park (absent, null, or with
  no code, as `ParkCode` reads it) keeps the stored one (`carryPark`; the stored progress whole when the
  merge would pass the progress cap), so a Bridge resuming after a restart neither blanks the reason
  nor raises a second park event. `dedupe_key`
  (kind|library|path) returns the open job on a second press (200, else 201; the console says when that
  job is one still stopping after a cancel, `cancel_requested`, as nothing new can be queued until it ends). `GET /bridge/jobs/next?wait=
  &kind=` (`bridge.Service.ClaimFor`) long-polls (at most 25 s, woken by an enqueue; it ends with the
  server's base context, so a stop isn't held by the claim a Bridge always has waiting: 204) for the
  highest-priority oldest queued job of the
  heartbeat's known kinds, narrowed by `kind=` (the Bridge's free lanes; none left answers 204 at once),
  and leases it for 2 min (`attempts` + 1). A claim with nothing to write (no lapsed lease, no lost
  private follow request, nothing `nextQueued` would hand out: `Service.claimDue`, on the reader pool)
  answers without the writer, so an idle paired Bridge's polls leave the content generation alone (as
  do its heartbeats and its lease and progress job reports: see Content generation, server#258). `PATCH /bridge/jobs/{id} {attempt, state?, progress?, result?,
  error?}` is bound to the claim's attempt (a dead claim's late report, an ended job, or one gone with its
  library: 409 `not_held`), renews the lease and answers the job, whose `cancel_requested` asks the Bridge
  to stop.
  Claim responses retain the request deadline but bypass response buffering and flush before return;
  a wait the deadline ends still answers 503 `request timed out`, as the buffered timeout did.
  A claim ended before its reply was written, a failed 200 write, or a Bridge that left before the
  reply finished returns its untouched lease to queued and restores the attempt in one transaction,
  then wakes claims; a flushed reply stays claimed when the server starts stopping or the deadline
  passes after it (the Bridge still reads it). Cleanup matches the claimed state, attempt and
  lease timestamps; any Bridge report changes the state or renews the lease, even an empty
  acknowledgement within the same second, so received work is preserved. A pending admin cancel
  becomes cancelled instead of queued.
  Leases are expired (`expireSQL`: requeued with the dead attempt's progress, result and error dropped,
  failed on the last of `MaxAttempts` (3), or cancelled when a cancel was asked; those two keep what was
  reported) inside every `bridge.Service` transaction that reads or changes job states (`expireTx`:
  enqueue, claim, update, cancel), and before a console read only when a lease has actually run out
  (`expireDue`: a reader-pool check first, so polls don't queue for the one writer); once it commits, a
  requeue wakes the waiting claims and a failed job is announced (`settle`). `MoveJobs` (the catalog's
  move transaction) expires nothing: a lease it finds lapsed is left to the next Service call, which
  can announce it. `List` returns its
  own `next_before` (it reads one row past the page). Cancel (`POST
  /admin/bridge/jobs/{id}/cancel`) ends a queued or parked job at once, else sets `cancel_requested`.
  `GET /bridge/libraries/{id}/book[?path=]` gives an INDEXED book's files with the size and mtime the
  server sees now (a single-file book, which has no file rows, is listed as its own file; no path: the
  first whole book of the library's first five by title, the mapping check's sample; 404 `no_book`; never an
  on-demand index); `POST /bridge/rescan {library_id, path, mode?, from?}` re-reads one path: mode
  `full` (or none) indexes it (`IndexPath`; with `from`, the book's path before a move the Bridge made,
  `Scanner.IndexMoved` carries its durable state when the old book is gone from disk and the
  fingerprints match, else ignores it), mode `attachments` re-reads only the book's `.audiosilo/` files
  (`Scanner.RefreshAttachments`, answering `{path, attachments: {characters, recaps, description, chapters, bridge, readalong}}`); 404
  `not_indexable`. Console: `GET /admin/bridge` (pairing, online, heartbeat, kinds, features, each
  library with the Bridge's mapping, workers, `ui_url`), `GET`/`POST /admin/bridge/jobs`, `GET
  /admin/bridge/jobs/{id}`, `GET /admin/bridge/estimate?library_id=&path=&kind=[&brief=1]` (the
  confirmation before `sidecars.generate`, `handlers_bridge_estimate.go`: the source (`library.BookEbook`,
  the Bridge's own .epub rule, an unreadable epub (no zip, no container.xml) counting as none), `Status.EstimateSidecars` from the heartbeat's figures (`asr_hours`,
  `agent_cost_usd` null while there is no history, `subscription`, `agent_ready`, `figures` false when
  the Bridge sent none, so a false `agent_ready` means "not known"; `can_contribute` the Bridge's answer,
  false while the contributions module is off), `writable`, what the book already
  has (`has.community` via `meta.Service.Coverages`, cache first and never a full Enrich per book,
  `community_unknown` when it couldn't be asked; `has.local` from `book_attachments` (`catalog.BookAttachmentParts`: which columns are non-empty, nothing decoded); each with
  `characters`, `recaps` (the recap LIST: `coverageOf` reads `fillable`, where a community
  summary alone is no recaps, so Generate is offered and the series batch queues it) and, since Phase 1c, `description`: the community work's `community_description`
  (`Coverage.Description`: its `community_description` or its core description, either shown before a generated one) or a stored generated one), `can_describe`
  (the Bridge is online and advertises `sidecars.describe`; brief too), up to 10 earlier
  books of its series in the library with no recaps (`catalog.EarlierInSeries`, each costed), and any
  open job (`open_job`: the book's open generate job, else the job of another kind that refuses a generate
  job now, `Service.Blocker`, i.e. a queued or working `chapters.detect` or `sidecars.contribute` job, never
  a share held while `contributions` is off; its `kind` says which, additive: the player's Generate holds
  back for those two kinds);
  `brief=1` skips the folder look and the series scan, for a card or a batch; `kind=readalong.align`
  answers its own shape, below; any other kind is a
  400); 409 `kind_unavailable`. With a `path`, `GET /bridge/libraries/{id}/book` also answers a `book` block
  (title, authors and narrators split by `names.Split`, series, `series_index`, asin, isbn, duration,
  `work_id` from the metadata cache alone, `CachedWorkID`, and Phase 3c's `ebook {path, size}`, omitted
  with none), never on the sample-book answer; each `files[]` entry carries its indexed `duration`
  (Phase 3c, additive; a single-file book's is the book's).
  Settings > Bridge in `admin-ui` (`features/bridge/`: the topic shows the heartbeat's estimates and
  each generate or describe job's status; the book page's aside and each series card generate characters and
  recaps, a series card queueing those of its books without recaps in reading order). The admin book
  detail carries `generated_description: {text, generated_at?}` (omitted without one; `generated_at` is
  bridge.json's description time; read with the description and provenance columns only, and only when
  the signature names a description), which the book page's Community card shows (`describe-section.tsx`:
  a Description sub-part with the "Generated on this server" mark, the text clamped behind Show more, and
  a note when the player's About shows another description instead: the book's own, or the
  community's; "Write a description" confirms and queues `sidecars.describe` when `describeOffered`:
  `can_describe` and `writable`, local characters or recaps, no description here or from the community
  (and `community_unknown` unset), no open generate (the estimate's `open_job` included), describe or
  chapter detection job; and the card itself adds no generated description shown, no description of the book's own, and
  no estimate being fetched again; its dialog says the player shows it only when the book has no other
  description, and says when the Bridge went away; the describe job's status through `GenerateStatus`
  (a finished one hidden once a description shows), whose done line for a generate job also says
  whether its description came (`result.description`, `description_error`), and whose stage for a
  generate job's description step is `describing`). Notifications word a describe job as the
  description ("Description ready: <book>"). A generate job queued while a describe job is open
  cancels it in the same transaction (`cancelOpenTx`: at once when queued or parked, else
  `cancel_requested`), since the generate run writes the description itself. Notifications word a detect
  job as the chapters ("Chapters found: <book>"; the event carries the book's `chapters_source` once done,
  read after the Bridge's attachments rescan, and the message promises "Listeners see them in the player now."
  only when it is `generated`, else says they were saved but the book uses other chapters; the bell's body
  says so too, `detectUnused`), and its `chapters_not_detected` park (no headings were heard;
  a person's decision) says to add markers or match the book to a recording with a chapter list. The
  console's Health `no_chapters` offers **Detect chapters** (row and bulk; `fixFor` + `detectAvailable`: an
  online Bridge with the `chapters` feature): `health/detect-chapters-dialog.tsx` confirms with the
  transcription time from the Bridge's status (`detectPlan`: its figure and library mappings, one request
  for any selection) and "no agent cost", and queues `chapters.detect` per book, passing over (named in the
  toast by `skipNotes`) one refused `generate_open`, `describe_open` or `chapters_chosen` and one whose
  answer is its own PARKED detect job (`queueInOrder` never counts a returned `needs_attention` job as
  queued: reason `parked`, "waiting for attention on the Bridge"); a row whose `chapter_choice` is set
  (admin rows carry it, additive) offers no Detect, says why (`rowFix`, `detectChosenNote`) and is left out
  of the bar's selection; it stays open while it queues (a close is ignored then), so a run's outcome lands
  in the dialog that started it. The Community card's `GenerateSection` shows a queued or working detect
  job's status (`generateBlocker`: the job list, else the estimate's `open_job`) in place of Generate, and
  the series batch and the generate dialog pass a book refused `detect_open` over by name
  (`GENERATE_SKIPS`) rather than stopping. The book page's Chapters card follows the book's detect job
  (`useBookGenerateJob` with `KIND_DETECT`, on any server that knows the Bridge, metadata or not:
  `detectJobsKnown`) and fetches the book again when it ends; an open detect job counts in the page's job
  poll (`BOOK_KINDS`) and holds back "Write a description" (the Bridge can't describe a book while its
  detection runs or is parked); the Bridge page words them through `GenerateStatus` ("24 chapters found").
  **Read-along** (Phase 3c, feature `readalong`): `readalong.align` makes a book's read-along map
  (`.audiosilo/readalong.json`, see Bridge attachments and Path-addressed API) by speech recognition and
  alignment, no agent; book required, params `{}` or none (`noParams`), writes (`library_not_writable`),
  books libraries only; refused 409 `no_ebook` for a book with no ebook (the queue handler, before
  enqueueing), and 409 `generate_open` / `detect_open` while either is open for the book (parked too:
  a read-along cancels neither; both run over the same audio, one engine book per audio path on the
  Bridge). A NEW `sidecars.generate` or `chapters.detect` is refused 409 `readalong_open`
  (`ErrReadalongOpen`) while a read-along job is open, except that a generate job cancels a PARKED
  one ("make the read-along again after"; so Generate holds back only for a queued or working one); a
  detect job cancels none. Re-aligning a ready map is
  allowed (no bulk action: per book only). `GET /admin/bridge/estimate?kind=readalong.align&library_id=&path=`
  answers `{kind, asr_hours, figures, writable, ebook: {path,size}|null, readalong: Book.readalong|null,
  open_job?}` (`asr_hours` the whole audio at the Bridge's `asr_seconds_per_audio_hour`, the most it
  may take: less when the Bridge kept the book's transcript; `open_job` the book's open read-along job,
  else the generate or detect job it would wait for). Notifications word it "Read-along made: <book>
  (N% lined up)" (the result's `coverage.text` rounded down; the event's `coverage`) and "Read-along
  failed: <book>". The console's book page shows a **Read-along** card for a book with an ebook
  (`features/book/readalong-*`: the map's state, coverage and stale reason, Make / Make again with a
  confirmation quoting "up to N h of transcription, less if the Bridge still has this book's transcript;
  no agent cost", the job's status through `GenerateStatus`, an open generate or detect job's status in
  place of the offer), `KIND_READALONG` counts in the page's job poll, the Community card's Generate
  shows an open read-along in place of the action (`generateBlocker`), and the series batch and Health's
  Detect chapters pass a book refused `readalong_open` over by name. Jobs are carried by
  `bridge.MoveJobs`, which `catalog.moveBookState` calls in its transaction with the catalog's clock (an
  open job colliding with an open one of its kind at the new path is cancelled; `DedupeKey` rebuilds the
  key), and go with their library. **Hidden folders**: no route reaches content through a hidden
  segment by name (`library.ContentPath`, the one statement of the rule: `ContentJoin` for a path that
  names content, `IndexPathWithin`, `listDir`, and `scopedPathIn` for every path-taking route, a
  hidden path reading as missing, 404), so a book's `.audiosilo/` never becomes a phantom book or a
  streamable file; `findCover` never takes a hidden file (an AppleDouble `._x.jpg`) as a cover. Audited `bridge.code`, `bridge.paired` (system),
  `bridge.unpair`, `bridge.job`, `bridge.cancel`. `SetBridge` before `Handler()` (the launcher always calls it).
  **Community jobs (Phase 2b, `bridge/contribute.go`)**: two kinds that file to the community metadata
  database with the Bridge's GitHub sign-in and write nothing into the library (no
  `library_not_writable`). `kindSpec.module` names the managed module a kind needs and `bridge.ModuleFor`
  answers it (also `contributions` for a `sidecars.generate` job with `contribute: "issue"`): the queue
  handler answers **404 `module_off`** for such a job while the module is off, before anything else about
  it, and `Enqueue` refuses one 409 `cannot_contribute` unless `Status.CanContribute()`. While the module
  is off a queued one is **held** (`bridge.Service.ModuleOn`, wired by `SetBridge`): claims pass over it
  (`nextQueued`) until the module is on again, so turning it off leaves its queued jobs at rest (one a
  lapsed lease requeued included); a job the Bridge holds (claimed, running or parked) carries on.
  `sidecars.contribute` (family `sidecars`; "Share with the community" after a local run: the Bridge files
  what its engine book holds): a book `path`, params strict `{work_id?, dimensions?}` (`dimensions` from
  `characters`, `recaps`, each once, stored in that order; absent or empty = both); refused 409
  `generate_open` while a `sidecars.generate` job is open for the book; a NEW generate job is refused 409
  `contribute_open` (`ErrContributeOpen`) while a contribute job is queued or working, and cancels a parked
  one ("replaced what it was sharing"); while `contributions` is off a queued (held) one gives way like a
  parked one: no 409, not the estimate's `open_job`, cancelled (`Service.givesWay`, `Service.held`); the
  console's book page mirrors it with the settings envelope's toggle (`blocksGenerate`).
  Result (the Bridge's): `{library_id, path, work_id, contributed: {characters?, recaps?} (issue URLs),
  covered: [dimensions the community already had]}`. `meta.correct` (new family and feature `meta`;
  "Suggest corrections"): a book `path`,
  params strict `{work_id, recording_id?, asin?, isbn?, region?, changes: [{field, current, corrected}],
  evidence, source?}` (1-25 changes of `bridge.CorrectionFields` = title, author, narrator, series,
  series_index, more_series, published, isbn, each once; values trimmed, one line, at most 500
  characters, `corrected` set and different from `current`; `evidence` required, at most 2000 characters,
  line breaks kept, CRLF stored as LF, and at most 2000 as the issue carries it (`issueLength`: a line
  starting with # loses its indent and gains a backslash); `recording_id`, when given, a community slug
  (`model.ValidSlug`, trimmed); no value holds a control character (`unicode.IsControl`, C1 included;
  nor the evidence one but a line break or a tab) and no value or the evidence a bidirectional
  embedding, override or isolate (U+202A-U+202E, U+2066-U+2069, `isBidiControl`, as audiosilo-meta's
  issueform refuses; ZWJ/ZWNJ and the marks are fine), each looked for as written and as the community
  reads the text (`checkProse` on `decodeRefs`, untrimmed, so a U+0085 a trim would drop before a
  trailing `&#32;` is refused too; its issueform's `formValue`: an HTML character reference ending in
  `;` is the character it names, a whitespace one a space, so `&#x202E;` is refused as U+202E is); a
  `corrected` that so read and trimmed (`asRead`) is empty or `current` (the record's value as it
  spells it) changes nothing; evidence left as `_No response_`, the form's blank, or reading as nothing
  is none), the values stored as sent: what the Bridge or the community would refuse when filing is a
  400 now). Its params cap is its own, 128 KiB (`kindSpec.maxParams`, every other kind 16 KiB as sent): a batch at every bound
  sent by an ASCII-only encoder (each character outside the BMP two `\u` escapes) is about 120 KB, and
  the queue route's body bound follows (`bridge.MaxParamsBytes` + 4 KiB). The cap is on the params as
  sent: `bridge.CheckCorrect` checks it on the api's path (`checkCorrections` re-encodes them before
  Enqueue). As stored they fit the Bridge's own 64 KiB bound (`ParseParams`) at every bound, about
  63.7 KB in the characters `json.Marshal` escapes to six bytes (`TestCorrectStoredParamsFitTheBridge`;
  raise both together). The handler's `checkCorrections` (security-relevant: the server
  files only an admin's own edits) then requires each change's field to be `catalog.SourceEdited` on the
  book (`BookMatchState`; never a scanned, path, ABS or accepted-community value) with `corrected` the
  book's value now (`MatchState.EditedValue`), else **400 `not_edited`** naming the `field`, stores the
  book's own spelling of each value and stamps `source` = `audiosilo-server/<version>` whatever was sent.
  Only the values are bound to the book: the community records the job names (`work_id`, `recording_id`,
  `asin`, `isbn`, `region`) are the caller's (the console sends the book's community match).
  Result: `{issue_url, issue_number, changes, auto: [fields], maintainer: [fields]}`. Notifications word
  them "Shared with the community" / "Corrections sent" (failed: "Sharing with the community failed",
  "Corrections failed"; a contribute job that filed nothing, its result's `contributed` empty and `covered`
  not, `bridge.Job.SharedNothing`: "Already in the community"). Neither kind parks on a credential: the
  Bridge fails one it can't file for want of a GitHub sign-in, to be queued again. The credential parks
  are other kinds', worded alike in the console (`bridge.park.<code>`) and the push
  (`notify.bridgeParkReason`): `agent_credential_refused` (a `sidecars.generate` or `sidecars.describe`
  job's) says to replace the credential where the agent gets it (the Bridge's Accounts tab, or the agent
  container when it has one of its own, which wins), with no Retry: a change on the Accounts tab resumes
  the job at once (the Bridge's `agentCredentialChanged`), one in the agent container at the park's
  `retry_at`. `contrib_unavailable` (a `sidecars.generate` job's, at its `contribute: "issue"` stage) says
  to sign in to GitHub on the Accounts tab, which resumes the job at once (`githubChanged`), and, for a
  Bridge already signed in, to retry the job once GitHub can be reached: the Bridge parks it too when a
  sign-in's token couldn't be refreshed (GitHub unreachable), and nothing resumes that by itself (no
  `retry_at`, no re-admit at a restart).
- **Podcasts (Bridge Phases 2c-1, 2c-2a, 2c-2b and 2c-3)**: libraries have an immutable `kind` (`books`, the default,
  or `podcasts`). Creating or re-rooting either kind may never overlap a podcast root in either
  direction (clean absolute paths, checked under the writer). Config sync logs a root clash and
  skips that entry, keeping its existing library when present, so startup continues. API creates
  and patches still refuse it with 409 `root_overlaps`. Podcast libraries are indexed only
  from regular, non-symlinked `feed.json` v1 files in top-level non-hidden show folders, capped at
  32 MiB and 20,000 episodes. Unknown fields are ignored; a future version skips the show. Episode
  paths are validated, one level below their show, unique and audio-only; no tags, probes,
  deduplication or book metadata heuristics run, and a metadata source change leaves episodes as feed.json
  wrote them. Presence requires a valid committed Fetch generation, matching regular file size, no
  tombstone and no publication journal. Show and attachment listings extend the feed signature, so
  audio and CHAP changes are noticed with unchanged feed stats; targeted rescans force a refresh, as
  does a show with fewer episode rows than its `episode_count` (`PodcastEpisodeRows`: an older server
  scanned the library as books; a row counts only while its `indexed_at` (`0073`, backfilled from the books row; NULL counts) is
  its books row's, so one that older scan rewrote in place counts as missing), and every scan removes the library's books rows that are no episode
  (`PruneNonEpisodeBooks`, that older scan's folder books). A scan prunes only shows indexed before it
  listed the root, so one the Bridge indexes meanwhile stays. Missing
  episodes remain indexed, with `episode` on `/item`, fetch/status handling on `/stream`, and empty
  chapters. `/fs` lists indexed titles (serial shows oldest first). Episodes order by published, season,
  episode number, path (`episodeKeyCols`: undated as '', no season 0, no number -1, so an undated
  serial follows its numbers) for episode pages and their cursor (an older four-value cursor is 400),
  `/fs`, `/next`, the Keep window and the fetch candidates (these put undated episodes last).
  Artwork belongs to the show.
  A library kind this server does not know (`catalog.KnownKind`; a newer server's, left by a
  rollback) is never scanned as books: `Scan` fails with `library.ErrUnknownKind` and a targeted index
  refuses any library that isn't `books` (`resolveBook`, `ErrNotIndexable`).
  `catalog.Caller.Podcasts` comes from this request's credential via `api.callerOf(ctx)` and
  `auth.SeesPodcasts`: Admin console and AudioSilo/dev allowed; released players need
  `PodcastPlayerMinVersion` (2.2.0, the first with podcast views; numeric X.Y.Z, a pre-release
  counts as its base version), so 2.1.x and older stay hidden. Stored clients cover headerless media requests.
  The catalog applies this gate before admin shortcuts, on single and multiple library scopes
  and library lists. The console's views of another person (user detail, a progress edit) take
  that person's access under the viewing request's gate. Background callers and cast tokens keep
  it false. Private shows then narrow
  every player's scope, including admins, to shows they follow. A follow records the feed.json
  `show_id`, so a different show reusing the same path requires fresh proof. A granted library
  with no visible shows still serves empty lists; content paths remain denied. Knowing a feed URL proves access
  by its current or previous normalized SHA-256. Whole-library grants remain the requirement for
  following a new URL. Book search/recent, admin book tools, the console's activity figures and
  listening goals exclude podcast libraries.
  A whole-list replace (`PUT /me/queue`, `PUT /me/collections/{id}/items`) from a caller the gate
  hides podcasts from keeps the stored entries only the gate hides (`catalog.GatedScopes`: their
  podcast scopes as a podcast-seeing client would get them, passed as `keep`), each after the listed
  entry it followed, or all after the list in stored order when the new order would shuffle them; they
  count toward the cap as an add's hidden rows do (oldest evicted). Entries hidden for another reason
  (a revoked share, an unfollowed private show) and every replace from a podcast-seeing caller keep
  the delete rule, so a 2.1.x device reordering Up next can't drop a 2.2.0 player's episodes.
  Core show, episode, item and media reads are always available to scoped callers. Follow,
  unfollow and check actions use the module User wrapper (no kids or demo accounts), JSON bodies
  for URLs, and `modules.podcasts.enabled` (off by default,
  `AUDIOSILO_MODULES_PODCASTS_ENABLED`). Its capability also needs an online Bridge offering
  `podcast`; reads do not. Five open subscribes per user, deduped by URL hash and user. Private
  URLs live only in the Bridge service's volatile memory and its claim reply; stored params keep
  an empty URL (a follow that restores a private show's URL is private, whatever its flag says),
  display labels are redacted, a restart expires unclaimed requests (so does deleting the
  requester's account, `requested_by` NULL: `lostPrivateRequests`, server#179), and deleting a library keeps only
  the URLs of open subscribes (`bridge.PruneSecrets`, under Subscribe's lock, run to the end). Podcast
  errors, progress and results are redacted before storage and never notify. The module scheduler
  folds ended outcomes idempotently, wakes on job endings and sweeps every minute, then queues
  up to ten due followed shows; a job or show that fails is logged and passed over, and a stored
  validator over a poll's 512-byte cap is dropped. Successful cadence clamps to 15 minutes..24 hours; failures back
  off, `gone` stops automatic checks. Secret loss is retained separately from the latest error:
  only a successful subscribe or poll clears it, so a follow by URL can still restore it after
  another failure. Follows and feed status survive index pruning. The Bridge's
  marker route validates `.audiosilo/library.json` without symlinks (4 KiB, version, id, nonce),
  and podcast rescans accept only shows or indexed episodes. Admins cannot enqueue podcast kinds;
  default job lists omit polls. Shared fixtures live in `internal/podcast/testdata` and the byte-identical
  `phase2c2/` copy (manifest hashes are tested).
  Migration 0054 binds feed status to `(library_id,show_path,show_id)`: late or unbound outcomes
  cannot change a replacement show's validators, failures, gone or secret flags. Subscribe repair
  and poll params capture the expected identity. Fetch outcomes have a separate backoff summary
  (30 minutes doubling to 24 hours, larger Retry-After capped at 24 hours); cancellation and
  identity replacement do not count as failures. Fold before pruning terminal fetch/retain history.
  Policies are durable `(library_id,show_path,show_id)` preferences, reset for a replacement,
  included in DB backups, moved with collision handling and cascaded on library deletion. Default
  `{mode:"keep",keep:3,days:7}` applies to old and new shows. Admin-only retention GET/PUT/DELETE
  and explicit fetch use `/admin/libraries/{id}/podcasts/retention|fetch`; writes require the module,
  an online Bridge offering the exact kind and a checked writable mapping. Policy controls are API-only.
  Internal quiet `podcast.fetch` and `podcast.retain` jobs carry IDs, never URLs, credentials or
  listener facts. The scheduler wakes on follows, policies, rescans and job endings, and considers
  unchanged feeds too: at most ten new fetches per pass and two per show, rotated across shows;
  ten retain jobs per pass, no more than hourly per show. Keep fetches newest listed N, Archive
  oldest listed first, On demand no feed-wide downloads. No current accessible followers means
  no automatic feed-wide disk work. On-demand stream/Range/download/transcode triggers share
  `bridge.EnsurePodcastFetch` with player Retry, admin, prefetch and scheduled work. The writer
  transaction deduplicates open work, promotes priority with MAX without changing attempts, checks
  cooldown and authorizes new work against the current online heartbeat/mapping. Priorities are
  100/20/10/-10 respectively; explicit Retry/admin bypass cooldown, while ordinary stream retries
  expose the terminal failure. Existing work can be promoted offline or with an unwritable mapping.
  Each non-admin, including kids, may create at most five open fetch intents across libraries.
  The same writer transaction checks the creator's count before insertion; promotion preserves
  ownership and existing open work still wins at the cap. Admins and requester-free scheduled
  work are exempt. Refusal is an unavailable fetch status with retry_after_seconds 2 and safe
  error code `fetch_limit` ("Too many episodes are being fetched. Try again shortly."). Streams
  keep 503 `podcast_fetch_unavailable`; player fetch POST uses its existing error envelope with
  409 `fetch_limit`. Prefetch silently skips capped new work.
  Absent streams wait within one two-second absolute budget, including DB work, with bounded
  per-path broadcast registrations and a final descriptor recheck. Disconnection releases the waiter
  without cancelling the job. Pending media answers 503 with `podcast_fetching`,
  `podcast_fetch_unavailable` or `podcast_fetch_failed`, `Retry-After: 2`, `Cache-Control: no-store`
  and a public `fetch` summary. Kids with read access may fetch; demo accounts cannot start disk work.
  Core player `GET /libraries/{id}/podcasts/fetch?path=` observes only; strict POST `{path}` retries
  at 100 and answers 202, or 200 when ready. Status always includes state, nullable request_id,
  int64 bytes_received, nullable int64 bytes_total, retry_after_seconds and nullable safe error.
  Episode pages and Book.episode gain optional `fetch`, batched per page. `podcast_fetch:true`
  advertises this protocol even offline; the existing podcasts capability keeps its module meaning.
  One-ahead queue preparation uses the actual request's caller, scopes and private proof, at most
  one candidate per user/current, in bounded detached work; an absent candidate is queued at 10 and a
  queued one (a scheduled -10 intent) raised to 10, never duplicated. Queue mutations reconsider an active
  podcast stream/fetch; removal never cancels shared work. Playing the prepared episode promotes
  the same intent. No player UI or released-client gate change is included.
  Heartbeat replies advertise `claim_min_priority`; negotiated `GET /bridge/jobs/next` accepts a
  signed int32 `min_priority`, filtered in the general claim transaction before attempts or leases
  change. Omission preserves ordinary claims. Bridge worker reservation/claim refresh is owned by
  the companion repo.
  Bridge-token-only retention lookup and strict POST `retention/check` return a no-store aggregate
  from one DB snapshot plus current stream marks. Only enabled, non-demo, matching-ID followers
  with current episode access count, including private follow proof and household grants with
  podcasts explicitly enabled internally. Missing progress or invalid finish timestamps deny
  deletion. Exact favourites protect even after access revocation; a show-folder heart does not.
  Queues, open fetches and live direct/download/transcode requests also protect. Grace starts at
  max(last follower finish, last fetch), plus policy days. Keep deletes only outside its window;
  Archive never deletes. Keep N is a soft download window: unfinished, starred or queued episodes
  can exceed it indefinitely, including when one follower never listens.
  CHAP attachments are `<show>/.audiosilo/<stem>.chapters.json`, validated by ParseChapterList,
  `from=id3_chap`, and the committed single basename/size/duration/fetch timestamp. Times are used
  directly, without generated-book chapter heuristics or alignment. Valid lists expose
  `chapters_source:id3_chap`, never `generated`; no usable CHAP yields one whole-file chapter.
  Missing audio clears files and chapters while retaining the indexed episode and user state.
  Media marks streams before opening, checks current feed identity, stats, journal absence and
  os.SameFile, then keeps that descriptor for the entire direct/download/transcode response.
  Direct bytes use `"podcast-g<generation>-<sha256>"` and private revalidation; transformed bytes
  use an owned descriptor input and no source strong ETag. Ready media remains core while offline
  or module-off. The console uses episode plurals, hides book settings for podcasts, omits those
  keys on podcast saves, and localizes root_overlaps at the folder field in all six locales.
  **Phase 2c-3 (discovery and OPML)**: `internal/podcast/discovery` searches iTunes and Podcast Index
  (`GET /podcasts/discovery/providers|search`, `GET /podcasts/discovery/artwork?id=`) and
  `internal/podcast` follows by URL for both `POST /podcasts/follow`'s `feed_url` and every row of
  `POST /podcasts/opml/import?library_id=` (`Follower.Follow`: module, whole grant, URL stable under a
  second `NormalizeFeedURL` (else `invalid_feed_url`, 400 on the single follow), known show by hash,
  else an online Bridge offering `podcast.subscribe`; the single follow keeps its 200/202 envelopes).
  An open subscribe for the same URL at the other privacy is `bridge.ErrPrivacyConflict` (409
  `privacy_conflict`, an `error` row), checked inside the dedupe transaction. Search, providers, the
  artwork and the import are module User routes plus the client gate (`callerOf(ctx).Podcasts`, 403
  `no_library_access`); the artwork takes the media auth (session/API `?token=`, the media rate limit);
  `GET /podcasts/opml/export` needs only `requireAuth` + `notDemo` + the client gate, module off or
  Bridge offline. Every new GET has an explicit `HEAD` route answering 405 first, every new query rejects
  unknown or repeated parameters (400 `invalid_request`), and JSON answers are `Cache-Control: no-store`.
  Discovery is built only when boot-only `podcasts.discovery.enabled` (default true,
  `AUDIOSILO_PODCAST_DISCOVERY_ENABLED`, no console setting) is on, else its routes are 404
  `discovery_off`. `podcast_discovery` = that AND the live module toggle (false, not absent);
  `podcast_opml` is always true. The Podcast Index pair is env only (`AUDIOSILO_PODCAST_INDEX_KEY` /
  `_SECRET`, read at start, `yaml:"-" json:"-"`, never in settings, logs, audit, cache keys or errors);
  a half or malformed pair turns that provider off with one warning naming the variable. Upstream
  fetches dial through `internal/netguard` (the dialled address checked: providers public :443, artwork
  public any port; no proxy, no cookies, providers no redirects, artwork <= 3 without a downgrade), within
  bounded bodies, per-user and per-provider token buckets (`internal/podcast/quota`), two provider and
  four image slots, a 15 s failure cooldown, coalesced misses and bounded TTL caches. The artwork's
  admission is its own, sized by a results page (BRIDGE-03): a miss past the image slots queues for one
  (up to 40 waiting, each at most the fetch budget, then 503 `discovery_busy`), and its per-user and
  server-wide buckets let two pages through (burst 40, 120 a minute), so a page's artwork asked for at
  once is all served and search's own budgets are untouched; turning the module off
  (`syncModules`) or shutdown calls `Reset`, so nothing publishes afterwards. Artwork is only a URL a
  result registered (`sha256` id), re-encoded to a <= 320 px JPEG (`media.ThumbnailJPEG`). Build tag
  `podcastdiscoverytest` alone honours `AUDIOSILO_TEST_PODCAST_DISCOVERY_ORIGIN=http://127.0.0.1:<port>`
  (both providers and fixture art at that one origin); an untagged binary ignores it.
  `internal/podcast/opml` reads the raw body (application/xml, text/xml, text/x-opml or
  application/x-opml+xml, UTF-8 only, no content coding: else 415 `unsupported_opml_type`) whole, <= 1 MiB,
  with strict tokens: no DOCTYPE, directive or processing instruction but the XML declaration, no
  unknown entity, duplicate attributes refused, outline/nesting/depth/element/attribute caps (413
  `opml_limit`), and the whole document valid (else 400 `invalid_opml`) before any row acts. Rows follow
  in preorder as the caller, at most `ImportNewRequests` (5) new subscribes per upload besides the
  five-open cap (the rest `limited`, the next upload is the retry wave), the module and grant rechecked
  per row. A feed whose last subscribe by the caller here failed within `RecentFailureWindow` (1 h, the
  Bridge service's clock) is an `error` row `recently_failed` (`request_id` the failed request), neither
  queued nor counted (`FollowURL.SkipRecentFailure`, `bridge.RecentSubscribeFailure`); a failure that
  isn't the feed's (`notFeedFailures`: an expired private request, a lost lease, the library or the
  Bridge's storage) never counts, and the single follow always retries. Budgets: import 2/min burst 1 and two at a time server-wide, export 6/min burst 2 (429
  `opml_rate_limited` + Retry-After, 503 `opml_busy` + 1; a busy import is answered before its body is read (`Slots.Busy`) and refunds its token, so the retry runs). The export (`catalog.PodcastExport`: current
  scopes and private narrowing, follows matching the show id, <= 10,000 rows else 413
  `opml_export_limit`; `podcast.BuildExport`; `opml.Render`, <= 4 MiB) never writes a private show,
  dedupes public URLs (lowest library then path) and counts omissions in `X-OPML-*` headers. For an
  allowed origin `cors` exposes those and Retry-After (`corsExposeHeaders`) and grants `Range`
  (`corsAllowHeaders`), so a cross-origin web player reads them; a denied origin gets no grant. Once
  `cors_origins` is set every answer carries `Vary: Origin` (`liveConfig.corsOn`); `timeout` seeds it
  into TimeoutHandler's own header map, so a handler's Vary (spa's Accept-Encoding) adds to it. Shared
  fixtures: the byte-identical `internal/podcast/testdata/phase2c3/` copy.
- **Managed modules (BRIDGE-PLAN.md Phase 2a, `internal/modules`, `api/modules.go`)**: optional features
  that ship **off**, turned on in the console's Settings > Modules without a restart; nothing turns one on
  by itself (pairing a Bridge doesn't). A module is a `modules.Module` value: `ID` (its capability flag,
  config key `modules.<id>.enabled`, setting `modules.<id>`, env `AUDIOSILO_MODULES_<ID>_ENABLED`, console
  copy `settings.module.<id>.name|blurb|needs`), `RequiresBridge` (a Bridge feature from `bridge.features`,
  `""` for none), `Routes(Registrar)` and `Start(ctx, Deps)`, either nil. The registry is
  `modules/builtin.Modules()` (a sub-package, so a module's own `modules/<id>` package imports `modules`
  without a cycle); the launcher's `API.SetModules` refuses to start on a `modules.Validate` error (id
  `^[a-z][a-z0-9_]{0,31}$`, once only, not a core capability name (`coreCapabilities`), its config row
  present: `config.HasModule`). **Adding a module**: its row in `config/settings.go`'s `fields` (+
  `ModulesConfig` field, `config.example.yaml`), its entry in `builtin` (`RequiresBridge` names a `bridge` constant, e.g. `bridge.FeatureContribute`), its console copy in every locale,
  and the player's registry entry (audiosilo-frontend `src/modules/registry.ts`) if listeners see it; a
  module's own settings are setting `<id>.<name>`, key `modules.<id>.<name>`. **Gating**: every route a
  module registers (`Registrar.Admin` = `requireAdmin`, `.User` = `requireAuth` + `notKid` (403 `kid_account`)
  + `notDemo` (403 `demo_account`; the token's lookup loads `is_demo`, so neither reads the account), then
  `requireModule`; a
  pattern must be `"METHOD /api/v1/..."`, else `checkModulePattern` panics at start) answers **404
  `module_off`** while the toggle is off, and is always mounted, so a toggle needs no restart. The routes
  gate on the toggle ONLY: an action needing the Bridge keeps its own 409, so routes don't flap with
  heartbeat timing. The **capability flag** (`modules.Active` over `Module.BridgeState`, the framework's
  rule; `api.moduleActive` feeds it) is the toggle AND the requirement met (an online Bridge whose features
  hold it), `false` (never absent) while off; `GET /server` computes the
  Bridge block once per request and reuses it. **Background work** (`modules.Runner`; the API holds one): `StartModules(ctx)` at launch (beside
  `StartChapterChecks`, with `defer StopModules()`: cancel every run and wait, before the database
  closes), then `syncModules` (`Runner.Sync`) in `handleUpdateSettings` right after the live config swap,
  under the settings mutex: a module turned on starts `Start` in its own goroutine (a panic ends that
  run only; a run that ended by itself while on starts again at the next save), one turned off is
  cancelled without the save waiting; turned back on meanwhile, the new run waits for the old to return
  (a run's `done` closes only after every earlier run's, so however often it is toggled, runs never
  overlap). **`GET /admin/modules`** (admin only):
  `{"modules":[{id, enabled, active, requires_bridge?, bridge_state?}]}` in registry order (`[]` when empty;
  `bridge_state` `offline` | `no_feature` | `ready`, both omitted when no Bridge is needed); the lock is the
  settings envelope's `locked["modules.<id>"]`, not repeated. A toggle is an ordinary settings save
  (`settings.update` audited, `setting_locked` under its variable). **Console**: Settings > Modules
  (`admin-ui` `features/settings/modules-topic.tsx`, `useModules`: polled every 15 s like the Bridge's
  status and refetched after every settings save) shows a card per module (switch, blurb, docs link,
  Bridge state line, the "hidden from listeners" note while enabled but not active); a page a module
  brings goes in `settings-model.ts` `MODULE_PAGES` and `visiblePages` lists it only while the toggle is
  on (a `?topic=` for it opens General). Three modules ship: `contributions` (needs `contribute`, its actions are Bridge community jobs),
  `podcasts` (needs `podcast`, its Start schedules feed checks; see Podcasts above) and `audible` (needs `audible`, its
  Start schedules account polls, its page is Settings > Audible; see Audible accounts below). Every api test's `newTestEnv` mounts the builtin modules through the real `Handler()`, so a bad
  or clashing route pattern fails the tests, not a start.
  `transcode`, `metadata` and `bridge` stay out of the registry (shipped keys and flags, not a plain
  runtime on/off).
- **Audible accounts (BRIDGE-PLAN.md Phase 4a, module `audible`, `catalog/audible.go`, `internal/audible`,
  `bridge/audible.go`, `api/handlers_audible.go`)**: the Bridge signs in to Audible and holds every credential; the
  server only keeps what it reports. `0063`: `audible_accounts` (`account` = the Bridge's id
  `^[a-z0-9]{8,32}$`, label, marketplace = one of `config.Regions`, `user_id` -> users CASCADE, `library_id` ->
  libraries SET NULL, policy `all`|`series`|`manual`, sync, `poll_hours` default 6, `last_poll_at`,
  `last_queued_at`) and `audible_titles` (account CASCADE + asin; title, authors/series JSON, release_date,
  runtime_s, state `available`|`preorder`|`unavailable`|`gone`, reason, the match `match_library_id` /
  `match_path` / `match_how` (`asin`|`fuzzy`): path-keyed, **no FK**, nulled by `DeleteLibrary` (in the
  library's own delete transaction, so no titles call matches into it between), moved with a moved book's
  places (`moveAudibleTitles`, `MoveDurableState`'s per-user transaction) and by a join (`JoinDurableState`, onto the
  joined book); plus the console's later columns rejected,
  ignored, dest_path, import_job_id, which a titles PUT never overwrites). **Paused** = `library_id` NULL
  (the library was deleted, its open poll cancelled with `bridge.CancelAudiblePaused`): the scheduler
  skips it and Check now is 409 `account_paused` until the Bridge maps it again. **Bridge routes** (bridge
  token only, header only, not module-gated, like podcasts'): `GET /bridge/users` (`{users:[{id, username,
  display_name, admin}]}`: `catalog.BridgeUsers`, never a kid member, a demo or a disabled account, and a
  PUT naming one is 404 `unknown_user`); `PUT`/`DELETE /bridge/audible/accounts/{account}` (an upsert in one
  transaction, the library must be a books library (400 `not_books`, 404 `unknown_library`); DELETE cancels
  the account's open poll (`bridge.CancelAudibleRemoved`; both reasons are `IsCancelReason`'s and the
  console's `SERVER_CANCEL_REASONS`), 204 even when unknown); `PUT .../titles` (`{poll_id, final,
  titles:[...]}`, strict JSON, up to `MaxAudibleTitles` a call, a call without its `titles` array 400, never
  "owns nothing"; free text cleaned and bounded, not refused: a title's authors and series past the first 20
  and 10 are dropped; a match path is clean, or `.` for a `book` override on the library root, as the books
  list gives it; a match naming a missing or non-books library is stored as none; `final` marks every title
  this `poll_id` did not send `gone` and sets `last_poll_at`); `GET /bridge/libraries/{id}/books`
  (`{books:[{path, title, authors, narrators, series, asin, isbn, duration}], next}`, path cursor, up to
  500; a main series with no position takes the one the code leading the book's folder name gives,
  `metadata.FolderCodePosition`: "DH06 - ..." is Dragon Heart 6, "LAM05-06" the range "5-6" (an omnibus
  never reads as its first volume), only when the code's letters are the series' initials (all or some,
  in order; "HALO 07" in "Halo" gives none), in this listing only, never the scan's position; the Bridge
  matches under `match.ImportOptions`, see `pkg/match` `Options`);
  `PUT /bridge/libraries/{id}/enrichment?path=` (body `{"asin"}`: `catalog.BackfillASIN`, 204; the
  same ASIN again a no-op; 404 `book_not_found` for no book of a books library there; 409 `asin_set` for
  another ASIN or an admin's ASIN override, a cleared one too; only a write moves the content generation,
  since the Bridge asks again for such a book after every poll).
  **Admin routes** (`moduleRoutes`, 404 `module_off` while off): `GET /admin/audible/accounts` (with title
  counts), `GET /admin/audible/titles` (`account`, `state`, `matched=1|0`, `q` over the title, each author
  as written (`json_each`, never the stored list's JSON) and an exact ASIN, keyset by title then account and
  asin, `after` cursor), `POST /admin/audible/accounts/{account}/poll` (Check now: 201 a new `audible.poll`
  job, 200 the open one, raised to a person's priority (the kind `promotes`), audited as `bridge.job`).
  `DELETE /admin/audible/accounts/{account}` is admin only (`requireAdmin`, the bridge token refused) and
  **not** behind the toggle: it removes an account the Bridge can no longer remove (unpaired, paired again, a
  DELETE that failed), which the scheduler would otherwise poll for good, so it must work after the module is
  turned off. It is the Bridge's delete (`DeleteAudibleAccount`: titles go, the open poll cancelled), 204
  known or not (404 `unknown_account` only for an id the Bridge could never have made), audited as
  `bridge.audible.remove` when there was one. It signs nothing out of Amazon: that stays the Bridge's.
  **Polls**: `audible.poll` is an internal, quiet kind, params `{"account"}`, no library or path, deduped
  per account (`audibleAccountSuffix`), only the newest ended one kept, gated by
  `ModuleFor` (held while the module is off). `audible.RunScheduler` (the module's Start) ticks each minute:
  with an online Bridge that offers the kind, it queues the accounts due (`DueAudiblePolls`: not paused,
  mapped to a person an account may still be mapped to (`bridgeUserSQL`: not one disabled or made a kid
  since), `last_queued_at` older than `poll_hours`) at priority -10 and marks them queued, so a failed poll
  waits its interval.
  **Console**: Settings > Bridge's Audible accounts card (`features/audible/audible-accounts-card.tsx`,
  read-only mapping, links to the Bridge's Accounts tab, Check now and Remove per account) while the module
  is on, and Settings > Audible (`audible-topic.tsx`, a `MODULE_PAGES` page: the titles with account, state
  and on-this-server filters, search, and Check now and Remove for the chosen or only account; Remove
  (`remove-account-button.tsx`) confirms first, saying it removes the account from this server only and that
  the Bridge's Accounts page signs the device out of Amazon); the loaded titles refresh when an account's `last_poll_at` moves, `useTitlesAfterPolls`).
  Job-start actions use the existing Bridge status: unpaired shows a pairing link and disables them;
  unsupported kinds are disabled too. A paired offline Bridge can still queue a kind it offers, with
  guidance that the job waits for it to return. Account settings and removal remain available.
  A scheduled poll reads "Scheduled by the server" in the job list (`serverScheduled`), and the Bridge's
  optional `audible` setup step links its Accounts tab.
- **Audible backups (BRIDGE-PLAN.md Phase 4b, `audible.import`, `catalog/audible_import.go`,
  `audible/imports.go`, `bridge/audible.go`)**: the server decides what to back up and when; the Bridge
  downloads, decrypts for the account and places the book. `0064`: `audible_accounts.template` (the account's
  placement template, `''` = the Bridge's placement beside the series' other books; checked by
  `bridge.CheckAudibleTemplate`, a copy of the Bridge's `placement` grammar: keep `audibleTemplateVars` in
  step with its `knownVars` and `templateMods` with its `parseMods`; no `..` or hidden, dot-led part, which
  the scan would skip), `policy_since` (a free column; backfilled `COALESCE(last_poll_at, created_at)`)
  and `audible_titles.import_pending` (a console Back up on a preorder). **The job**: `audible.import` is
  internal, `writes` (a writable books mapping), not quiet (done and failed are notified, named by the title),
  gated by `ModuleFor`; params `{account, asin, library_id, dest_path, template}` (strict here, read leniently
  by the Bridge; `dest_path` is the title's library-relative file path, or a folder for it when the last
  segment has no audio extension (a template's too), or `''`, `CheckAudibleDestPath`; library and template
  are the account's at queue time), the row's `library_id` set, deduped per `|account|asin`. Priorities: a console Back up 0
  (a preorder's pending request too, when a poll queues it, with no requester), a policy -10 with no
  requester (as the podcast scheduler's fetches), Back up everything -20 (below new episodes: a bulk backup
  in the Bridge's one download slot never holds them back). `SetAudibleImport` records the job on the title
  (`import_job_id`); the open one comes back on a repeat. **On this server** is a match not marked Not this
  book (rejected): Back up, the policy's pending drop, the titles' `matched` filter and the accounts'
  `matched` count read it so. **Done** (`FoldAudibleImport` from `OnEnded`): the
  result (`{library_id, path, files:[{path,size}], bytes, enriched}`; `enriched:false` is no ASIN added: one
  already set, or a file that joined a book that isn't the title's) names the book path `POST /bridge/rescan`
  answered for the placed file; it must be the job's library and an indexed book (`ErrImportNotIndexed`), and
  the title's own (`importOwnBook`, else `ErrImportNotOwn`, logged): a book carrying the title's ASIN, or one
  holding only the files the backup placed (a single-file book is its file), so a file placed into a folder
  book already there (the scan makes it one book) never makes that book, nor another title's, the title's
  match; and never the book the admin marked Not this book for the title. It sets the match
  (`match_how:"import"`), clears rejected and pending, only while `import_job_id` is still this job. A poll
  keeps an `import` match while its book is indexed and the poll names no other. A failure's code is the
  result's `code` (or a `code:` error prefix, or a park's code): `AudibleImportCodes`, shown on the title
  (`import {job_id, state, code}`, `state` a job state or `pending`). **Policy** (`AudiblePolicyImports`,
  after each final titles PUT while the module is on and a Bridge offers the kind; `autoImport.wants`, a
  table test): available, not on this server, not ignored, nothing open; a pending request always; otherwise not
  rejected, not `contested` (`AudibleReasonContested`: the Bridge left it unmatched because another title
  claimed the same book, so the book is here under that title), never attempted, first seen after `policy_since`, and `all`, or `series` with a book in one of
  its series the account's person can see (`seriesOwner`, by series key, `book_more_series` too).
  `policy_since` moves to now when the console turns on `all` or `series` and at the account's first
  complete poll, so the library it already had is never a flood: that is Back up everything's.
  **Preorders**: an account with a preorder a backup may wait for (`preorderWaitingSQL`: not on this server,
  not rejected or ignored, or with a pending request while not here) released by today's UTC date, at most
  14 days ago, is due every hour (`DueAudiblePolls`); past 14 days such a title reads `needs_attention`
  (`preorderOverdue`, the same terms). A pending request survives a restart (a column) and is queued by the
  first poll that reports the title available (a rejected match's too). **Admin routes**
  (`moduleRoutes`, admin session or admin API key; cast, feed and bridge tokens 401):
  `POST /admin/audible/titles/{account}/{asin}/backup` (202 `{pending:false, job_id}`, 200 `{pending:true,
  job_id:null}` for a preorder; 404 `unknown_title`, 409 `already_matched` (matched, not rejected),
  `import_open`, `not_available` (unavailable or gone), `account_paused`; the queue's
  `kind_unavailable` / `library_not_writable`), `POST /admin/audible/accounts/{account}/backup-all`
  (`{queued}`: `backupAllSQL`: available, unmatched, not rejected, ignored or contested, nothing open, the
  latest import not a `done` one whose book is still indexed (a refused fold, `ErrImportNotOwn`, isn't
  queued again; a done one whose book left the library is), a failed or cancelled one again; a console Back up of one contested or done title, and its pending request, still queue), `PATCH /admin/audible/titles/{account}/{asin}` (`{rejected?, dest_path?}`; 400
  `invalid_dest_path`) and `PATCH /admin/audible/accounts/{account}` (`{policy?, template?}`, 400
  `invalid_policy` / `invalid_template`), audited: a queued Back up `bridge.job` (kind `audible.import`; also
  when recording it on the title failed), a pending one `bridge.audible.backup`, then `bridge.audible.backup_all`
  / `title` / `account` (the audit guard reads `moduleRoutes.Admin` routes too). The
  accounts list adds `template`, `policy_since` and `backup_estimate {count, bytes}` (Back up everything's
  titles, runtime at 8000 bytes a second); titles add `import`, `needs_attention`, `planned` and `contested` (see Audible
  unmatched titles below). The Bridge's account
  PUT sets `policy` on create only (the console owns it after). Removing an account cancels its open
  imports. **Console**: per title Back up (off while its account is paused), Not this book (and undo) and
  Destination... (`title-actions.tsx`), the import state and code (`importStatus`; the loaded titles are asked
  again every 5 s while one's backup is queued or running, `backupRunning`); the Bridge's job list names an
  `audible.import` by its ASIN, and its placed files are no verify check (`resultSummary`); per account Account settings... (policy, template) and Back up
  everything... with a confirm "N books, ~X GB" (`backup-all-button.tsx`), and the legal line on the
  titles page and both dialogs.
- **Audible unmatched titles (4b owner fixes, `0065`, `catalog/audible.go`)**: what the console says of a title the
  Bridge left unmatched. `0065` adds `audible_titles.planned` (JSON, `''` none) and `contest_library_id` + `contest_path`
  (NULL and `''` none; the library is the account's at the PUT, and no contest is stored when the title's plan names
  another library: the account was mapped elsewhere while the poll ran), all the poll's: each titles PUT writes them and
  a stored match (the poll's, a kept `import` match, `FoldAudibleImport`) clears them; a title the final call marks
  `gone` loses its plan. **Planned**: each title of the PUT may carry `planned {library_id, path, file, kind, siblings, state,
  conflict, template}` (the Bridge plans the backup's place during `audible.poll`; placement rules and the disk check
  are its own, the server never previews one), checked strictly by `checkAudiblePlan`: library > 0, `path` a clean
  relative file path within `MaxPathBytes` and `file` a name with no `/` (both `""` only when `state` is `error`),
  `kind` `series_existing`|`series_new`|`standalone` (`""` only for `error`), `state` `new`|`joins`|`exists`|`error`,
  `siblings` 0..100000, `template` within `bridge.MaxAudibleTemplate`, `conflict` `file`|`book`|`audio`|`""`, or for
  `error` the reason (cleaned and bounded); anything else is 400 `invalid_request` for the whole call, bar a `path`
  over `MaxPathBytes` (Audible's long text through a template; the Bridge bounds no length), stored as an `error` plan
  instead. The list shows it only while the account is mapped to the plan's library (a paused or remapped account
  shows none until its next poll). **Contested**: `contested_path` (input only, omitempty: a clean book path or `.`, `bridgeBookPath`) is the
  book a title tied for; `GET /admin/audible/titles` (and a single title, the PATCH answer) shows `contested
  {library_id, path, with:[{asin, title}]}` or null: shown only while the account is still mapped to the contest's
  library, `with` the account's other titles (not `gone`) contested over that book or matched to it, by title, at most
  10 (`fillContests`, one query a page). A move or a join carries `contest_path` like a match's path, and a library's
  delete clears its contests (`DeleteLibrary`). **Poll params**: `audible.poll` carries the account's `template` (omitempty;
  `AudiblePoll(account, template, prio)`, from `DueAudiblePolls` and Check now), bounded, never parsed: a template
  the grammar later refuses never stops the polls (the Bridge reports such a plan as an `error`). A request that finds
  the account's poll still queued gives it its params (`promotes`, `bridge.Enqueue`), so Check now after a template
  change plans by the new one; a claimed poll runs as claimed. **Console**:
  `PlannedNote` (`audible-topic.tsx`, `plannedLine`; none on an `unavailable` or `gone` title, which can't be backed
  up): "Will go to {path}, beside N books of its series" and the like,
  "(planned at the last check)", warning tone on `exists` or `error`, a note when the plan's template differs from
  the account's now, and "Your destination is checked when the backup starts" instead while a `dest_path` is set;
  Destination...'s placeholder is the planned path, never its value (`plannedPlaceholder`). `ReasonNote`: `contested`
  explained with the other titles and the book linked (`bookRoute`), a plain sentence without data (this title isn't
  shown as here: the other may well be, when the Bridge kept it as the better match); a reason that is
  a machine code reads "The Bridge couldn't match it to a book on this server" (`reasonKey`); Audible's own words stay.
- **Server settings, system status, updates, logs (admin redesign Phase 5a)**: `internal/config/settings.go`
  is the ONE table of console settings (`fields`): each has an id `<section>.<name>` (also where it sits in
  `GET /admin/settings`), its config.yaml key, its `AUDIOSILO_*` variable, whether it is read only at start
  (`restart`) and its per-field check (`fix`). `applyEnv` walks the same table and records which keys the
  environment set (`fromEnv`); those are **locked** in the console (409 `setting_locked`) and `Save` writes
  config.yaml's own value for them (`file`), so an env value never lands in the file. (The embedding
  launcher's overrides, `Pin` and the `"launcher"` lock were retired in v2.7.0 with the archived desktop
  manager: config.yaml and the environment are the only sources.) `WithSettings` returns a
  validated copy (all or nothing; `SettingError` with reason unknown/read_only/locked/invalid and the setting
  id, mapped by `writeCatalogError` to `invalid_setting`/`unknown_setting`/`setting_read_only`/
  `setting_locked` + `field`). The API never mutates a config: `API.boot` is the config the server started
  with (restart-only settings read it: bind, TLS, web_dir, metadata.base_url, metadata.mode, demo on/off + idle_ttl) and
  `API.live` (`liveConfig`, read via `a.config()`) is swapped whole by a save, carrying the parsed trusted
  proxies and CORS origins, so name, public URL, home address (`lan_url`), CORS, proxies, app links, metadata on/off and the demo
  library/cap apply on the next request. `restart_pending` lists restart settings whose saved value differs
  from `boot`. New keys: `name` (`DisplayName`: GET /server `name`, pairing `server_name`; default
  "AudioSilo") and `update_check` (default true, `AUDIOSILO_UPDATE_CHECK`). The console replaces a hidden
  or invalid topic with the General URL once settings load. Editable cards share one unsaved-draft
  navigation and before-unload guard, cleared by saving or resetting all dirty cards. `GET /admin/system`
  (`handlers_system.go`): tools + versions (`toolfetch.Version`, cached), metadata health (`meta.Service.Ping`:
  metaserve `/healthz`, cached a minute, **only while metadata is on**; in mirror mode through the same
  client as lookups: the copy once ready, the remote service until then) plus `metadata.mode` (the running mode, both modes) and, in mirror mode only,
  `metadata.mirror` (`metamirror.Status`: `state` empty|downloading|opening|ready|error - `opening` while
  a copy is opened (at start, or a download before its swap), `error` only without a usable copy -, tag, built_at, schema_version, schema_newer, size_bytes, checked_at, next_check_at,
  downloaded_at, `progress` {done, total: compressed bytes, total 0 = unknown} while downloading, the last
  `error` kept over a working copy, `fallback` = no usable copy so lookups go to `base_url`).
  `GET /admin/meta/mirror` is the copy's status alone (200 + `metamirror.Status`); `POST
  /admin/meta/mirror/check` wakes the mirror: 202 + its status after the wake (`next_check_at` now).
  Both: 404 `metadata_off` (`metadataOff`) while off, 409 `not_mirror_mode` (coded) without a mirror
  (`mirrorRefused`); the POST is not audited (changes nothing an admin chose).
  The console: Health > System's community metadata row reads `metadata.mirror` while the copy is in
  use (`activeMirror` in `system-model.ts`, also Settings' status line and `metadataDown`), and
  `useMirrorPoll` (Health > System and Settings' status line) polls only `GET /admin/meta/mirror`
  every 2 s while the copy is busy (downloading, opening, checking, or due now), merging it into the
  cached system status and refetching `/admin/system` once when it settles (that stays at 30 s); a
  404 `metadata_off` / 409 `not_mirror_mode` answer stops the poll, drops the copy from the cached
  status and refetches it once. A running check (no `next_check_at`) keeps the Next check fact as
  "Checking now". Settings > Community
  metadata's Source card saves `metadata.mode` (restart). TLS certificates read from their
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
  (`scheduled|manual|before-restore|before-upgrade`), written under a hidden temporary name, owner-only (0600: it holds
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
  restored database's audit log as the server's own act. **Before an upgrade** (UPGRADE-01): next, still
  before `store.Open`, `backup.BeforeUpgrade` asks `store.PendingMigrations` (read-only; empty for no file or
  no migration recorded, so a fresh install gets no copy) and, when migrations are pending, writes a
  `before-upgrade` copy with `store.VacuumFile` plus one INFO line naming them and the copy, keeping the
  newest 3 (`keepBeforeUpgrade`, `pruneBeforeUpgrade`: the copy just made always, whatever the clock
  said when the others were named; a copy holding the same pending migrations as a newer one goes,
  so the retries of a start whose migrations failed part way never push out the copy of the database
  as the older server left it); a failed copy is a warning and the start carries on. **Audit log** (`catalog/audit.go`,
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
  via `auth.ConsumePairing`'s code kind), `updates.Checker.OnAvailable`, `backup.Service.OnFailure`,
  the upcoming releases job (`book_released`, once a run finds releases not announced before: see Upcoming
  releases), `bridge.Service.OnTransition` (`bridge_job_finished` into done, `bridge_job_failed` into failed or into
  a park that isn't transient: `bridge.Job.Notable`, which leaves out `agent_rate_limited` and
  `agent_unavailable` whatever their `retry_at`, and counts a park whose code changes in place as news;
  a failure by an expired last lease is reported too, once its transaction commits; the job's `error`,
  the Bridge's own text that can name its folders, is the bell's `detail`, never sent out).
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
  an earlier session of that person; an admin's `RevokeDevice` or Sign out everywhere
  (`SignOutEverywhere`), a new password or disabling the account forgets it.
- **Support card** (`catalog/support.go`, `api/handlers_support.go`, console `features/overview/support-card.tsx`):
  a quiet Overview card pointing at GitHub Sponsors (`admin-ui/src/lib/support.ts` `SPONSOR_URL`, also the
  account menu's "Support AudioSilo" item, About's link row and the update notice's one line; the
  release footer in `.goreleaser.yml` carries the same line). Admin console only, never the player and no
  capability flag. `GET /admin/support` → `{show}`; `POST /admin/support` `{action: "donated"|"snoozed"}`
  (the same words storage and audit use) → `{show: false, until?}` without recomputing, `until` (RFC 3339)
  only when a snooze was stored; the console's toast words that date. 400 for any other action
  (`catalog.ErrInvalidSupportChoice` in `writeCatalogError`); admin only; audited `settings.support` with
  `choice` and a snooze's `returns_at`, not when nothing changed. The answer is server-wide and **taken
  on trust**: nothing is checked, nothing is sent anywhere, and it unlocks nothing (a donation stays a
  gift). Stored in `server_state` (migration 0040: key/value server facts that are not config.yaml
  settings) through the generic `catalog/server_state.go` accessors `getServerState` (an unreadable value
  counts as none) and `putServerState`; key `support_card`, value `{"choice":"donated"}` or
  `{"choice":"snoozed","until":<formatSessionTime>}`. `SetSupportChoice` reads and writes in one
  transaction so a donation is never downgraded by a later snooze. Due when not donated, no snooze
  running, and the server's age (the earliest non-demo `users.created_at`, so a restored backup keeps its
  age) is >= `SupportAfterDays` (30), or >= `SupportMinDays` (7) with >= `SupportAfterFinished` (10) books
  finished here (finishes dated before that first account, e.g. an Audiobookshelf import's, and demo
  accounts' don't count; the count stops at 10). `SupportSnoozeMonths` 6. Console strings live in one
  `support.*` i18n group (menu, About, card).
- **Library export** (`internal/catalog/export.go` + `api/handlers_export.go`):
  `GET /admin/libraries/{id}/export` (admin only) downloads a library's book list
  as `audiosilo-<library-slug>-<YYYY-MM-DD>.json` - the `{"format":"audiosilo-books",
  "version":1,…}` envelope the community metadata site imports on its Watching page
  so a user can mark which entries of a series they own. **The file leaves the
  server**, so by default it carries bibliographic facts only (title, subtitle, authors, narrators,
  series + position (`series_position` "1-3" for a range: the importer's grammar takes one),
  `series_list` (`[{name, position}]`, main first, only for a book in more than its main series,
  as the player's `Book.SeriesList`), `published`,
  publisher, language, abridged, genres, asin/isbn, runtime_min, chapters; `version` stays 1,
  the meta site's readers ignore keys they don't know) and **never by default** a path, size,
  codec, format or anything else about the filesystem, nor the book's tags (the owner's own notes,
  "Owned", "Read") - a regression test asserts that. `?include=` is a comma list, repeatable
  (`catalog.ExportOptions`; any other value is a 400): `paths` adds each row's `paths` (every copy
  collapsed into the row, index order; the envelope's `library.id` names the library, one export =
  one library) and `tags` its `tags` (every copy's, each once), for the admin's own scripts; such a
  file is named `...-<YYYY-MM-DD>-private.json`, apart from the one to share. `description` adds
  each row's `description` (opt-in for size, not privacy: alone it keeps the shareable name), read
  for the whole library in ONE query (`catalog.descriptions`, like `chapterCounts`), never per
  book. `?copies=separate` (else `merged`, the default; any other value a 400) makes one row per
  indexed book, no merging. The console's "Export with file paths and tags..." asks for
  `include=paths,tags,description&copies=separate` and says the file is then not for a public issue. `catalog.ExportLibraryBooks` composes the envelope (keyset paging over
  `ListBooks`, never OFFSET; copies of the same book within the library collapse on
  `exposedDedupKey`, the kept entry taking each fact from whichever copy has it -
  `mergeExportBook`, since the copy that sorts first may be the untagged rip; the main
  series and its position are one fact, and a merged `series_list` is every series either
  copy lists, the row's main one first, `mergeExportSeries`); the
  handler is transport-only and writes it with a `json.Encoder`. The leak guard
  asserts on the MARSHALLED key sets as an allowlist, so a field added to
  `ExportBook` fails the test even when its name (`format`, `files`) is invisible
  to a substring scan. The single `author`/`narrator` string is split into a list only
  where it clearly holds several names (`;`, ` & `, ` and `, and a comma **only**
  when every part still has two words, so "Alexandre Dumas, pere" stays whole).
  Advertised by the additive `export` capability.
- **Library admin**: a new library (`catalog.CreateLibrary`, every route that adds one) takes the
  next `sort_order` (max + 1), so it lands last in the display order, never in its name's place
  among libraries left at 0 (ADMIN-12). `PATCH /admin/libraries/{id}` edits name/root/default_view and the scan
  settings (`scan_schedule`, `ignore_patterns`) and queues a rescan when the root or ignore rules
  change. A library the config declares (config.yaml; upserted by name at every start) keeps its name and folder: changing
  either is 409 `config_library`, the same values resent (a folder respelled) write nothing, and
  another library renamed or added with a declared name at another folder is 409 `name_taken`
  (the next start would re-root it). `DELETE /admin/libraries/{id}` removes the library
  + its index (files on disk untouched), except a library the config declares: 409
  `config_library` (the next start would add it back empty, everyone's progress in it gone;
  server#176), and the console disables its delete with the reason. Both are surfaced in the
  admin console.
- **User/account admin**: `PATCH /admin/users/{id}` edits role/password/disabled
  in place (one `auth.UpdateUser` transaction); `DELETE /admin/users/{id}`
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
  counts (`catalog.CountBooksByLibrary`, each library with its `kind`) and a cross-user "currently listening"
  feed (`catalog.ListeningOverview`, progress LEFT-joined to books on the path);
  with `?range=` it answers the Activity page's `activity` block instead (see Phase 4a above).
- **Progress reconciliation** is last-write-wins by `updated_at` (version breaks
  ties) in `catalog.SaveProgress` - the realtime layer (Phase C) must reuse it so
  REST and WebSocket writes converge. **Sync v2's rule** (`catalog.ResolvePlace`, pure, table-tested;
  reading progress uses it since Bridge Phase 3b, listening sync since Track W1, see Listening sync): a save carries
  `base_version`, the stored version its device last saw (0 none seen; absent, never diverges). It
  is **diverged** when a row is stored, the base is sent and differs from its version, and another
  device (`device_id`) wrote the row, or the base is 0 whatever the device (a device id outlives
  a client's kept places: signed out and back in, its start saved on 0 must not replace its own
  place further on). A diverged save never moves the place back silently: the
  further `progression` is kept (the save applied, or the stored row kept; a tie keeps the stored
  row) and the place not kept comes back as `other_place` (for reading progress response only, never
  stored; listening sync stores it on the row, see Listening sync), which the
  reader offers ("On {device} you were at {percent}. Jump there?"). Any other save is last-write-wins
  exactly as above (`newerStamp`, which `IsNewer` shares; the save compared at the version it would
  be stored with, so a tie in time applies), a stale one echoing the stored row, so moving back on
  purpose works (listening sync adds its backstop, see Listening sync). The server bumps `version` on every applied write; `updated_at` is stored as
  fixed-width UTC milliseconds (a far-future one replaced by server time) so a move's merge compares
  it as text; `device_name` is the saving token's.
- **Listening sync** (Sync v2, Track W1, capability `listening_sync`, `catalog/listening_sync.go`,
  `api/handlers_listening_sync.go`, migration `0061`): `POST /me/listening/sync` (`requireAuth`: session or API
  key; kids and demo accounts too) takes `{device_id (1-200 bytes), sent_at | clock_offset (exactly one),
  events[0-500]}` and answers `{server_time, results[] (one per event, request order), progress[]}`. Envelope
  400s: `invalid request` (JSON, an unknown top-level field), `device_id is required` / `is
  too long`, `send sent_at or clock_offset` (none, both, unparseable, |offset| > 1e9), code `too_many_events`;
  a body over 1 MiB is 413 code `too_large` (server#284: the player halves its batch on it). A batch with nothing
  to apply opens no writer transaction.
  A bad EVENT never fails the batch: each is decoded on its own, strictly (`DecodeSyncEvents`), so a wrong type,
  an unknown field, or a listen without a number for `from_pos` or `to_pos` (a null would read as 0:00) is that
  event's `rejected` `invalid`, as is a `path` over `MaxPathBytes` (4096 bytes as sent, `relpath.MaxBytes`, server#289;
  the content routes and the routes that store a path answer one with 400 `path is too long`: `pathInScope` (`scopedPath`
  and `scopedPathIn` check first), `pathTooLong` for a route taking a path outside it (the admin's folder override and
  enrichment, browse, a list's remove, the Bridge's book read); before the library is looked up, server#291, so an
  unknown one answers the 400 and an event's over-cap path is `invalid` before any lookup. A podcast's show or episode
  path is capped by `feed.ShowPath` / `EpisodePath`, and the Bridge's enrichment PUT by its own check, each the route's
  400 `invalid_request`. The admin's Bridge estimate and queue, the Bridge's rescan and the jobs and sessions filters
  take no cap: each looks a book up first (an over-cap path finds none) or only filters, storing nothing). The handler
  resolves each distinct
  `(library_id, path)` as PUT progress does (`scopedPathIn`: 403 -> `no_access`, 404 -> `not_found`, a path
  cleaning to nothing or over the cap -> `invalid`; then `progressPath`; then `SyncBookAt`, the book's length and its files'
  offsets in current order), all reads before the one write. `SyncListening` is ONE `WithUserStateTx`: checks
  (`invalid`, `unknown_kind`, an id repeated in the batch `duplicate` of a copy that passed them: a copy rejected
  leaves the next to be checked on its own, server#284), the clock (shift = now - sent_at, kept as
  a Duration, or clock_offset, added to every time; a corrected time past now pulled back with its span, keeping
  its length), `too_old` (older than
  90 days), the fingerprint (only for the exact book path, 10 s), the file remap (fe#188: a `*_file` of the book
  is its offset now + the seconds in it, clamped to its length; else the sent seconds); then each event in
  corrected-time order: an id already stored for the user is `duplicate` (a replay whose answer was lost: when it
  is the newest link of the device's chain on the row, `span_at` at the row's version equal to its stored `ended_at`
  (a finish or unfinish: `finish_changed_at`, a late one keeping the chain's later end), the device's later base-0
  events chain on it; a replay of a span that kept the stored place, or one an edit or
  another writer has since overtaken, seeds nothing); a listen runs `ResolvePlace`
  (whole-book seconds as the progression; in-batch chaining: base 0 on a book this batch wrote resolves on
  that version) and the **backstop**: a `resume`/`continue` span ending more than 60 s before an unfinished
  row is `not_applied` `resume_behind` (with the span's place as `other_place`; one carrying on, within 5 s, from
  its device's offered place moves the offer to where it got to), unless it diverged from a
  place its device had seen (base > 0, another device's row since): that is real listening elsewhere, so
  `ResolvePlace` keeps the further place and the span's is offered and persisted. A diverged place not kept
  is stored on the row (`other_*`; `other_version` = the version written when the event's place is kept, else
  the row's next one, since the row's own device acknowledged its current version before the offer existed)
  unless it is the kept row's device or within 30 s; it is cleared by a non-diverged apply whose base as sent
  (never the in-batch chaining's) is at or after `other_version`, a finish, an edit, or a move/join. A listen writes position, duration, speed, device, `device_name` (the token's),
  `updated_at` (the corrected end, or the row's own when that is later: a diverged apply never moves it back, so
  an old app's PUT dated in between can't win, server#284; the device's later spans then compare with the end
  of its own last span, or of a finish or unfinish of its own after it, `span_at`: the chain below), version + 1, never the finish. finish/unfinish apply only when newer than
  `finish_changed_at` (else `older`): finish marks finished (the position to the end when the length is known,
  `finished_at` kept when already finished, the offer dropped), unfinish clears it and keeps the position, taking
  a finished row for its device (so the restart span after it resolves on that device's own row, not as
  diverged from the device that finished it; an unfinish of another device's unfinished row takes nothing, so
  that device's place stays and this device's next base-0 span still diverges from it); both bump the version.
  A `finish` (applied or `older`, not a furthest-only batch's) marks the token's session on the book finished
  when its last span ended within `SessionGap` before it (`finishSession`, server#284), opening none.
  Every stored listen counts, whatever its place did: `recordSpan` (the token's session begun by the span's
  start with the latest last save, `latestSession`, extended within `SessionGap` or by `continuousPlayback`, its place,
  length and speed taken only from a span not older than its last save, server#284; a
  span from before a household switch continues the session before it; `listenedBetween`'s clip of the span;
  `transcoded` only for a span ending within `livePlaying`) and history (extends the newest row on the book
  that ended 0-60 s before the span's start, 2 s overlap allowed, and within 5 s of its start, however many
  newer rows the book has; else a row; `idx_history_user_path_ended`).
  Rejected events are never stored. `listening_events` is pruned at 120 days (`PruneListeningEvents`, from
  `pkg/launcher.retention`). The legacy PUT stays last-write-wins (finished included) and is logged as a `put`
  event (`SaveProgressFrom`), stores `device_name` and `finish_changed_at` (at its `updated_at`, never past
  the server's clock), clears no offer. Every other writer of `finished` stamps `finish_changed_at` too (the upserts through one
  fragment, `finishChangedValue` / `finishChangedSet`, binding finished as ?6 and the time as ?13), so an
  older sync finish/unfinish can't undo it: `EditProgress` (own and admin PATCH) at now, an import's apply and
  undo (`progressUpsert`) at the row's `updated_at` capped at now (`finishChangeStamp`, the PUT's rule; an undo of a
  row the import had moved on at now, a re-import's too), a move or
  join keeping the newer of the two. Progress reads
  carry `device_name` and `other_place` (omitempty). The chain lasts across POSTs (server#289, `0062`,
  `chainOf`): `writePlace` keeps the span's end on the row as `span_at` at its version (`span_version`; a
  finish or unfinish that leaves the row the device's moves it there, never back, and starts one only when it
  is not older than the row's `updated_at` (a save elsewhere since stays newer), a finish with no row starts it,
  one that leaves another device's row ends it), and while the row is still the device's at that version its next span,
  in this POST or a later one, compares with `span_at`, not the kept `updated_at`. A base-0 span of a later POST
  resolves on the row's version when it continues the chain (`continues`: from within 5 s of the place, or
  starting 0-60 s after `span_at`, 2 s overlap), and so do the POST's base-0 spans after it (in-batch chaining);
  until one does, each is judged on its own, so one that does not still diverges from its own place (a device signed out and
  back in, but also a backlog that resumes after a pause and a seek: one POST would have chained it). Every
  other writer ends the chain (`endSpanChain`: the PUT, which can keep the client's version, `EditProgress`, an
  import's apply and undo, a move or join). An Audible account's batch is furthest only (`SyncBatch.FurthestOnly`, see Audible sync).
- **Chapters are normalized** (`metadata.Chapter`) so single-file m4b chapters and
  multi-file mp3 parts share one shape: each chapter carries `file_path` (the
  library-relative file to stream via `/stream?path=`), in-file `start`/`end`, and
  `book_offset` (its start on the whole-book timeline). For folder books the
  scanner probes each part and, if a part has its own embedded chapters (a single
  chaptered m4b in its own book folder), expands those; otherwise the whole part
  becomes one chapter. `GET /libraries/{id}/chapters?path=` returns
  `{chapters, files, duration}`; a player renders single- and multi-file books
  identically.
- **Community chapters** (`0035`; `internal/chapteralign`, `internal/chaptercheck`,
  `catalog/communitychapters.go`): a book with an ASIN/ISBN is checked against its
  EXACT community recording's chapter list (`meta.Service.RecordingChapters`: the
  lookup's `recording_id`, never `pickRecording`'s first-recording fallback; metaserve
  `works/{id}/recordings/{rid}/chapters`, 404 = none known). The fit is anchored and
  piecewise (see the package doc): local chapter starts pair with community ones by
  title (numbering stripped, word containment, one-letter typos) within
  `max(120 s, 1%)`, file boundaries and untitled starts by time within 5 s of the
  neighbouring anchors' offset; the audio's end pins to the community end or to a
  community boundary near it (what follows, at most `min(20 min, 10%)`, is listed as
  omitted: a preview, credits); stretches between anchors must agree (weighted median
  within 1%, any >= 120 s on either timeline within 10%) or it is another edition; every file boundary
  must be anchored (else `crosses_files`, with the straddling chapter); then each
  placed boundary snaps to the longest pause (>= 0.4 s at -40 dB) within
  `4 s + drift`, starting 0.3 s before the speech. Outcome per book in
  `community_chapters` (path-keyed cache, no FK, moved by `MoveDurableState`; `basis` =
  asin|isbn|duration ms|`books.chapters_hash` (the scan's chapters)|the files in
  play order relative to the book ('' for a single-file book), a different one makes
  it due; a month makes it stale; a check whose basis isn't the book's current one is
  never applied, and the admin page marks it `stale`, so a rescan of other audio,
  re-tagged chapters, files renamed in place or a removed match never brings back
  chapters fitted to the old files. The fit stores its files relative to the book
  too, so a moved book (the same audio) keeps its check and its chapters; `list_hash` names the community list, so a background recheck
  finding the same list for the same audio only renews `checked_at`, while a check
  asked for fits again). A pass the service keeps failing (5 in a row) waits for the
  next tick whatever kicks it, and a book a check failed for waits an hour, so it
  can't hold up the books behind it. A kick waits 5 s for the kicks behind it (a
  scan kicks once per book), so a burst is one pass. The book page's
  `community_check_failed` says the last check failed (the service, or the 4-minute
  pause budget of a 5-minute check: past it, the rest are left unsnapped).
  `refreshEffective` -> `applyChapterSource` puts the community chapters in the
  `chapters` rows when a current check fitted and either the admin chose them
  (`chapter_choices`: `files`|`community`, durable, path-keyed, moved with the book's
  edits by `MoveDurableState`, never copied onto a joined book) or, with no choice,
  the status is `fill` (no chapters of its own: none, or one per file); else the
  scan's own, which `UpsertBook` always keeps in `books.scanned_chapters` (as
  `books.scanned` keeps the metadata; `''` on a row from before 0035, whose rows are
  the scan's). `books.chapters_fit` (a hash of the fit in the rows) keeps an
  unchanged fit from being written again. Chapter renames (`chapter_overrides`, by
  file + start) work on community chapters too. `Catalog.OnBookChange` (fired to every
  listener after `UpsertBook`, `UpsertBooks` (once a transaction), `EditBook(s)`, `BackfillASIN`, `SetCover`/`DeleteCover` commit,
  and a library's new metadata source re-resolving its books) wakes the background pass, so a
  scan, an applied match or a typed ASIN is checked without waiting for the
  10-minute tick. **Generated chapters** (Phase 1d, `catalog/generatedchapters.go`): a book's stored
  `chapters.json` (`metadata.ChapterList`: version 1, 2-2000 chapters on the whole-book timeline, contiguous
  from 0 within a second, titles 1-200 bytes with no control character, 1-2000 files each named relative to
  the book's folder (a single-file book's is its file name) with no dot segment, size and length; anything else
  fails the file) is a source between the community's and the files': `applyChapterSource` uses it
  (`applyGenerated`) only with NO choice, when the community's don't apply, the list's files equal the book's
  (`chaptersOnFiles`: count, book-relative path and size; in the same order, or in another, a list
  found while the book played in byte order before #160, its chapters then moved with their files by the
  listed lengths, `movedChapters`, each running to the next start in the new order) and `chapteralign.Align` with no Prober
  answers `fill` (the book has no chapters of its own: no file holding two); then the rows are the fit,
  `chapters_source = "generated"` (`ChaptersFromGenerated`, never a choice an admin makes: `PATCH
  {chapter_source}` still takes files|community|auto) and `chapters_fit` a hash of what it was fitted from (`generatedKey`: the
  stored list, the scan's own chapters and the files' book-relative paths, sizes and lengths), so a refresh of an
  unchanged book neither decodes nor fits the list again. Leaving it (the file gone,
  other files, a community fill, a files choice) restores the scan's own (`useChapters` is the one write of
  a stand-in list: on a row from before the snapshot it records the rows as the snapshot and, when the book's
  community check was made of the book as it is now, re-stamps that check's basis, for either source, so the
  check stays current; a stale one stays stale). Wire (additive): `chapters_source: "community"` or `"generated"`
  on `/chapters` and the book JSON (an old app shows ordinary chapters with no label); admin book page `chapter_source`, `chapter_choice`,
  `community_chapters` (status, detail, `stale`; not the fitted list),
  `community_checking` (`chapter_source` may be `generated`, which the book page names "Found in the
  narration on this server"), `generated_chapters_fit` (a stored chapters.json fits the files now,
  `generatedFits` via the same `fitGenerated` as `applyGenerated`; omitted otherwise); the Chapters card's
  own source control (`chapterSourceAction`, never beside the community part's own action): "Use the
  file's chapters" over generated ones (PATCH files), "Use automatic" over a choice the book isn't using the
  community's for (PATCH auto; a files choice that keeps a fitting found list out says so); `PATCH .../book {chapter_source}` (bulk too); `POST
  /admin/libraries/{id}/book/community-chapters?path=` (check now, in the
  background; 404 `metadata_off`); Health `detailed_chapters` (status `refine`, no
  choice made); admin rows carry `chapters_source` + `chapters_check` + `chapter_choice` (omitted when automatic). Note the scan
  sets no ASIN from tags (they are only hints; an Audiobookshelf metadata.json does set one): a book is checked once matched.
- ffprobe is optional; code paths must degrade gracefully when it is absent (with it, most files
  are read in-process: see In-process probe)
  (path-derived metadata still works; codec is left unknown → `direct_playable`
  defaults to true). ffmpeg is likewise optional (transcoding off without it).
- **Audible sync (Bridge Phase 4c, `audible.sync`, `catalog/audible_sync.go`, `api/handlers_audible_sync.go`,
  `0066`)**: an account's listening places kept in step with Audible both ways. The console owns the switch
  (`PATCH /admin/audible/accounts/{account}` `{sync}`, audited; off cancels the open sync with
  `bridge.CancelAudibleSyncOff`); `0066` sets every account off, a new account is created off and a Bridge PUT never
  turns it on (its `sync` is read and ignored); a PUT mapping the account to another person turns it off, forgets
  its `last_sync_at` and cancels its open sync (`CancelAudibleSyncOff`: the console turned it on for the person it
  showed then). "Don't sync this book" is `audible_titles.no_sync`
  (`0066`; `PATCH /admin/audible/titles/{account}/{asin}` `{no_sync}`, audited, kept by a poll like `rejected` and
  `ignored`); it is the sync's alone, backups never read it (the registry's `ignored` still keeps a title out of
  backups). A title syncs when `syncTitleSQL`: matched, not rejected, not ignored, not no_sync, not `gone`, not
  `preorder` (bridge#60: the poll may match a preorder to another edition on disk, and Audible holds no place for an
  unreleased ASIN; released, it syncs, the re-match trigger offering its book's place again, `0071`). **Audible to
  here**: `audible/scheduler.go` `queueSyncs` (while the Bridge offers the kind) queues one `audible.sync` per
  `DueAudibleSyncs` account (sync on, not paused, `refused_at` NULL, `bridgeUserSQL`, `sync_queued_at` older than
  `bridge.AudibleSyncEvery`, 15 min) at priority -10, none for an account with no synced title; params
  `{account, titles:[{asin, library_id, path, runtime_s}]}` (strict, `audibleSyncParams`; ASIN once each, a clean
  book path or `.`; at most 1 MiB, `AudibleSync` drops the last titles past it), internal, quiet, `ModuleFor`
  audible, deduped per account, `promotes` (a queued one takes the newest titles), only the newest ended kept
  (`pruneAudibleEnded`). A console change to which titles sync (`PATCH .../titles/...` `{rejected}` or `{no_sync}`)
  or turning sync on queues the account's sync at once (`audible.SyncNow`, `catalog.AudibleSyncQueueable`: the same
  rule bar the 15 min, an online Bridge offering the kind; a title change queues even an empty title list, so the
  Bridge drops a left-out title now; a sync the Bridge already runs keeps its titles). Ended (`OnEnded`, `FoldAudibleSync`): a sync that read Audible's places sets
  `last_sync_at` (`bridge.AudibleSynced`: done, its result neither `waiting` nor `sync_off`, the Bridge's result
  fields, hand-mirrored); failed with code `audible_refused` (result `code`, or an `audible_refused:` error prefix,
  `AudibleSyncCode`; `audible_sign_in`, `bridge.SyncAudibleSignIn`, the Bridge's transient sign-in refusal, never an
  import's, is an ordinary failure) sets `refused_at`, cleared by any account PUT of the Bridge's (a fresh sign-in, an edit) or a
  complete poll (`PutAudibleTitles` final: Audible answered the account), both stamping `refusal_ended_at` (`0071`):
  a refused sync claimed in an earlier second than that (`refusalEndedSince`; `claimed_at` is whole seconds, so one
  in the same second still marks it) is from before the fresh sign-in and marks nothing (server#303). The Bridge posts the places to `POST
  /bridge/audible/accounts/{account}/listening` `{sent_at, events}` (bridge token only, header only; body strict,
  `sent_at` required, the Sync v2 event shapes and limits): the batch is `SyncBatch{UserID: the account's person,
  DeviceID: "audible:<account>", DeviceName: "Audible (<label or STORE>)", TokenID: 0, FurthestOnly: true}`, set by
  the route alone (no body field reaches either; `/me/listening/sync` refuses an unknown field). Each event's book is
  resolved under the PERSON's scopes (`userPathResolver`, `catalog.UserScopes`: no access is `no_access`), then must
  be a synced title's match (`AudibleSyncBooks`, else `rejected` `not_synced`; `rejected` `duration_mismatch`, listen
  and finish alike, when no title on the book has an Audible `runtime_s` within 5% of the longer plus 2 min of the
  book's length, `audibleRuntimeAgrees`: matched to another recording; the length is the indexed duration, else the
  person's own row's, the feed's rule, server#303; either unknown agrees). **FurthestOnly** (`furthestListen`): a
  listen more than 1 s (`furthestSlack`) ahead of the stored place applies over any device's row whatever its time
  (the row's `updated_at` never moves back, its playback speed and offer kept; the player's own reconcile then moves
  or offers, and a device's diverged span offers Audible's place by name); at or behind it, or on a finished row,
  `not_applied` `behind` (no offer, nothing stored on the row), and so is a listen heard at or before the row's
  `finish_changed_at` (a place from before a finish or a restart here is the old reading's); a place past the book's end by more than 5% plus 2
  min (`furthestPastEndRatio`, the manager's wrong-match guard) is `rejected` `duration_mismatch`, one just past it
  is the end; an unfinish is `behind`; a finish is any finish. The device id `audible:<account>` is this route's
  alone: `/me/listening/sync` refuses a batch naming one (400), a legacy PUT naming one (a row echoed back) saves under
  none, and `EditProgress` clears it on the row it edits (the edit reaches the feed, and Audible's next place can't
  undo it unseen), as a listening import does on a row it moves on (`importer.mergeProgress`, the device name too;
  its undo restores Audible's place from `import_progress_prior` under no device either, `progressPrior.restored`, so
  the feed carries the undo to Audible too). No listening session, `listening_daily` or history row is recorded
  (owner decision 2); the
  events are logged with token 0. 200 the Sync v2 answer; 403 `sync_off` (the module or the account's sync off, or
  the account paused: no library), 403 `user_unavailable` (the person disabled, a kid or a demo since:
  `ErrAudibleSyncUser`), 404 `unknown_account` (none, a bad id, or its person deleted: the account cascades). **Here to Audible**: `GET /bridge/progress?account=&since=&limit=` (default 200,
  at most 500; the same 403s and 404, 400 `invalid_request` for no account, a bad limit or cursor) answers
  `{rows:[{library_id, path, asin, position, duration, finished, updated_at, device_id, version}], next, more}`:
  the person's progress rows on the synced titles' books inside their scopes, one per title (two titles on one book
  are two rows), rows whose `device_id` is the account's left out (no echo), a row whose own title's `runtime_s` disagrees
  with the book's length left out (`audibleRuntimeAgrees`, the listening route's `duration_mismatch` checked per
  title: the indexed duration, else the row's stored one, both unknown agreeing; the Bridge checks no length of its
  own), keyset by `(change_seq, asin)` after the opaque cursor `since` (base64url JSON); `next` is
  the last row read, one left out too (since again when none), a feed cursor the Bridge keeps between polls; `more`
  a full page read (its rows may be fewer). `progress.change_seq` (`0066`, `idx_progress_user_change`) is the order
  this server wrote the rows in: drawn from the one-row `progress_change_counter` (strictly increasing, never
  reused, so a deleted row can't hand its number to the next write), set by two triggers on every insert and every
  UPDATE OF position, duration, finished, version, device_id or updated_at (so no write path can miss it; free in
  the user-state rule, the counter's `seq` too), existing rows numbered in updated_at order. Two triggers on
  `audible_titles` (a title inserted with a match; its match changed, synced again (`no_sync`, `rejected`, `ignored`
  cleared, back from gone or released from preorder, `0071`) or its `runtime_s` changed) move the person's row on that
  book to the next number, so a newly matched or re-synced book's existing place (or one left out for a length that may
  agree now) is offered to Audible. A move or join moves the book's titles in its per-user transaction, after the
  places (`moveAudibleTitles`, server#303), so the trigger offers the carried place (merged by the carry's rule with a
  row an earlier book left at the new path), never that left-behind row as it stood, and a carry that fails leaves the
  titles where they were; a person with no place on the moved book still has the left-behind row offered, as the
  player shows it for the moved book. A row written late with
  an earlier `updated_at` (an offline backlog, a diverged apply or late finish keeping the later time, a client's
  clock behind) is still fed; the row's own `updated_at` is what the feed sends. Console:
  Account settings... (`account-settings-button.tsx`, renamed from Backup settings) holds the sync switch, the
  account card's Sync row says off, on and when it last synced, paused with the account, or stopped since Audible
  refused the account; a scheduled sync's job reads "Syncing places with Audible", scheduled by the server; a title
  on this server has Don't sync / Sync again (`title-actions.tsx`, `canSync`) and a Not synced badge.
- **Add from LibriVox (LibriVox L6, `librivox.fetch`, `api/handlers_librivox.go`, `bridge/librivox.go`,
  `catalog/librivox.go`, `meta/librivox.go`)**: an admin adds a LibriVox recording to a books library
  from the community metadata database (never librivox.org); the Bridge downloads and places it. No
  module, no toggle and no `/server` flag: the routes are admin only (`requireAdmin`: a listener's
  session or key 403; no token, bridge, cast and feed tokens 401) and answer 404 `metadata_off`
  while the lookup is off; the console offers it when the Bridge's `features` list `librivox` (the
  family of `librivox.fetch`, so an old server never shows it). **Routes**: `GET
  /admin/librivox/search?q=&lang=&limit=` (metaserve `works/search?source=librivox`, `lang` one
  tag, `limit` 1-50, default 20, each checked here so metaserve's 400 never reads as an outage:
  `{results:[{work_id,title,authors,language,release_date}]}`), `GET /admin/librivox/work?id=`
  (the work's LibriVox recordings only, those with a `librivox_id`, through `fetchWork`, never
  `pickRecording`: `{work:{id,title,authors,language}, recordings:[{recording_id, librivox_id,
  narrators, runtime_min, chapter_count, cover_url, url, in_library:[{library_id,path}], job:{id,
  state}|null}]}`; `in_library` is `catalog.RecordingBooks`, indexed books whose `book_matches`
  row names that exact, non-empty recording of that work; `job` the newest open fetch of the
  project in any library, `bridge.Service.OpenLibrivoxFetches`) and `POST /admin/librivox/fetch
  {library_id, librivox_id}` (202 `{job}`, the open one for the same library and project on a
  repeat; 400 a malformed body, 404 library not found, 404 `not_found` for a project metaserve
  doesn't know, 409 `in_library {library_id, path}` when the recording is on this server in any
  library, 409 `not_book_library`, then the queue's `kind_unavailable` (no Bridge, or one that
  doesn't run it; an offline one still queues) and `library_not_writable`; 502 metadata down;
  audited `bridge.job` when queued). **The job**: `librivox.fetch` is internal, `writes`, a books
  library's, not quiet (a done one is "Added from LibriVox: <title>", named by its result's
  path), no module; params `{librivox_id, work_id, recording_id, library_id}` are the server's,
  strict, the work and recording resolved at queue time by `lookup?librivox_id=` asked alone
  (`meta.Service.LibrivoxRecordingOf`; a lookup with no recording is not found, never a work's
  first recording); deduped per `|<librivox_id>` within the library. The Bridge reads the
  project's manifest from its own metadata service, places `Author/Title/NN - Section.mp3` and
  `cover.jpg` through a dot-led staging folder beside it (the scan skips it), writes
  `.audiosilo/librivox.json` (`{version, librivox_id, work_id, recording_id, archive_id}`, not
  read here yet: `knownAttachment` skips it, so the book has no attachments and no scan error;
  `library.TestScannerToleratesLibrivoxFile`), rescans and reports `{library_id, path,
  librivox_id, files, bytes}`. Failure codes `LibrivoxFetchCodes` (`not_writable`, `exists`,
  `disk_full`, `not_found`, `archive_unavailable`, `metadata_off`). **Done**
  (`catalog.FoldLibrivoxFetch` from `OnEnded`, a content write): the result must name the job's
  own library and project and a book indexed in a books library (else `ErrFetchNotIndexed`,
  logged); it writes the book's `book_matches` row (`how` accepted, `set_by` the requesting admin,
  no run, no offered identifiers) through `writeAnchor`, replacing a stale row at that path (the
  Bridge placed into a vacant folder: a deleted book's leftover) and never another path's; a job
  whose params name no recording writes nothing. `/meta` then answers the book from its recorded
  match (LibriVox L5) with no change. Known limits: a fold lost between the job's commit and the
  fold (a crash), and Clear community matches, drop the identity until the server reads
  `librivox.json` (a follow-up). **Console**: Library > LibriVox (`features/librivox/`).
- **Unavailable-root guard**: the scanner aborts with `ErrLibraryUnavailable`
  (and does NOT prune) if a library root is missing/unreadable, or if it returns
  zero audio files while books are still indexed. This protects the index - and
  the progress/bookmarks that cascade from it - when a network share (SMB/NFS)
  is unmounted. Library roots are always local paths; mount remote shares first.
- **First-run setup (FIRST-RUN-PLAN.md Phase F1, `catalog/setup.go`, `library/preview.go`,
  `library/mounts.go`, `api/handlers_admin_setup.go`)**: the account still comes from the banner or
  `--setup`'s `/setup` page, which now asks only for the account and links "Continue setup" to `/admin`
  (`POST /setup` creates only the admin, 201 `{user}`; the `library_name`/`library_root` pair the archived
  desktop manager sent was retired in v2.7.0, so a body carrying either is 400 and creates nothing). The console's flow then runs on the ordinary admin
  APIs; the server holds only its progress and a folder preview. Admin only, console only (no
  capability). **State**: `server_state` key `setup_state`, `{finished, dismissed, steps: {id:
  "done"|"skipped"}}`, ids `catalog.SetupSteps` (`folder naming metadata reach abs backups phone
  bridge`); no row = a fresh server, not finished; migration `0053` marks a server with a library or a
  second non-demo, non-member account finished and dismissed. `GET /admin/setup` -> `{state, request:
  {host, scheme, proxied, peer?, peer_trusted}, config_libraries, mount_hints}`: `request` is the direct
  connection's Host and scheme (the server never trusts X-Forwarded-Host/Proto), `proxied` any forwarding
  header (`proxied`), `peer` (only when proxied) the connecting address for `trusted_proxies`;
  `config_libraries` the names the config declares (config.yaml); `mount_hints` (only while not finished or
  while the folder step is skipped, else `[]`) `library.MountHints` (Linux
  `/proc/self/mountinfo`: folders, not system paths or pseudo filesystems, not the data folder, sorted,
  at most 20; `[]` elsewhere). `PUT /admin/setup` `{finished?, dismissed?, steps?}` merges (one writer
  transaction), returns the state, audited `settings.setup`; 400 `invalid_setup_step`
  (`catalog.ErrInvalidSetupStep`). **Preview** `POST /admin/setup/preview` `{root, ignore_patterns?}` ->
  `{root, counts: {audio_files, folder_books, file_books, capped}, same_root, samples: [{rel_path, tags,
  path, abs}], blanks: {tags, path}, suggested}`: checks the folder as the folder picker does
  (`library.CheckRoot`: 400 `path_not_absolute`, 404 `folder_unreadable`; library create checks neither),
  the root as library create does (`catalog.CheckLibraryRoot`, the same `checkLibraryRoot` read outside
  the writer: 409 `root_overlaps`) and the ignore rules (400 `invalid_pattern`), and
  `same_root` names a library already at exactly the root (a warning; create allows it). It walks as a
  scan discovers (`discoverAuto`, no overrides, a `walkCap` of 5,000 folders and 10 s: `capped`), reads
  (four at a time, under the preview's 15 s overall: none starts after it, one running then is ended with
  its ffprobe via `metadata.ExtractContext` and left out, fewer samples, `capped` untouched) 8 samples spread evenly over the path-sorted books as a scan reads names (`setNames`: tags over
  `DeriveFromPath`, `readABS`; no fingerprint, chapters or covers), and names each both ways with
  `catalog.ResolveScanned` (the stored book's own `bookLayers.scannedFields`, so preview and library never
  disagree). `blanks` counts, per source on its own, empty title/author/series and a missing position
  beside a series (never the narrator: both modes take it from the tags): the tags before any path
  fallback (a generic title counting as none), the folders as `metadata.FromPathLayout` reads them; the
  columns stay the resolved names, narrator included.
  `suggested` is the source with fewer, the tags on a tie. Read-only: never a file's contents, never an
  index write (in the audit test's `notAudited`).
