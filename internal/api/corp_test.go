package api

import (
	"net/http"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Security-relevant (server#188): a web player served on another site (its
// origin in cors_origins) loads covers and audio as no-cors <img>/<audio>
// requests, which the same-site resource policy would make the browser block.
// With cors_origins set, an authenticated media answer is cross-origin: media
// authenticates only by an explicit token (?token= or Authorization), never an
// ambient cookie. Everything else keeps same-site: media with cors_origins
// unset, a media request that fails to authenticate (a cookie is no
// credential), and every non-media route. The fixture book has no cover art,
// so an authenticated cover answers 404 (a refused one 401).
func TestMediaResourcePolicy(t *testing.T) {
	t.Parallel()
	env := func(origins []string) (*testEnv, int64, string) {
		e := newTestEnvWith(t, func(c *config.Config) { c.CORSOrigins = origins })
		return e, newFixtureLibrary(t, e).ID, adminSession(t, e)
	}
	on, onLib, onTok := env([]string{"http://localhost:8081"})
	off, offLib, offTok := env(nil)
	cookie := map[string]string{"Cookie": "token=" + onTok + "; session=" + onTok}

	for _, c := range []struct {
		name         string
		e            *testEnv
		path, bearer string
		headers      map[string]string
		status       int
		corp         string
	}{
		// Allowed: an explicit token with cors_origins set.
		{"?token= stream", on, mediaURL(onLib, "stream", cradlePart, onTok), "", nil, http.StatusOK, "cross-origin"},
		{"bearer stream", on, bookURL(onLib, "stream", cradlePart), onTok, nil, http.StatusOK, "cross-origin"},
		{"?token= cover", on, mediaURL(onLib, "cover", cradlePart, onTok), "", nil, http.StatusNotFound, "cross-origin"},
		// Denied: no explicit token (a cookie is never read), cors_origins unset,
		// and non-media routes.
		{"cookie stream", on, bookURL(onLib, "stream", cradlePart), "", cookie, http.StatusUnauthorized, "same-site"},
		{"no token stream", on, bookURL(onLib, "stream", cradlePart), "", nil, http.StatusUnauthorized, "same-site"},
		{"bad token stream", on, mediaURL(onLib, "stream", cradlePart, "nope"), "", nil, http.StatusUnauthorized, "same-site"},
		{"cors unset ?token= stream", off, mediaURL(offLib, "stream", cradlePart, offTok), "", nil, http.StatusOK, "same-site"},
		{"cors unset bearer cover", off, bookURL(offLib, "cover", cradlePart), offTok, nil, http.StatusNotFound, "same-site"},
		{"api route", on, "/api/v1/me", onTok, nil, http.StatusOK, "same-site"},
		{"library item", on, bookURL(onLib, "item", cradlePart), onTok, nil, http.StatusOK, "same-site"},
		{"server info", on, "/api/v1/server", "", nil, http.StatusOK, "same-site"},
	} {
		resp, _ := c.e.doHeaders(t, "GET", c.path, c.bearer, "", c.headers)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.status)
		}
		if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != c.corp {
			t.Errorf("%s: CORP %q, want %q", c.name, got, c.corp)
		}
	}
}
