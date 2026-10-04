package api

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// The admin catalog API (admin console Library and Book screens). Transport only:
// parsing, validation of the request's shape, and error mapping. The queries live
// in catalog (adminbooks.go, bookdetail.go, overrides.go, covers.go); match search
// in meta.

// maxBulkBooks caps one bulk edit; the console pages larger selections.
const maxBulkBooks = 1000

// maxFilterValues caps a repeatable filter (?format=, ?codec=); each value is an
// SQL parameter, and a real library has a handful.
const maxFilterValues = 50

// Bounds on a match search's inputs (they become upstream queries).
const (
	maxMatchQuery = 300
	maxMatchID    = 20
)

// handleAdminListBooks serves GET /admin/books: a keyset page of books across
// every library, filtered, searched and sorted (see bookFilterFromQuery).
func (a *API) handleAdminListBooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, msg := bookFilterFromQuery(q)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	opt := catalog.AdminListOptions{Filter: f, Sort: q.Get("sort"), Cursor: q.Get("cursor"), Limit: queryInt(r, "limit", 0)}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		opt.Desc = true
	default:
		writeError(w, http.StatusBadRequest, `order must be "asc" or "desc"`)
		return
	}
	page, err := a.cat.ListAdminBooks(r.Context(), opt)
	if err != nil {
		a.writeCatalogError(w, err, "admin list books failed", "could not list books")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleAdminBookFacets serves GET /admin/books/facets: the facet counts for the
// same filter parameters as the list.
func (a *API) handleAdminBookFacets(w http.ResponseWriter, r *http.Request) {
	f, msg := bookFilterFromQuery(r.URL.Query())
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	facets, err := a.cat.BookFacets(r.Context(), f)
	if err != nil {
		a.writeCatalogError(w, err, "admin book facets failed", "could not count books")
		return
	}
	writeJSON(w, http.StatusOK, facets)
}

// bookFilterFromQuery parses the list/facet filter parameters. A non-empty msg is
// a 400: a filter that can't be parsed is refused, never silently dropped (which
// would show the admin an unfiltered list as if it were filtered).
func bookFilterFromQuery(q url.Values) (catalog.BookFilter, string) {
	f := catalog.BookFilter{
		Query: strings.TrimSpace(q.Get("q")), Author: q.Get("author"), Series: q.Get("series"),
		Narrator: q.Get("narrator"), Formats: q["format"], Codecs: q["codec"],
	}
	var ok bool
	if f.LibraryID, ok = parseOptionalID(q.Get("library_id")); !ok {
		return f, "invalid library_id"
	}
	if len(f.Formats) > maxFilterValues || len(f.Codecs) > maxFilterValues {
		return f, "too many format or codec values"
	}
	for name, dst := range map[string]**bool{
		"direct_playable": &f.DirectPlayable, "has_cover": &f.HasCover, "has_chapters": &f.HasChapters,
		"matched": &f.Matched, "edited": &f.Edited,
	} {
		switch v := q.Get(name); v {
		case "":
		case "true", "false":
			b := v == "true"
			*dst = &b
		default:
			return f, name + ` must be "true" or "false"`
		}
	}
	for name, dst := range map[string]*float64{"min_duration": &f.MinDuration, "max_duration": &f.MaxDuration} {
		if v := q.Get(name); v != "" {
			d, err := strconv.ParseFloat(v, 64)
			if err != nil || d < 0 {
				return f, name + " must be a number of seconds"
			}
			*dst = d
		}
	}
	// ?issue= lists one Health issue's books (duplicates are groups, served by
	// /admin/issues/duplicates); ?issue_ignored=true the ones an admin ignored.
	if f.Issue = q.Get("issue"); f.Issue != "" && !catalog.ValidBookIssue(f.Issue) {
		return f, "unknown issue"
	}
	switch v := q.Get("issue_ignored"); v {
	case "", "false":
	case "true":
		if f.Issue == "" {
			return f, "issue_ignored needs an issue"
		}
		f.IssueIgnored = true
	default:
		return f, `issue_ignored must be "true" or "false"`
	}
	for name, dst := range map[string]*string{"added_after": &f.AddedAfter, "added_before": &f.AddedBefore} {
		if v := q.Get(name); v != "" {
			bound, ok := normalizeInstant(v)
			if !ok {
				return f, name + " must be a date (YYYY-MM-DD) or an RFC 3339 time"
			}
			*dst = bound
		}
	}
	return f, ""
}

// parseOptionalID reads an optional id query value (?library_id=, ?user_id=, a
// keyset cursor): 0 when absent, false when present but not a positive id.
func parseOptionalID(v string) (int64, bool) {
	if v == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	return id, err == nil && id > 0
}

// normalizeInstant accepts the bounds added_at compares against: a date or a full
// RFC 3339 timestamp. added_at is stored as RFC 3339 UTC and compared as text, so a
// timestamp is rewritten in that same form (an offset such as +02:00 would
// otherwise compare hours off); a bare date is a prefix of it and kept as is.
func normalizeInstant(v string) (string, bool) {
	if _, err := time.Parse("2006-01-02", v); err == nil {
		return v, true
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", false
	}
	return t.UTC().Format(time.RFC3339), true
}

// handleAdminPeople serves GET /admin/authors and /admin/narrators.
func (a *API) handleAdminPeople(field, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		libID, ok := parseOptionalID(r.URL.Query().Get("library_id"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid library_id")
			return
		}
		agg, err := a.cat.People(r.Context(), field, libID)
		if err != nil {
			a.writeCatalogError(w, err, "admin people aggregate failed", "could not list "+key, "field", field)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			key: agg.People, "merge_suggestions": agg.Suggestions, "unknown": agg.Unknown,
		})
	}
}

