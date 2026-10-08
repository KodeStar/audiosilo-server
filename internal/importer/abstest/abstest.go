// Package abstest is a fake Audiobookshelf server for tests: it serves responses
// recorded from a real ABS 2.37.1 (testdata/abs, trimmed of fields the importer
// doesn't read) the way ABS does - Bearer tokens, 401 "Unauthorized" for a bad
// one, 403 "Forbidden" on the users list for a non-admin, 0-based session pages,
// and every route also under the /audiobookshelf prefix. Only tests import it.
package abstest

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

//go:embed testdata/abs/*.json
var fixtures embed.FS

// The recorded users (testdata/abs/users.json).
const (
	RootID = "016cd1cd-770c-4b67-aeb9-2f70203625fc"
	AlexID = "6e3d250f-691e-4fdb-9a6f-d8556ae2e5bc" // 37 sessions over 10 books, 9 progress rows, 3 bookmarks
	JoID   = "cbd5d53e-282b-4b18-82b6-93b0b404f9d4" // 10 sessions over 2 books
)

// Tokens the server accepts: the root user's (admin) and alex's own.
const (
	AdminToken = "admin-token-for-tests"
	AlexToken  = "alex-token-for-tests"
)

// alexPages are alex's recorded session pages (10 a page).
var alexPages = []string{"sessions-alex-p0.json", "sessions-alex-p1.json", "sessions-alex-p2.json", "sessions-alex-p3.json"}

// Server is the fake ABS.
type Server struct {
	*httptest.Server
	// StatusWithToken counts /status requests that carried an Authorization
	// header (the importer must not send its token before ABS is identified).
	StatusWithToken atomic.Int32
	// Requests counts every request.
	Requests atomic.Int32
}

// New starts a fake ABS, closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func file(w http.ResponseWriter, name string) {
	b, err := fixtures.ReadFile("testdata/abs/" + name)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

// sessionPage serves one recorded page of a user's sessions (files, one a page),
// whatever itemsPerPage was asked: an empty page past the end, as ABS does.
func sessionPage(w http.ResponseWriter, r *http.Request, files ...string) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pages := len(files)
	if page < 0 || page >= pages {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 0, "numPages": pages, "page": page,
			"itemsPerPage": 10, "sessions": []any{}})
		return
	}
	file(w, files[page])
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.Requests.Add(1)
	p := strings.TrimPrefix(r.URL.Path, "/audiobookshelf")
	if r.Method != http.MethodGet {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if p == "/status" {
		if r.Header.Get("Authorization") != "" {
			s.StatusWithToken.Add(1)
		}
		file(w, "status.json")
		return
	}
	if !strings.HasPrefix(p, "/api/") {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	var admin bool
	switch r.Header.Get("Authorization") {
	case "Bearer " + AdminToken:
		admin = true
	case "Bearer " + AlexToken:
	default:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
		return
	}
	forbidden := func() { http.Error(w, "Forbidden", http.StatusForbidden) }
	parts := strings.Split(strings.Trim(p, "/"), "/") // api, ...
	switch {
	case p == "/api/users":
		if !admin {
			forbidden()
			return
		}
		file(w, "users.json")
	case p == "/api/me":
		if admin {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + RootID + `","username":"root","type":"root","mediaProgress":[],"bookmarks":[]}`))
			return
		}
		file(w, "me-alex.json")
	case p == "/api/me/listening-sessions":
		if admin {
			sessionPage(w, r)
			return
		}
		sessionPage(w, r, alexPages...)
	case len(parts) == 3 && parts[1] == "users":
		if !admin {
			forbidden()
			return
		}
		switch parts[2] {
		case AlexID:
			file(w, "user-alex.json")
		case JoID:
			file(w, "user-jo.json")
		case RootID:
			_, _ = w.Write([]byte(`{"id":"` + RootID + `","username":"root","type":"root","mediaProgress":[],"bookmarks":[]}`))
		default:
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	case len(parts) == 4 && parts[1] == "users" && parts[3] == "listening-sessions":
		if !admin && parts[2] != AlexID {
			forbidden()
			return
		}
		switch parts[2] {
		case AlexID:
			sessionPage(w, r, alexPages...)
		case JoID:
			sessionPage(w, r, "sessions-jo-p0.json")
		default:
			sessionPage(w, r)
		}
	case p == "/api/libraries":
		file(w, "libraries.json")
	case len(parts) == 4 && parts[1] == "libraries" && parts[3] == "items":
		// Only the recorded libraries: the name never comes from the request.
		switch parts[2] {
		case "a2b98fa9-8cc8-41b5-bcad-46bd14d5f3e5":
			file(w, "items-a2b98fa9-8cc8-41b5-bcad-46bd14d5f3e5.json")
		case "771c554d-8974-4e02-abd2-3551b64fac40":
			file(w, "items-771c554d-8974-4e02-abd2-3551b64fac40.json")
		case "1ddff5ba-787a-4b54-922e-0744e3e5838a":
			file(w, "items-1ddff5ba-787a-4b54-922e-0744e3e5838a.json")
		default:
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}
