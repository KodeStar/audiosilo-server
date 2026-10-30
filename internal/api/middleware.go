package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/web"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// requestTimeout bounds how long a non-streaming request may run. Its purpose is
// resilience, not latency policing: if the single writer connection is held by a
// slow/stuck operation (e.g. a write stalled on a network volume), a request that
// queues for it would otherwise hang forever - there is deliberately no HTTP
// WriteTimeout (audio streams must run long), so nothing would ever release it.
// The deadline cancels the request context (aborting the blocked DB call) and
// returns 503 instead of wedging the connection. It must comfortably exceed the
// slowest legitimate synchronous request (e.g. a large library cascade-delete);
// scan endpoints return immediately since they run the scan in a goroutine.
const requestTimeout = 30 * time.Second

// isStreamingPath reports whether a request path serves a long-lived or large
// body that must NOT be bounded by requestTimeout: a book's media (cover/stream,
// incl. transcoding) or ebook, a backup's download and the web player's static
// asset mount.
func isStreamingPath(p string) bool {
	return isLibraryMedia(p) ||
		isBackupDownload(p) ||
		p == "/web" || strings.HasPrefix(p, "/web/")
}

// isLibraryMedia reports whether p is GET /libraries/{id}/cover, /stream or
// /ebook, and nothing deeper: a community cover (/libraries/{id}/meta/cover) is a
// bounded thumbnail fetched upstream, so it stays under the timeout. The ebook is
// tens of megabytes for an illustrated one, which a slow phone link fetches for
// longer than the request timeout; it stays header-auth only and is no media
// route: it is limited with the authenticated API, per credential (requireAuth).
func isLibraryMedia(p string) bool {
	leaf, ok := libraryLeaf(p)
	return ok && (leaf == "cover" || leaf == "stream" || leaf == "ebook")
}

// libraryLeaf is what follows /api/v1/libraries/{id}/ in p, ok only when p is
// under a non-empty id.
func libraryLeaf(p string) (leaf string, ok bool) {
	rest, ok := strings.CutPrefix(p, "/api/v1/libraries/")
	if !ok {
		return "", false
	}
	id, leaf, ok := strings.Cut(rest, "/")
	return leaf, ok && id != ""
}

// isBackupDownload reports whether p is GET /admin/backups/{name} (a backup's
// download), not the restore beside it, which stays bounded.
func isBackupDownload(p string) bool {
	name, ok := strings.CutPrefix(p, "/api/v1/admin/backups/")
	return ok && name != "" && !strings.Contains(name, "/")
}

// bridgeClaimPath is the Bridge's long-poll claim, which the timeout middleware
// bounds without buffering its response.
const bridgeClaimPath = "/api/v1/bridge/jobs/next"

// timeout wraps ordinary handlers in http.TimeoutHandler, which cancels the
// request context at the deadline and emits a 503. Claims retain the context
// deadline without buffering; streaming routes pass through unbounded.
// TimeoutHandler gives the handler a header map of its own and copies it over
// the real one key by key, so the Vary set outside (cors's Origin) is seeded
// into it first: a handler's own Vary (spa's Accept-Encoding) then adds to it
// instead of replacing it.
func (a *API) timeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == bridgeClaimPath {
			// Claims must observe response write failures before returning, so
			// they can release an undelivered lease. TimeoutHandler buffers the
			// response until after the handler's cleanup has finished.
			ctx, cancel := context.WithTimeout(r.Context(), a.timeoutDur)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Only reads stream: an upload to a streaming-shaped path (an admin's PUT
		// .../cover) stays bounded, or a slow client could hold it open forever.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && isStreamingPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		vary := slices.Clone(w.Header().Values("Vary"))
		http.TimeoutHandler(http.HandlerFunc(func(tw http.ResponseWriter, r *http.Request) {
			for _, v := range vary {
				tw.Header().Add("Vary", v)
			}
			next.ServeHTTP(tw, r)
		}), a.timeoutDur, `{"error":"request timed out"}`).ServeHTTP(w, r)
	})
}

