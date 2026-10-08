// Package importer imports listening history from another audiobook server
// (v1: Audiobookshelf) into AudioSilo: it fetches one ABS user's sessions,
// progress and bookmarks (abs.go, payload.go), matches their books to the
// books here (match.go), plans what an import writes (plan.go) and runs the
// fetch in the background and the review, apply and undo (service.go). The
// rows themselves are catalog's (catalog/imports.go). Admin-only in v1.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The ABS client is read-only (GETs only) and careful with the address an admin
// typed: http/https only, /status must say it is Audiobookshelf before the token
// is sent anywhere, redirects stay on the same host (and never drop to http),
// every response is size-capped and every request times out. Errors are coded
// (Error) and their messages are fixed sentences: an ABS response body never
// reaches a caller or a log. It does not refuse private or loopback addresses
// (an ABS on the LAN is the usual case): the routes that use it are admin-only.

// Error codes (catalog.Import.ErrorCode, and the users endpoint's 502 codes).
const (
	CodeUnreachable  = "abs_unreachable"
	CodeUnauthorized = "abs_unauthorized"
	CodeNotABS       = "not_abs"
	CodeFetchFailed  = "fetch_failed"
)

// Error is a failure talking to ABS: a code and a safe English sentence. The
// cause (a network error, never a body) is kept for the server's log.
type Error struct {
	Code string
	Msg  string
	err  error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.err }

func absErr(code string, cause error) *Error {
	msg := map[string]string{
		CodeUnreachable:  "Could not reach the Audiobookshelf server.",
		CodeUnauthorized: "Audiobookshelf refused the API token.",
		CodeNotABS:       "That address is not an Audiobookshelf server.",
		CodeFetchFailed:  "Audiobookshelf sent something the import could not read.",
	}[code]
	return &Error{Code: code, Msg: msg, err: cause}
}

// ErrInvalidURL is an ABS address that isn't an absolute http(s) URL (or carries
// credentials, a query or a fragment).
var ErrInvalidURL = errors.New("the address must be an http or https URL")

// Response size caps. The library items list is the big one (about 1.5 KB an
// item, so 256 MB is far past any real library).
const (
	capSmall    = 1 << 20   // /status
	capUsers    = 16 << 20  // the users list, /api/libraries
	capUser     = 64 << 20  // one user, with all their progress and bookmarks
	capSessions = 64 << 20  // one page of sessions
	capItems    = 256 << 20 // one library's items
)

// sessionsPerPage is how many sessions one request reads.
const sessionsPerPage = 200

// Client reads one ABS server with one token.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// ParseBaseURL checks an ABS address and normalizes it: no trailing slash, and
// a pasted web-app link ("http://host/audiobookshelf/library/...") cut back to
// the "/audiobookshelf" base it was served under (ABS answers the API under it
// as well as without it).
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, ErrInvalidURL
	}
	if i := strings.Index(u.Path+"/", "/audiobookshelf/"); i >= 0 {
		u.Path = u.Path[:i+len("/audiobookshelf")]
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// NewClient returns a client for the ABS server at base (ParseBaseURL's) with
// token, an API key or a user's token (sent as a Bearer token, only after
// /status has said the server is ABS).
func NewClient(base *url.URL, token string) *Client {
	host, secure := base.Hostname(), base.Scheme == "https"
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Minute
	tr.TLSHandshakeTimeout = 15 * time.Second
	return &Client{base: base, token: token, http: &http.Client{
		Transport: tr,
		Timeout:   3 * time.Minute, // the items list of a big library is slow to build
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !strings.EqualFold(req.URL.Hostname(), host) {
				return errors.New("redirected to another host")
			}
			if secure && req.URL.Scheme != "https" {
				return errors.New("redirected from https to http")
			}
			return nil
		},
	}}
}

