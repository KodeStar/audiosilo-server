package api

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
)

const ipKey ctxKey = 1

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
		if a.cfg.TLS.Mode == config.TLSAutocert {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// cors applies a strict allow-list CORS policy. With no configured origins,
// cross-origin browser requests are simply not granted CORS headers (the API
// still works for native apps and same-origin web clients).
func (a *API) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	wildcard := false
	for _, o := range a.cfg.CORSOrigins {
		if o == "*" {
			wildcard = true
		}
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (wildcard || allowed[origin]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
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
	var nets []*net.IPNet
	for _, c := range a.cfg.TrustedProxies {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := peerIP(r.RemoteAddr)
		if isTrusted(ip, nets) {
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

// rateLimit enforces the per-IP token-bucket request rate.
func (a *API) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.ipLimiter.Allow(clientIP(r)) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts a session token from the Authorization header, falling
// back to a `token` query parameter when the header is absent. The query
// fallback exists for browser media elements (<img>/<audio>), which cannot set
// an Authorization header, so the web client can still authenticate GET
// requests for covers and audio streams.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	if t := strings.TrimSpace(r.URL.Query().Get("token")); t != "" {
		return t
	}
	return ""
}

// requireAuth authenticates a session token and injects the user into context.
func (a *API) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		u, err := a.auth.ResolveToken(r.Context(), token, auth.KindSession)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
