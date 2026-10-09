package api

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/web"

	"github.com/kodestar/audiosilo-server/internal/auth"
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
// incl. transcoding), a backup's download and the web player's static asset mount.
func isStreamingPath(p string) bool {
	return isLibraryMedia(p) ||
		isBackupDownload(p) ||
		p == "/web" || strings.HasPrefix(p, "/web/")
}

// isLibraryMedia reports whether p is GET /libraries/{id}/cover or /stream, and
// nothing deeper: a community cover (/libraries/{id}/meta/cover) is a bounded
// thumbnail fetched upstream, so it stays under the timeout.
func isLibraryMedia(p string) bool {
	rest, ok := strings.CutPrefix(p, "/api/v1/libraries/")
	if !ok {
		return false
	}
	id, leaf, ok := strings.Cut(rest, "/")
	return ok && id != "" && (leaf == "cover" || leaf == "stream")
}

// isBackupDownload reports whether p is GET /admin/backups/{name} (a backup's
// download), not the restore beside it, which stays bounded.
func isBackupDownload(p string) bool {
	name, ok := strings.CutPrefix(p, "/api/v1/admin/backups/")
	return ok && name != "" && !strings.Contains(name, "/")
}

// timeout wraps non-streaming handlers in http.TimeoutHandler, which cancels the
// request context at the deadline and emits a 503. Streaming routes pass through
// unbounded (see isStreamingPath).
func (a *API) timeout(next http.Handler) http.Handler {
	timed := http.TimeoutHandler(next, a.timeoutDur, `{"error":"request timed out"}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only reads stream: an upload to a streaming-shaped path (an admin's PUT
		// .../cover) stays bounded, or a slow client could hold it open forever.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && isStreamingPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		timed.ServeHTTP(w, r)
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

// cors applies a strict allow-list CORS policy. With no configured origins,
// cross-origin browser requests are simply not granted CORS headers (the API
// still works for native apps and same-origin web clients).
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && a.config().allowsOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+auth.ClientHeader)
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
// Media routes limit themselves (requireMediaAuth). Everything else spends from
// the general bucket, keyed by address because it runs before authentication: it
// bounds the work done (token lookups included) for a caller nobody has
// identified yet.
func (a *API) rateLimit(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, _ := mux.Handler(r); !web.IsStatic(h, r) && !isMedia(h) && !a.ipLimiter.Allow(clientIP(r)) {
			rateLimited(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func rateLimited(w http.ResponseWriter) {
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}

// bearerToken extracts a session token from the Authorization header. When
// allowQuery is set it additionally accepts the token as a `token` query
// parameter - used ONLY for media GETs (covers/audio), where browser
// <img>/<audio> elements cannot set an Authorization header. The fallback is
// deliberately NOT accepted on other routes: a session token in a query string
// can leak into access logs and Referer headers, so it stays confined to the
// media handlers that actually need it (see requireMediaAuth).
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
func (a *API) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, failure := a.authenticate(r, false)
		if failure != "" {
			writeError(w, http.StatusUnauthorized, failure)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
// share one budget, so one person's cover grid could blank another's. A runaway
// client or a leaked token is still capped: a request its credential's bucket
// refuses has already cost a token lookup, so it pays into the address's bucket
// too, and a client hammering past its budget is soon refused before the lookup.
// Both payments are Charge, not Allow: requests that passed Ready together each
// pay, even past empty, so concurrency can't buy lookups the bucket never paid for.
func (a *API) requireMediaAuth(next http.Handler) http.Handler {
	return mediaHandler{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !a.ipLimiter.Ready(ip) {
			rateLimited(w)
			return
		}
		ctx, failure := a.authenticate(r, true)
		if failure != "" {
			a.ipLimiter.Charge(ip)
			writeError(w, http.StatusUnauthorized, failure)
			return
		}
		if !a.mediaLimiter.Allow(strconv.FormatInt(credentialFrom(ctx).ID, 10)) {
			a.ipLimiter.Charge(ip)
			rateLimited(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})}
}

// mediaHandler marks a route wrapped in requireMediaAuth, so rateLimit can tell
// media from the general API by the handler the mux picks for a request.
type mediaHandler struct{ http.Handler }

func isMedia(h http.Handler) bool {
	_, ok := h.(mediaHandler)
	return ok
}

// authenticate resolves r's credential and returns r's context carrying the user
// and the credential, or the reason for a 401.
func (a *API) authenticate(r *http.Request, allowQueryToken bool) (context.Context, string) {
	token := bearerToken(r, allowQueryToken)
	if token == "" {
		return nil, "missing bearer token"
	}
	// Session OR api key - both authenticate; pairing tokens are excluded, so
	// a QR/pairing secret can never be used as a durable credential here.
	// The request's address and app (X-AudioSilo-Client) are recorded on the
	// token, for the admin console's devices and sessions.
	presence := auth.Presence{IP: clientIP(r)}
	presence.Client, _ = auth.ParseClient(r.Header.Get(auth.ClientHeader))
	u, cred, err := a.auth.ResolveRequest(r.Context(), token, presence, auth.KindSession, auth.KindAPI)
	if err != nil {
		return nil, "invalid or expired token"
	}
	// The credential carries the matched kind, so credential-minting handlers
	// can bar an api key (denyAPIKey) - a leaked key must not spawn a durable
	// credential.
	ctx := context.WithValue(r.Context(), userKey, u)
	return context.WithValue(ctx, credentialKey, cred), ""
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
