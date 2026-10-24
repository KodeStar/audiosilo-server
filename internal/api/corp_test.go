package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// Security-relevant (server#188): a web player served on another site (its
// origin in cors_origins) loads covers and audio as no-cors <img>/<audio>
// requests, which the same-site resource policy would make the browser block.
// With cors_origins set, an authenticated media answer is cross-origin: media
// authenticates only by an explicit token (?token= or Authorization), never an
// ambient cookie. Everything else keeps same-site: media with cors_origins
// unset, a media request that fails to authenticate (a cookie is no
// credential), and every non-media route.
func TestMediaResourcePolicy(t *testing.T) {
	t.Parallel()
	env := func(origins []string) (*testEnv, string, string) {
		e := newTestEnvWith(t, func(c *config.Config) { c.CORSOrigins = origins })
		lib := newFixtureLibrary(t, e)
		tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
		if err != nil {
			t.Fatal(err)
		}
		return e, "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10), tok
	}
	on, onLib, onTok := env([]string{"http://localhost:8081"})
	off, offLib, offTok := env(nil)
	q := "?path=" + url.QueryEscape(cradlePart)
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
		{"?token= stream", on, onLib + "/stream" + q + "&token=" + onTok, "", nil, http.StatusOK, "cross-origin"},
		{"bearer stream", on, onLib + "/stream" + q, onTok, nil, http.StatusOK, "cross-origin"},
		{"?token= cover", on, onLib + "/cover" + q + "&token=" + onTok, "", nil, 0, "cross-origin"},
		// Denied: no explicit token (a cookie is never read), cors_origins unset,
		// and non-media routes.
		{"cookie stream", on, onLib + "/stream" + q, "", cookie, http.StatusUnauthorized, "same-site"},
		{"no token stream", on, onLib + "/stream" + q, "", nil, http.StatusUnauthorized, "same-site"},
		{"bad token stream", on, onLib + "/stream" + q + "&token=nope", "", nil, http.StatusUnauthorized, "same-site"},
		{"cors unset ?token= stream", off, offLib + "/stream" + q + "&token=" + offTok, "", nil, http.StatusOK, "same-site"},
		{"cors unset bearer cover", off, offLib + "/cover" + q, offTok, nil, 0, "same-site"},
		{"api route", on, "/api/v1/me", onTok, nil, http.StatusOK, "same-site"},
		{"library item", on, onLib + "/item" + q, onTok, nil, http.StatusOK, "same-site"},
		{"server info", on, "/api/v1/server", "", nil, http.StatusOK, "same-site"},
	} {
		resp, _ := c.e.doHeaders(t, "GET", c.path, c.bearer, "", c.headers)
		if c.status != 0 && resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.status)
		}
		if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != c.corp {
			t.Errorf("%s: CORP %q, want %q", c.name, got, c.corp)
		}
	}
}