// secureHeaders sets conservative security headers suitable for an API exposed
// to the internet.
func (a *API) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		// HSTS only when we terminate TLS with a real, publicly-trusted cert
		// (autocert). Never for selfsigned: pinning HSTS would make the
		// unavoidable certificate warning impossible to bypass and lock users
		// out. With mode off (behind a reverse proxy) the proxy owns HSTS.
		if a.config().TLS.Mode == config.TLSAutocert {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// corsAllowHeaders are the request headers a cross-origin client may send:
// the bearer token, a JSON or OPML body, the client id (auth.ClientHeader) and
// Range, for a script's fetch of a stream with the Authorization header (the
// player's episode probe; an <audio> element's seeks are no-cors and need none).
const corsAllowHeaders = "Authorization, Content-Type, " + auth.ClientHeader + ", Range"

// corsExposeHeaders are the response headers a cross-origin script may read:
// Retry-After, which every 429 sends (the rate limits, the sign-in lockouts,
// the OPML and discovery budgets) and some 503s (a busy transcode, an episode
// on its way; the request timeout sends none), and the OPML export's counts.
const corsExposeHeaders = "Retry-After, " + headerOPMLExported + ", " +
	headerOPMLOmittedPrivate + ", " + headerOPMLOmittedUnavailable

// cors applies a strict allow-list CORS policy. With no configured origins,
// cross-origin browser requests are simply not granted CORS headers (the API
// still works for native apps and same-origin web clients). Once any origin
// is configured, every answer (no Origin, a denied one, a preflight) carries
// Vary: Origin, so a cache never reuses an answer made without the grant for
// a granted origin's request, or the other way round.
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.config()
		if cfg.corsOn() {
			w.Header().Add("Vary", "Origin")
		}
		origin := r.Header.Get("Origin")
		if origin != "" && cfg.allowsOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// realIP resolves the trustworthy client IP and stashes it in the context. The
// X-Forwarded-For header is honored only when the direct peer is a configured
// trusted proxy, preventing clients from spoofing their IP.
func (a *API) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := peerIP(r.RemoteAddr)
		if isTrusted(ip, a.config().proxies) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				if c := strings.TrimSpace(parts[len(parts)-1]); c != "" {
					ip = c
				}
			}
		}
		ctx := context.WithValue(r.Context(), ipKey, ip)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func peerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func isTrusted(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(ipKey).(string); ok {
		return ip
	}
	return peerIP(r.RemoteAddr)
}

// rateLimit enforces the request rate by route class, read off the handler the
// mux would pick for r. Static files the web package registers (the admin
// console, the web player, the connect page; web.IsStatic) are not counted: they
// are served from memory or, for a player in web_dir, through spa.Files'
// per-version cache, and one cold console page is forty-odd chunk requests.
// Media routes limit themselves (requireMediaAuth), and so do the authenticated
// API routes outside /api/v1/auth/ (requireAuth, tokenKeyed), but an address
// whose general bucket is empty is refused here already, as they would refuse it,
// before the timeout layer's goroutine and buffer. Everything else spends from
// the general bucket, keyed by address because it runs before authentication: it
// bounds the work done (token lookups included) for a caller nobody has
// identified yet, and the sign-in, pairing and account routes, which a token must
// not take out of their address's budget.
func (a *API) rateLimit(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if web.IsStatic(h, r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if isMedia(h) || tokenKeyed(h, pattern) {
			if !a.ipLimiter.Ready(ip) {
				rateLimited(w, a.ipLimiter.Wait(ip))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !a.ipLimiter.Allow(ip) {
			rateLimited(w, a.ipLimiter.Wait(ip))
			return
		}
		// Said in the context, so requireAuth never limits this request a second
		// time, and limits per credential any request this did not limit.
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), addressChargedKey, true)))
	})
}

// addressCharged reports whether rateLimit spent r from its address's bucket.
func addressCharged(r *http.Request) bool {
	charged, _ := r.Context().Value(addressChargedKey).(bool)
	return charged
}

// authRoutes is the prefix of the sign-in, pairing and account routes: limited by
// address whether or not they are authenticated (POST /auth/pair mints a pairing
// secret, /auth/password checks a password, /auth/tokens mints a key), so a
// credential never moves them out of their address's budget.
const authRoutes = "/api/v1/auth/"

// authHandler marks a route wrapped in requireAuth (and so requireAdmin), so
// rateLimit can leave an authenticated route's limit to it (tokenKeyed).
type authHandler struct{ http.Handler }

// tokenKeyed reports whether the route the mux picked (h, its pattern) is limited
// per credential by requireAuth rather than per address by rateLimit: an
// authenticated route outside authRoutes (pattern "POST /api/v1/auth/pair" is in).
func tokenKeyed(h http.Handler, pattern string) bool {
	_, ok := h.(authHandler)
	_, p, _ := strings.Cut(pattern, " ")
	return ok && !strings.HasPrefix(p, authRoutes)
}