// handleAdminSeries serves GET /admin/series.
func (a *API) handleAdminSeries(w http.ResponseWriter, r *http.Request) {
	libID, ok := parseOptionalID(r.URL.Query().Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	series, err := a.cat.Series(r.Context(), libID)
	if err != nil {
		a.writeCatalogError(w, err, "admin series aggregate failed", "could not list series")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

// writeBookError maps an error from a book endpoint: no book at the path is a 404
// with its code; everything else goes through writeCatalogError.
func (a *API) writeBookError(w http.ResponseWriter, err error, op, genericMsg string, logKV ...any) {
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, codeBookNotFound, "no book is indexed at that path")
		return
	}
	a.writeCatalogError(w, err, op, genericMsg, logKV...)
}

// handleAdminBook serves GET /admin/libraries/{id}/book?path=: the book page.
// authorizedPath resolves the library and path (an admin's scope is the whole
// library); these endpoints only touch the database, so a path that isn't an
// indexed book is the catalog's 404.
func (a *API) handleAdminBook(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	a.writeBookDetail(w, r, lib.ID, p)
}

func (a *API) writeBookDetail(w http.ResponseWriter, r *http.Request, libID int64, p string) {
	d, err := a.cat.AdminBookDetail(r.Context(), libID, p)
	if err != nil {
		a.writeBookError(w, err, "admin book detail failed", "could not load book", "library", libID, "path", p)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// editRequest is the body of a book edit (PATCH) and, without chapters, of a bulk
// edit.
type editRequest struct {
	Set      map[string]string `json:"set"`
	Revert   []string          `json:"revert"`
	Source   string            `json:"source"`
	Chapters *struct {
		Set    map[int]string `json:"set"`
		Revert []int          `json:"revert"`
	} `json:"chapters"`
}

func (e editRequest) toEdit(userID int64) catalog.BookEdit {
	edit := catalog.BookEdit{Set: e.Set, Revert: e.Revert, Source: e.Source, UserID: userID}
	if e.Chapters != nil {
		edit.ChapterSet, edit.ChapterRevert = e.Chapters.Set, e.Chapters.Revert
	}
	return edit
}

// details are the edit for the audit log: the values set (a long one cut short),
// the fields reverted, where they came from, and how many chapter titles changed.
func (e editRequest) details() map[string]any {
	d := map[string]any{}
	if len(e.Set) > 0 {
		set := make(map[string]string, len(e.Set))
		for k, v := range e.Set {
			if r := []rune(v); len(r) > 120 {
				v = string(r[:119]) + "…"
			}
			set[k] = v
		}
		d["set"] = set
	}
	if len(e.Revert) > 0 {
		d["revert"] = e.Revert
	}
	if e.Source != "" {
		d["source"] = e.Source
	}
	if e.Chapters != nil {
		if n := len(e.Chapters.Set) + len(e.Chapters.Revert); n > 0 {
			d["chapters"] = n
		}
	}
	return d
}

func (e editRequest) empty() bool {
	return len(e.Set) == 0 && len(e.Revert) == 0 &&
		(e.Chapters == nil || (len(e.Chapters.Set) == 0 && len(e.Chapters.Revert) == 0))
}

// handleAdminEditBook serves PATCH /admin/libraries/{id}/book?path=: set or revert
// metadata overrides (and chapter titles), then returns the updated book page.
func (a *API) handleAdminEditBook(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var req editRequest
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.empty() {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if err := a.cat.EditBook(r.Context(), lib.ID, p, req.toEdit(userFrom(r.Context()).ID)); err != nil {
		a.writeBookError(w, err, "edit book failed", "could not save the edit", "library", lib.ID, "path", p)
		return
	}
	a.audit(r, "book.edit", lib.Name+": "+p, req.details())
	a.writeBookDetail(w, r, lib.ID, p)
}

// handleAdminBulkEdit serves POST /admin/books/bulk: the same field edit applied
// to many books in one transaction (all or nothing; chapter edits are refused).
func (a *API) handleAdminBulkEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Books []catalog.Ref `json:"books"`
		editRequest
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch {
	case len(req.Books) == 0:
		writeError(w, http.StatusBadRequest, "books is required")
		return
	case len(req.Books) > maxBulkBooks:
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge, "too many books in one edit (at most 1000)")
		return
	case req.empty():
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if err := a.cat.EditBooks(r.Context(), req.Books, req.toEdit(userFrom(r.Context()).ID)); err != nil {
		a.writeBookError(w, err, "bulk edit failed", "could not save the edit", "books", len(req.Books))
		return
	}
	distinct := map[catalog.Ref]bool{}
	for _, b := range req.Books {
		distinct[catalog.Ref{LibraryID: b.LibraryID, Path: catalog.CleanRelPath(b.Path)}] = true
	}
	details := req.details()
	details["books"] = len(distinct)
	a.audit(r, "book.bulk_edit", "", details)
	writeJSON(w, http.StatusOK, map[string]int{"updated": len(distinct)})
}

// handleAdminMatch serves GET /admin/libraries/{id}/book/match?path=: community
// works the book might be. ?q= searches that text and ?asin= / ?isbn= look an
// identifier up; with none, meta.Candidates searches the book's own facts.
//
// Responses: metadata off -> 404 (code metadata_off); no book -> 404; upstream
// down -> 502; otherwise 200 {"candidates": [...]} (possibly empty).
func (a *API) handleAdminMatch(w http.ResponseWriter, r *http.Request) {
	if !a.metadataOn() {
		writeErrorCode(w, http.StatusNotFound, codeMetadataOff, "community metadata is turned off")
		return
	}
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, err := a.cat.GetBookByPath(r.Context(), lib.ID, p)
	if err != nil {
		a.writeBookError(w, err, "load book for match failed", "could not load book", "library", lib.ID, "path", p)
		return
	}
	query := r.URL.Query()
	mq := meta.MatchQuery{
		Text: strings.TrimSpace(query.Get("q")), ASIN: strings.TrimSpace(query.Get("asin")),
		ISBN: strings.TrimSpace(query.Get("isbn")), Title: book.Title, Series: book.Series,
		Author: book.Author, Duration: book.Duration, BookASIN: book.ASIN, BookISBN: book.ISBN,
	}
	if utf8.RuneCountInString(mq.Text) > maxMatchQuery || len(mq.ASIN) > maxMatchID || len(mq.ISBN) > maxMatchID {
		writeError(w, http.StatusBadRequest, "query too long")
		return
	}
	cands, err := a.meta.Candidates(r.Context(), mq)
	if err != nil {
		a.log.Warn("match search failed", "err", err, "library", lib.ID, "path", p)
		writeError(w, http.StatusBadGateway, "metadata service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": cands})
}

// handleAdminSetCover serves PUT /admin/libraries/{id}/cover?path=: upload a custom
// cover (the raw image as the body). It is stored in the database, never in the
// library folder, and served by the ordinary cover endpoint ahead of the book's own
// art. catalog.SetCover checks the image and that the book is indexed.
func (a *API) handleAdminSetCover(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, catalog.MaxCoverBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = catalog.ErrCoverTooLarge
		} else {
			writeError(w, http.StatusBadRequest, "could not read the image")
			return
		}
	}
	if err == nil {
		err = a.cat.SetCover(r.Context(), lib.ID, p, data, userFrom(r.Context()).ID)
	}
	if err != nil {
		a.writeBookError(w, err, "set cover failed", "could not save the cover", "library", lib.ID, "path", p)
		return
	}
	a.audit(r, "book.cover_set", lib.Name+": "+p, map[string]any{"bytes": len(data)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "cover set", "path": p})
}

// handleAdminDeleteCover serves DELETE /admin/libraries/{id}/cover?path=: remove a
// custom cover, back to the book's own art.
func (a *API) handleAdminDeleteCover(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if err := a.cat.DeleteCover(r.Context(), lib.ID, p); err != nil {
		a.writeCatalogError(w, err, "delete cover failed", "could not remove the cover", "library", lib.ID, "path", p)
		return
	}
	a.audit(r, "book.cover_remove", lib.Name+": "+p, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "cover removed", "path": p})
}