// get GETs path (with query) and decodes the JSON into dst, reading at most
// limit bytes. authed sends the token.
func (c *Client) get(ctx context.Context, path string, query url.Values, limit int64, authed bool, dst any) error {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return absErr(CodeFetchFailed, err)
	}
	req.Header.Set("Accept", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return absErr(CodeUnreachable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &statusError{code: resp.StatusCode, err: absErr(CodeUnauthorized, nil)}
	case resp.StatusCode >= 500:
		return absErr(CodeUnreachable, fmt.Errorf("status %d", resp.StatusCode))
	default:
		return &statusError{code: resp.StatusCode, err: absErr(CodeFetchFailed, fmt.Errorf("status %d", resp.StatusCode))}
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, limit)).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return absErr(CodeFetchFailed, errors.New("response too large"))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return absErr(CodeFetchFailed, errors.New("undecodable response"))
	}
	return nil
}

// statusError is an unexpected HTTP status, so a caller can fall back on a 403
// or 404 (its Error is the coded *Error).
type statusError struct {
	code int
	err  *Error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func hasStatus(err error, code int) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == code
}

// Status checks that the server is Audiobookshelf (GET /status, sent without the
// token) and returns its version.
func (c *Client) Status(ctx context.Context) (string, error) {
	var st struct {
		App           string `json:"app"`
		ServerVersion string `json:"serverVersion"`
		IsInit        bool   `json:"isInit"`
	}
	if err := c.get(ctx, "/status", nil, capSmall, false, &st); err != nil {
		var ae *Error
		if errors.As(err, &ae) && ae.Code != CodeUnreachable {
			return "", absErr(CodeNotABS, ae)
		}
		return "", err
	}
	if st.App != "audiobookshelf" || !st.IsInit {
		return "", absErr(CodeNotABS, nil)
	}
	return st.ServerVersion, nil
}

// absUser is an ABS user as the users list names it.
type absUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Type     string `json:"type"` // root | admin | user | guest
}

// Users lists the server's users (an admin token), or with a user's own token
// just that user (ABS answers 403 to the list, and /api/me is theirs).
func (c *Client) Users(ctx context.Context) ([]absUser, error) {
	var out struct {
		Users []absUser `json:"users"`
	}
	err := c.get(ctx, "/api/users", nil, capUsers, true, &out)
	if hasStatus(err, http.StatusForbidden) {
		var me absUser
		if err := c.get(ctx, "/api/me", nil, capUser, true, &me); err != nil {
			return nil, err
		}
		return []absUser{me}, nil
	}
	return out.Users, err
}

// absProgress is one ABS mediaProgress entry.
type absProgress struct {
	LibraryItemID string   `json:"libraryItemId"`
	EpisodeID     *string  `json:"episodeId"`
	MediaItemType string   `json:"mediaItemType"`
	Duration      float64  `json:"duration"`
	CurrentTime   float64  `json:"currentTime"`
	IsFinished    bool     `json:"isFinished"`
	LastUpdate    float64  `json:"lastUpdate"`
	StartedAt     float64  `json:"startedAt"`
	FinishedAt    *float64 `json:"finishedAt"`
}

// absBookmark is one ABS bookmark (no id: it is keyed by item and time).
type absBookmark struct {
	LibraryItemID string  `json:"libraryItemId"`
	Time          float64 `json:"time"`
	Title         string  `json:"title"`
	CreatedAt     float64 `json:"createdAt"`
}

// absUserDetail is one user with their progress and bookmarks.
type absUserDetail struct {
	absUser
	MediaProgress []absProgress `json:"mediaProgress"`
	Bookmarks     []absBookmark `json:"bookmarks"`
}

// userDetail reads user id with their progress and bookmarks: GET
// /api/users/{id}, or /api/me when the token is that user's own.
func (c *Client) userDetail(ctx context.Context, id string) (*absUserDetail, bool, error) {
	var u absUserDetail
	err := c.get(ctx, "/api/users/"+url.PathEscape(id), nil, capUser, true, &u)
	if hasStatus(err, http.StatusForbidden) {
		u = absUserDetail{}
		if err := c.get(ctx, "/api/me", nil, capUser, true, &u); err != nil {
			return nil, false, err
		}
		if u.ID != id {
			return nil, false, absErr(CodeUnauthorized, errors.New("the token can't read that user"))
		}
		return &u, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &u, false, nil
}

// absMetadata is a book's metadata: a minified item's (authorName, seriesName as
// strings) or an expanded item's / a session snapshot's (authors, series arrays).
type absMetadata struct {
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle"`
	AuthorName string `json:"authorName"`
	SeriesName string `json:"seriesName"`
	Authors    []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Series []struct {
		Name     string `json:"name"`
		Sequence string `json:"sequence"`
	} `json:"series"`
	ASIN string `json:"asin"`
	ISBN string `json:"isbn"`
}

// listened is a session's timeListening: seconds, null for a zero-length session,
// a string in some older databases.
type listened float64

func (l *listened) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		v = 0 // null, or unreadable: no listening
	}
	*l = listened(v)
	return nil
}