// rateLimited writes the request-rate 429 with Retry-After: wait, the time until
// the refused request's buckets have a token again (Wait, spendCredential).
func rateLimited(w http.ResponseWriter, wait time.Duration) {
	setRetryAfter(w, wait)
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}

// bearerToken extracts a session token from the Authorization header. When
// allowQuery is set it additionally accepts the token as a `token` query
// parameter - used ONLY for media GETs (covers/audio), where browser
// <img>/<audio> elements cannot set an Authorization header, and the calendar
// feed, which a calendar app reads by its URL alone. The fallback is
// deliberately NOT accepted on other routes: a session token in a query string
// can leak into access logs and Referer headers, so it stays confined to the
// handlers that actually need it (mediaAuth: requireMediaAuth,
// requireBookMediaAuth and requireFeedAuth).
func bearerToken(r *http.Request, allowQuery bool) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	if allowQuery {
		if t := strings.TrimSpace(r.URL.Query().Get("token")); t != "" {
			return t
		}
	}
	return ""
}

// requireAuth authenticates a session token or a personal API key (Authorization
// header only) and injects the user into context. An API key acts as its owner,
// so it satisfies requireAuth (and requireAdmin, when the owner is an admin)
// exactly like a session; a pairing token is never accepted here.
//
// It also owns the rate limit of its routes outside authRoutes (server#246),
// which rateLimit leaves to it, as mediaAuth does for media: a request that
// authenticates spends from its credential's bucket (tokenLimiter), not its
// address's, so several people behind one address (a household behind NAT, every
// visitor behind a reverse proxy) and a web player loading a whole library each
// have their own budget. Until it is proved, a token costs its address: the
// request is refused while the address's bucket is empty and a failed
// authentication (a missing or refused token) pays into it. Ready reserves
// nothing, so that bounds tokens tried one after another, not at once: requests
// that pass Ready together are each looked up before any of them pays, and with
// the debt floored at -burst, a flood sending more than burst+1 at a time gets
// that many lookups every (burst+1)/rate seconds. A request its credential's
// bucket refuses has cost a lookup too and pays into the address's bucket, so a
// client hammering past its budget is soon refused before any lookup. A request
// rateLimit already charged to its address (authRoutes) is only authenticated.
func (a *API) requireAuth(next http.Handler) http.Handler {
	return authHandler{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limiter := a.tokenLimiter
		if addressCharged(r) {
			limiter = nil // rateLimit charged the address: only authenticate
		}
		if ctx, ok := a.authLimited(w, r, limiter, false, sessionOrAPI, nil); ok {
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})}
}

// authLimited is requireAuth's and mediaAuth's one sequence (server#252):
// refuse while the address's bucket is empty (Ready), authenticate one of
// kinds (queryToken: ?token= too, mediaAuth's GETs only), charge the address for a
// failure, then spend from the credential's bucket in l (allowCredential). With
// l nil the request was already charged to its address (rateLimit's
// authRoutes) and is only authenticated. authed, when set, runs once the
// request has authenticated and before its credential spends, so a header it
// sets is on a 429 too. ok false means the answer (401 or 429) is written.
func (a *API) authLimited(w http.ResponseWriter, r *http.Request, l *rateLimiter, queryToken bool, kinds []string, authed func(context.Context)) (ctx context.Context, ok bool) {
	ip := clientIP(r)
	if l != nil && !a.ipLimiter.Ready(ip) {
		rateLimited(w, a.ipLimiter.Wait(ip))
		return nil, false
	}
	ctx, failure, _ := a.authenticate(r, queryToken, kinds)
	if failure != "" {
		if l != nil {
			a.ipLimiter.Charge(ip)
		}
		writeError(w, http.StatusUnauthorized, failure)
		return nil, false
	}
	if authed != nil {
		authed(ctx)
	}
	if l != nil {
		if wait, allowed := a.allowCredential(ctx, l, ip); !allowed {
			rateLimited(w, wait)
			return nil, false
		}
	}
	return ctx, true
}

