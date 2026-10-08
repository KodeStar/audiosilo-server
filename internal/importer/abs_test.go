package importer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/importer/abstest"
)

func clientFor(t *testing.T, raw, token string) *Client {
	t.Helper()
	u, err := ParseBaseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(u, token)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestParseBaseURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"http://abs.local:13378", "http://abs.local:13378"},
		{" https://abs.example.com/ ", "https://abs.example.com"},
		{"https://example.com/abs/", "https://example.com/abs"},
		{"http://host:13378/audiobookshelf/library/abc/bookshelf", "http://host:13378/audiobookshelf"},
		{"http://host/audiobookshelf", "http://host/audiobookshelf"},
		{"ftp://host", ""},
		{"host:13378", ""},
		{"http://user:pass@host", ""},
		{"http://host/?token=x", ""},
		{"http://host/#x", ""},
		{"http:///path", ""},
		{"", ""},
	} {
		u, err := ParseBaseURL(tc.in)
		if tc.want == "" {
			if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("%q: err = %v, want ErrInvalidURL", tc.in, err)
			}
			continue
		}
		if err != nil || u.String() != tc.want {
			t.Errorf("%q = %v, %v; want %q", tc.in, u, err, tc.want)
		}
	}
}

// TestClientReadsRecordedABS: the client reads every recorded response, pages
// the sessions to the end without repeats, and never sends the token to /status.
func TestClientReadsRecordedABS(t *testing.T) {
	srv := abstest.New(t)
	c := clientFor(t, srv.URL, abstest.AdminToken)
	ctx := context.Background()
	version, err := c.Status(ctx)
	if err != nil || version != "2.37.1" {
		t.Fatalf("status = %q, %v", version, err)
	}
	users, err := c.Users(ctx)
	if err != nil || len(users) != 3 || users[1].Username != "alex" || users[0].Type != "root" {
		t.Fatalf("users = %+v, %v", users, err)
	}
	u, own, err := c.userDetail(ctx, abstest.AlexID)
	if err != nil || own || len(u.MediaProgress) != 9 || len(u.Bookmarks) != 3 {
		t.Fatalf("user = %+v own=%v, %v", u, own, err)
	}
	sessions, err := c.sessions(ctx, abstest.AlexID, false)
	if err != nil || len(sessions) != 37 {
		t.Fatalf("sessions = %d, %v", len(sessions), err)
	}
	items, err := c.items(ctx)
	if err != nil || len(items) != 44+7+2 {
		t.Fatalf("items = %d, %v", len(items), err)
	}
	if n := srv.StatusWithToken.Load(); n != 0 {
		t.Errorf("the token went to /status %d times", n)
	}
}

// TestClientNonAdminToken: a user's own token can't list users (403), so the
// client falls back on /api/me and its own sessions.
func TestClientNonAdminToken(t *testing.T) {
	srv := abstest.New(t)
	c := clientFor(t, srv.URL+"/audiobookshelf", abstest.AlexToken)
	ctx := context.Background()
	users, err := c.Users(ctx)
	if err != nil || len(users) != 1 || users[0].ID != abstest.AlexID {
		t.Fatalf("users = %+v, %v", users, err)
	}
	u, own, err := c.userDetail(ctx, abstest.AlexID)
	if err != nil || !own || len(u.MediaProgress) != 9 {
		t.Fatalf("user = %+v own=%v, %v", u, own, err)
	}
	if s, err := c.sessions(ctx, abstest.AlexID, own); err != nil || len(s) != 37 {
		t.Fatalf("sessions = %d, %v", len(s), err)
	}
	// Someone else's history is refused.
	_, _, err = c.userDetail(ctx, abstest.JoID)
	wantCode(t, err, CodeUnauthorized)
}

func TestClientBadToken(t *testing.T) {
	srv := abstest.New(t)
	c := clientFor(t, srv.URL, "wrong")
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err) // /status needs no token
	}
	_, err := c.Users(context.Background())
	wantCode(t, err, CodeUnauthorized)
	if strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("the ABS body reached the error: %q", err)
	}
}