// absSession is one ABS listening session.
type absSession struct {
	ID            string      `json:"id"`
	LibraryItemID string      `json:"libraryItemId"`
	EpisodeID     *string     `json:"episodeId"`
	MediaType     string      `json:"mediaType"`
	MediaMetadata absMetadata `json:"mediaMetadata"`
	DisplayTitle  string      `json:"displayTitle"`
	DisplayAuthor string      `json:"displayAuthor"`
	Duration      float64     `json:"duration"`
	DeviceInfo    struct {
		ClientName    string `json:"clientName"`
		ClientVersion string `json:"clientVersion"`
		DeviceName    string `json:"deviceName"`
		Manufacturer  string `json:"manufacturer"`
		Model         string `json:"model"`
		BrowserName   string `json:"browserName"`
		OSName        string `json:"osName"`
	} `json:"deviceInfo"`
	TimeListening listened `json:"timeListening"`
	StartTime     float64  `json:"startTime"`
	CurrentTime   float64  `json:"currentTime"`
	StartedAt     float64  `json:"startedAt"`
	UpdatedAt     float64  `json:"updatedAt"`
}

// maxSessionPages bounds the paging (10 million sessions at 200 a page).
const maxSessionPages = 50000

// sessions reads all of user id's sessions, page by page (newest first; pages
// can shift while they are read, so a session seen twice is kept once). own: the
// token is that user's (ABS refuses their /api/users/{id} routes then).
func (c *Client) sessions(ctx context.Context, id string, own bool) ([]absSession, error) {
	path := "/api/users/" + url.PathEscape(id) + "/listening-sessions"
	if own {
		path = "/api/me/listening-sessions"
	}
	seen := map[string]bool{}
	var out []absSession
	for page := 0; page < maxSessionPages; page++ {
		var resp struct {
			NumPages int          `json:"numPages"`
			Sessions []absSession `json:"sessions"`
		}
		q := url.Values{"itemsPerPage": {strconv.Itoa(sessionsPerPage)}, "page": {strconv.Itoa(page)}}
		if err := c.get(ctx, path, q, capSessions, true, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Sessions {
			if s.ID == "" || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			out = append(out, s)
		}
		if len(resp.Sessions) == 0 || page+1 >= resp.NumPages {
			return out, nil
		}
	}
	return out, nil
}

// absLibrary is one ABS library.
type absLibrary struct {
	ID        string `json:"id"`
	MediaType string `json:"mediaType"` // book | podcast
}

// absItem is one library item (minified).
type absItem struct {
	ID        string `json:"id"`
	LibraryID string `json:"libraryId"`
	FolderID  string `json:"folderId"`
	Path      string `json:"path"`
	RelPath   string `json:"relPath"`
	IsFile    bool   `json:"isFile"`
	MediaType string `json:"mediaType"`
	Media     struct {
		Metadata absMetadata `json:"metadata"`
		Duration float64     `json:"duration"`
	} `json:"media"`
}

// items reads every book item of every book library the token can see.
func (c *Client) items(ctx context.Context) ([]absItem, error) {
	var libs struct {
		Libraries []absLibrary `json:"libraries"`
	}
	if err := c.get(ctx, "/api/libraries", nil, capUsers, true, &libs); err != nil {
		return nil, err
	}
	var out []absItem
	for _, l := range libs.Libraries {
		if l.MediaType != "book" {
			continue
		}
		var resp struct {
			Results []absItem `json:"results"`
		}
		if err := c.get(ctx, "/api/libraries/"+url.PathEscape(l.ID)+"/items", url.Values{"limit": {"0"}},
			capItems, true, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Results {
			if it.MediaType == "book" {
				out = append(out, it)
			}
		}
	}
	return out, nil
}