// allowCredential spends an authenticated request (ctx) from its credential's
// bucket in l (spendCredential): tokenLimiter for the API, mediaLimiter for
// media. A demo account's session spends from its address's bucket instead, its
// media too, as every API request did before server#246: anyone can mint a demo
// session (POST /demo/session), so a per-token budget would let one address
// multiply its budget by its sessions. A refused demo request pays into that
// bucket anyway, as spendCredential's refusals do: its lookup was already done,
// so requests that passed Ready together can't buy lookups for free. When
// refused, wait is the request's Retry-After.
func (a *API) allowCredential(ctx context.Context, l *rateLimiter, ip string) (wait time.Duration, ok bool) {
	if u := userFrom(ctx); u != nil && u.IsDemo {
		if !a.ipLimiter.Allow(ip) {
			a.ipLimiter.Charge(ip)
			return a.ipLimiter.Wait(ip), false
		}
		return 0, true
	}
	return a.spendCredential(l, credentialFrom(ctx).ID, ip)
}

// spendCredential spends one request from credential id's bucket in l (every
// credential is a tokens row, so its id is its bucket). A refusal charges the
// address (the lookup was already done) and waits the longer of the two
// buckets' waits: the next request meets the address's bucket first, which the
// charge may have put into debt.
func (a *API) spendCredential(l *rateLimiter, id int64, ip string) (wait time.Duration, ok bool) {
	key := strconv.FormatInt(id, 10)
	if l.Allow(key) {
		return 0, true
	}
	a.ipLimiter.Charge(ip)
	return max(l.Wait(key), a.ipLimiter.Wait(ip)), false
}

// requireMediaAuth is requireAuth that additionally accepts the credential as a
// `token` query parameter, for browser media elements that cannot set headers.
// Restrict its use to cover/stream GETs (see bearerToken). An API key works here
// too (it behaves as a session everywhere a session does).
//
// It also owns the media rate limit, which rateLimit leaves to it: a browser grid
// loads every cover as its own request, so sharing the general budget turned a
// fast server's covers into 429s. A media request spends nothing from its
// address's general bucket, but is refused while that bucket is empty and pays
// into it when it fails to authenticate, so unauthenticated media is bounded
// exactly like the general API. One that authenticates spends from its
// credential's media bucket instead: the credential is what the request just
// proved, and several people behind one address (a household behind NAT, or every
// visitor behind a reverse proxy missing from trusted_proxies) would otherwise
// share one budget, so one person's cover grid could blank another's (a demo
// session spends from its address's general bucket, allowCredential). A runaway
// client or a leaked token is still capped: a request its credential's bucket
// refuses has already cost a token lookup, so it pays into the address's bucket
// too, and a client hammering past its budget is soon refused before the lookup.
// Both payments are Charge, not Allow: requests that passed Ready together each
// pay, even past empty, so concurrency can't buy lookups the bucket never paid for.
func (a *API) requireMediaAuth(next http.Handler) http.Handler {
	return a.mediaAuth(next, sessionOrAPI)
}

// requireBookMediaAuth is requireMediaAuth that also accepts a cast token as the
// ?token= (never as a header, only for the token's own book: authenticate).
// Only a book's own media use it: GET /libraries/{id}/cover and /stream, the
// URLs a Chromecast receiver plays.
func (a *API) requireBookMediaAuth(next http.Handler) http.Handler {
	return a.mediaAuth(next, bookMediaKinds)
}

// mediaAuth is requireMediaAuth, requireBookMediaAuth and requireFeedAuth: the
// media rate limit and authentication described on requireMediaAuth, accepting
// the token kinds given.
func (a *API) mediaAuth(next http.Handler, kinds []string) http.Handler {
	return mediaHandler{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ctx, ok := a.authLimited(w, r, a.mediaLimiter, true, kinds, func(ctx context.Context) { a.mediaCrossOrigin(ctx, w) }); ok {
			next.ServeHTTP(w, r.WithContext(ctx))
		}
	})}
}

// mediaCrossOrigin opens an authenticated media answer (ctx) to a cross-site
// no-cors load when its caller is a cast receiver or the server lists CORS
// origins.
func (a *API) mediaCrossOrigin(ctx context.Context, w http.ResponseWriter) {
	cred := credentialFrom(ctx)
	if cred.Kind == auth.KindCast || a.config().corsOn() {
		// secureHeaders' same-site resource policy makes the browser block
		// a no-cors <img>/<audio> load from another site, and two kinds of
		// caller are one. A cast receiver is a web app on gstatic.com; a
		// player the operator serves on another site lists its origin in
		// cors_origins (server#188). No-cors requests carry no Origin to
		// check against that list, so the answer opts out for any caller
		// once one is configured. That is safe because the policy guards
		// against another site loading a resource with the browser's
		// ambient credentials, and media has none: authenticate takes the
		// token only from ?token= or Authorization (bearerToken; no cookie
		// is ever read), so a page that can load the answer already holds
		// the credential that fetches it anywhere.
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	}
}