func TestClientNotABS(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"other app": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"app":"jellyfin","serverVersion":"10","isInit":true}`))
		},
		"html": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>hello</html>`)) },
		"404":  func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"not set up": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"app":"audiobookshelf","serverVersion":"2.37.1","isInit":false}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			var sawToken bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawToken = sawToken || r.Header.Get("Authorization") != ""
				h(w, r)
			}))
			defer srv.Close()
			_, err := clientFor(t, srv.URL, "secret").Status(context.Background())
			wantCode(t, err, CodeNotABS)
			if sawToken {
				t.Error("the token was sent to a server not known to be ABS")
			}
		})
	}
}

func TestClientUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := clientFor(t, url, "x").Status(context.Background())
	wantCode(t, err, CodeUnreachable)
}

// TestClientRedirects: a redirect on the same host is followed (an http server
// moving /status under its base path); one to another host, or from https down
// to http, is refused before anything is sent there.
func TestClientRedirects(t *testing.T) {
	abs := abstest.New(t)
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			http.Redirect(w, r, "/audiobookshelf/status", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"app":"audiobookshelf","serverVersion":"9","isInit":true}`))
	}))
	defer same.Close()
	if v, err := clientFor(t, same.URL, "x").Status(context.Background()); err != nil || v != "9" {
		t.Fatalf("same-host redirect: %q, %v", v, err)
	}

	// 127.0.0.1 and localhost are one machine but different hosts.
	other := strings.Replace(abs.URL, "127.0.0.1", "localhost", 1)
	cross := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other+r.URL.Path, http.StatusFound)
	}))
	defer cross.Close()
	before := abs.Requests.Load()
	_, err := clientFor(t, cross.URL, abstest.AdminToken).Status(context.Background())
	wantCode(t, err, CodeUnreachable)
	if abs.Requests.Load() != before {
		t.Error("the redirect to another host was followed")
	}
}

// TestClientCapsResponses: a response over its cap is refused, not read whole.
func TestClientCapsResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"users":[` + strings.Repeat(`{"id":"x","username":"y"},`, 1000) + `{}]}`))
	}))
	defer srv.Close()
	var v any
	err := clientFor(t, srv.URL, "x").get(context.Background(), "/api/users", nil, 1024, true, &v)
	wantCode(t, err, CodeFetchFailed)
	if cause := errors.Unwrap(err); cause == nil || cause.Error() != "response too large" {
		t.Errorf("cause = %v, want the size cap", cause)
	}
}

func TestListenedDecoding(t *testing.T) {
	for in, want := range map[string]float64{`30`: 30, `12.5`: 12.5, `"45"`: 45, `null`: 0, `"x"`: 0} {
		var s absSession
		if err := json.Unmarshal([]byte(`{"timeListening":`+in+`}`), &s); err != nil || float64(s.TimeListening) != want {
			t.Errorf("%s = %v, %v; want %v", in, s.TimeListening, err, want)
		}
	}
}

// TestConnectChecksToken: Connect refuses a token Start would refuse (the same
// checkToken) before asking ABS anything, so the admin sees a token error and
// not "unreachable".
func TestConnectChecksToken(t *testing.T) {
	abs := abstest.New(t)
	s := New(nil, nil, nil, nil, nil)
	for _, tok := range []string{"", " \t", "a\r\nX-Injected: 1", "a\x00b", strings.Repeat("t", maxToken+1)} {
		_, _, err := s.Connect(context.Background(), abs.URL, tok)
		var ie *InvalidError
		if !errors.As(err, &ie) {
			t.Errorf("token %q: err = %v, want an InvalidError", tok, err)
		}
	}
	if n := abs.Requests.Load(); n != 0 {
		t.Errorf("a malformed token reached ABS %d times", n)
	}
	if tok, err := checkToken("  " + abstest.AdminToken + "\n"); err != nil || tok != abstest.AdminToken {
		t.Errorf("checkToken trims: %q, %v", tok, err)
	}
}