// mediaHandler marks a route wrapped in mediaAuth (requireMediaAuth,
// requireBookMediaAuth or requireFeedAuth), so rateLimit can tell media from the
// general API by the handler the mux picks for a request.
type mediaHandler struct{ http.Handler }

func isMedia(h http.Handler) bool {
	_, ok := h.(mediaHandler)
	return ok
}

// The token kinds a route accepts. Session OR api key everywhere: both
// authenticate, and pairing tokens are never accepted, so a QR/pairing secret
// can never be used as a durable credential. A cast token only on a book's own
// media (requireBookMediaAuth), and a calendar feed token only on the calendar
// feed, which takes no other kind (feedKinds, requireFeedAuth).
var (
	sessionOrAPI   = []string{auth.KindSession, auth.KindAPI}
	bookMediaKinds = []string{auth.KindSession, auth.KindAPI, auth.KindCast}
)

// authenticate resolves r's credential, one of kinds, and returns r's context
// carrying the user and the credential, or the reason for a 401 with the error
// behind it (nil for no token at all; a refused token's, or a failure to check
// it, which requireBridge tells apart). allowQueryToken also takes the token from
// ?token= (mediaAuth's GETs only, see bearerToken).
func (a *API) authenticate(r *http.Request, allowQueryToken bool, kinds []string) (context.Context, string, error) {
	token := bearerToken(r, allowQueryToken)
	if token == "" {
		return nil, "missing bearer token", nil
	}
	presence := requestPresence(r)
	var (
		u    *auth.User
		cred auth.Credential
		err  error
	)
	if slices.Contains(kinds, auth.KindCast) && r.Header.Get("Authorization") == "" {
		// A cast token is only ever the ?token= of a request with no
		// Authorization header (never a bearer credential, nor accepted beside
		// one), and auth accepts it only for a path below its own book's
		// (castGrant.covers); castOwnBook then refuses a book nested there.
		libraryID, _ := pathInt(r, "id")
		path := catalog.CleanRelPath(r.URL.Query().Get("path"))
		u, cred, err = a.auth.ResolveCastRequest(r.Context(), token, presence, libraryID, path, kinds...)
		if err == nil && cred.Kind == auth.KindCast {
			err = a.castOwnBook(r.Context(), libraryID, path, cred.Book)
		}
	} else {
		u, cred, err = a.auth.ResolveRequest(r.Context(), token, presence, kinds...)
	}
	if err != nil {
		return nil, "invalid or expired token", err
	}
	// The credential carries the matched kind, so credential-minting handlers
	// can bar an api key (denyAPIKey) - a leaked key must not spawn a durable
	// credential.
	return withCredential(r.Context(), u, cred), "", nil
}

// requestPresence is what a request says about its device: its address and app
// (X-AudioSilo-Client), recorded on the token for the admin console's devices and
// sessions.
func requestPresence(r *http.Request) auth.Presence {
	p := auth.Presence{IP: clientIP(r)}
	p.Client, _ = auth.ParseClient(r.Header.Get(auth.ClientHeader))
	return p
}

// castOwnBook refuses a cast token's request for anything but its own book.
// The grant's path rule reaches every path below the book's folder, and a folder
// book's folder can hold other books ("Series" with "Series/Book 2" in it), so
// the request's (clean) path must address the token's book in the index
// (catalog.BookPathAt: the book at it, else the innermost folder book holding
// it). A path the index places in no book is refused too. Security-critical.
func (a *API) castOwnBook(ctx context.Context, libraryID int64, path, book string) error {
	addressed, err := a.cat.BookPathAt(ctx, libraryID, path)
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		a.log.Warn("cast token book check failed", "library", libraryID, "path", path, "err", err)
	}
	if err != nil || addressed != book {
		return auth.ErrInvalidToken
	}
	return nil
}

// requireAdmin is requireAuth plus an admin-role check.
func (a *API) requireAdmin(next http.Handler) http.Handler {
	return a.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r.Context()); u == nil || u.Role != auth.RoleAdmin {
			writeError(w, http.StatusForbidden, "admin only")
			return
		}
		next.ServeHTTP(w, r)
	}))
}
