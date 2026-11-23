package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/matchrun"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// The admin catalog API (admin console Library and Book screens). Transport only:
// parsing, validation of the request's shape, and error mapping. The queries live
// in catalog (adminbooks.go, bookdetail.go, overrides.go, covers.go); match search
// in meta.

// maxBulkBooks caps one bulk edit, and any list of paths one request carries
// (boundedList); the console pages larger selections.
const maxBulkBooks = 1000

// selectionBodyMax bounds a body that carries up to maxBulkBooks paths (a
// share's rules, a selection of books for a bulk edit or a Health ignore):
// that many at catalog.MaxPathBytes, sent unescaped, with room for each one's
// library id and the request's other fields. A longer body is 400 too_large
// (decodeBody), as a longer list is (boundedList).
const selectionBodyMax = maxBulkBooks*(catalog.MaxPathBytes+64) + 64<<10

// bulkEditBodyMax is selectionBodyMax for a bulk edit, whose edit itself has
// the 1 MiB a one-book edit's body carries (maxBulkEditBytes).
const bulkEditBodyMax = selectionBodyMax + maxBulkEditBytes

// maxBulkEditBytes bounds a bulk edit's body less its books: the edit, which
// holds lists and maps of its own (revert, add, remove), so it never gets the
// room the selection's paths have (handleAdminBulkEdit).
const maxBulkEditBytes = 1 << 20

// spanBytes counts the bytes of the JSON values decoded into it, keeping none.
type spanBytes int64

func (n *spanBytes) UnmarshalJSON(data []byte) error {
	*n += spanBytes(len(data))
	return nil
}

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
	gen, cacheable := a.cat.ContentGeneration() // before any read (listcache.go)
	q := r.URL.Query()
	f, msg := bookFilterFromQuery(q)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	a.issueOff(&f)
	opt := catalog.AdminListOptions{Filter: f, Sort: q.Get("sort"), Cursor: q.Get("cursor"), Limit: queryInt(r, "limit", 0)}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		opt.Desc = true
	default:
		writeError(w, http.StatusBadRequest, `order must be "asc" or "desc"`)
		return
	}
	// A page is the database's answer to the query string alone (an admin sees
	// every book), so it is kept like the player's lists (server#245). Only a
	// first page: one admin scrolling a large library through every cursor page
	// would otherwise push the player's lists out of the shared cache.
	//
	// Health's books without chapters carry each book's chapter detection
	// (detectRows), which a Bridge's reports move without a content write: that
	// list is read each time.
	detect := a.bridge != nil && f.Issue == catalog.IssueNoChapters && !f.IssueIgnored
	write := a.cachedList(w, r, gen, cacheable && opt.Cursor == "" && !detect, nil, f.IssueOff)
	if write == nil {
		return
	}
	page, err := a.cat.ListAdminBooks(r.Context(), opt)
	if err != nil {
		a.writeCatalogError(w, err, "admin list books failed", "could not list books")
		return
	}
	if !detect {
		write(page)
		return
	}
	rows, err := a.detectRows(r.Context(), page)
	if err != nil {
		a.log.Warn("admin list books failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list books")
		return
	}
	write(rows)
}

// detectRow is a row of Health's books without chapters: the book, and its
// newest chapters.detect job when it has one (queued, working, parked or
// ended), so the row says what became of a detection and offers no second one
// while one is open (server#273).
type detectRow struct {
	catalog.AdminBook
	DetectJob *bridge.Job `json:"detect_job,omitempty"`
}

// detectPage is an AdminPage of detectRows.
type detectPage struct {
	Books      []detectRow `json:"books"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// detectRows is page with each book's newest chapters.detect job, read once
// for the page's books (bridge.LatestForBooks), never per row and never from
// the newest jobs alone: a book queued among hundreds still shows its own.
func (a *API) detectRows(ctx context.Context, page *catalog.AdminPage) (detectPage, error) {
	keys := make([]bridge.BookKey, len(page.Books))
	for i, b := range page.Books {
		keys[i] = bridge.BookKey{LibraryID: b.LibraryID, Path: b.Path}
	}
	jobs, err := a.bridge.LatestForBooks(ctx, bridge.KindDetectChapters, keys)
	if err != nil {
		return detectPage{}, err
	}
	out := detectPage{Books: make([]detectRow, len(page.Books)), NextCursor: page.NextCursor}
	for i, b := range page.Books {
		out.Books[i].AdminBook = b
		if j, ok := jobs[keys[i]]; ok {
			out.Books[i].DetectJob = &j
		}
	}
	return out, nil
}

// handleAdminBookFacets serves GET /admin/books/facets: the facet counts for the
// same filter parameters as the list.
func (a *API) handleAdminBookFacets(w http.ResponseWriter, r *http.Request) {
	gen, cacheable := a.cat.ContentGeneration() // before any read (listcache.go)
	f, msg := bookFilterFromQuery(r.URL.Query())
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	a.issueOff(&f)
	// Counted over every matching book (about 100 ms at 5,000), and like a page
	// the database's answer to the query string alone: kept until the next content write.
	write := a.cachedList(w, r, gen, cacheable, nil, f.IssueOff)
	if write == nil {
		return
	}
	facets, err := a.cat.BookFacets(r.Context(), f)
	if err != nil {
		a.writeCatalogError(w, err, "admin book facets failed", "could not count books")
		return
	}
	write(facets)
}

// bookFilterFromQuery parses the list/facet filter parameters. A non-empty msg is
// a 400: a filter that can't be parsed is refused, never silently dropped (which
// would show the admin an unfiltered list as if it were filtered).
func bookFilterFromQuery(q url.Values) (catalog.BookFilter, string) {
	f := catalog.BookFilter{
		Query: strings.TrimSpace(q.Get("q")), Author: q.Get("author"), Series: q.Get("series"),
		Narrator: q.Get("narrator"), Genre: strings.TrimSpace(q.Get("genre")), Tag: strings.TrimSpace(q.Get("tag")),
		Formats: q["format"], Codecs: q["codec"],
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

// issueOff marks f's ?issue= as listing nothing while its kind isn't kept
// current (unknown_asin with the report off), so a direct link shows no stale
// rows. Before the report's first answers its rows are none anyway, so this
// needs none of unknownASINListed's reads. Part of the list cache's key: the
// answer depends on it.
func (a *API) issueOff(f *catalog.BookFilter) {
	f.IssueOff = f.Issue == catalog.IssueUnknownASIN && !a.reportUnknownOn()
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

// aggregateLibrary reads an aggregate's optional ?library_id= (0: every
// library), answering 400 for one that isn't an id and 404 not_found for a
// library that doesn't exist, rather than an empty list as if it had no books
// (server#224). ok is false once it has answered.
func (a *API) aggregateLibrary(w http.ResponseWriter, r *http.Request) (int64, bool) {
	libID, ok := parseOptionalID(r.URL.Query().Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return 0, false
	}
	_, ok = a.optionalLibraryName(w, r, libID, "could not read the library")
	return libID, ok
}

// handleAdminPeople serves GET /admin/authors and /admin/narrators.
func (a *API) handleAdminPeople(field, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		libID, ok := a.aggregateLibrary(w, r)
		if !ok {
			return
		}
		agg, err := a.cat.People(r.Context(), field, libID, nil)
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
	libID, ok := a.aggregateLibrary(w, r)
	if !ok {
		return
	}
	series, err := a.cat.Series(r.Context(), libID, nil, true)
	if err != nil {
		a.writeCatalogError(w, err, "admin series aggregate failed", "could not list series")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

// writeBookError maps an error from a book endpoint: no book at the path is a 404
// with its code; an Unlink while a match run applies to the book's library is a
// 409. Everything else goes through writeCatalogError.
func (a *API) writeBookError(w http.ResponseWriter, err error, op, genericMsg string, logKV ...any) {
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, codeBookNotFound, "no book is indexed at that path")
		return
	}
	if errors.Is(err, catalog.ErrMatchRunBusy) {
		writeErrorCode(w, http.StatusConflict, codeMatchRunBusy, "a match run is applying to this library; stop it or wait for it to finish before unlinking")
		return
	}
	a.writeCatalogError(w, err, op, genericMsg, logKV...)
}

// handleAdminBook serves GET /admin/libraries/{id}/book?path=[&warm=1]: the
// book page. authorizedPath resolves the library and path (an admin's scope is
// the whole library); these endpoints only touch the database, so a path that
// isn't an indexed book is the catalog's 404. warm=1 is the chapter editor's
// ask to start the remuxes not made yet (adminRemuxes); no other read of the
// page starts one.
func (a *API) handleAdminBook(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	a.writeBookPage(w, r, lib, p, r.URL.Query().Get("warm") == "1")
}

// writeBookDetail answers with the book page, warming nothing (writeBookPage).
func (a *API) writeBookDetail(w http.ResponseWriter, r *http.Request, lib *catalog.Library, p string) {
	a.writeBookPage(w, r, lib, p, false)
}

// writeBookPage answers with the book page; warm starts the remuxes of its
// MP3-in-Matroska files not made yet (adminRemuxes).
func (a *API) writeBookPage(w http.ResponseWriter, r *http.Request, lib *catalog.Library, p string, warm bool) {
	// Asked before the page is read: a check that ends in between is then in the
	// page, rather than missing from a page that says none is running.
	ref := catalog.Ref{LibraryID: lib.ID, Path: catalog.CleanRelPath(p)}
	checking := a.chapterChecks != nil && a.chapterChecks.Checking(ref)
	d, err := a.cat.AdminBookDetail(r.Context(), lib.ID, p)
	if err != nil {
		a.writeBookError(w, err, "admin book detail failed", "could not load book", "library", lib.ID, "path", p)
		return
	}
	d.CommunityChecking = checking
	d.CommunityCheckFailed = !checking && a.chapterChecks != nil && a.chapterChecks.Failed(ref)
	b := d.Book
	d.MatchQuery = meta.SearchPrefill(b.Title, b.Author, b.Series, b.Path, b.IsFolder)
	if d.CommunityMatch != nil && a.meta != nil {
		d.CommunityMatch.WebURL = a.meta.WorkURL(d.CommunityMatch.WorkID)
	}
	if b.Ebook != nil {
		d.Book.Readalong = a.readalongOf(r.Context(), lib, b.Path, b.IsFolder, b.Ebook.Path, adminAudioFiles(d))
	}
	a.adminRemuxes(r.Context(), lib, d.Files, warm)
	writeJSON(w, http.StatusOK, d)
}

// adminRemuxes marks the book page's MP3-in-Matroska files while they are
// served as their remux (remuxing): each plays as stored (direct_playable: its
// stream is the remuxed MP3, handleStream, as /item says), and remux_cached says
// whether its copy is made (until then the stream is a live transcode from the
// file's start, which the console's chapter editor can't seek, so it asks for
// ?transcode=1&t= instead). With warm (the editor's ask, never a mere view of
// the page or a save's reply, so browsing the console pushes no listener's
// copy out of the cache) the copies not made yet are warmed in the
// background, as /item warms them for a player (warmRemuxes). A file not
// joined, or not looked at on a share not answering, is said not cached. One
// the remux refused is left unmarked: its stream is the stored Matroska, which
// no browser plays, and no copy is coming for the editor to wait for.
func (a *API) adminRemuxes(ctx context.Context, lib *catalog.Library, files []catalog.AdminFile, warm bool) {
	if !a.remuxing(lib) {
		return
	}
	var rels []string
	var at []int
	for i, f := range files {
		if media.Remuxable(f.Format, f.Codec) {
			rels = append(rels, f.Path)
			at = append(at, i)
		}
	}
	join := a.joinRemuxes
	if warm {
		join = a.warmRemuxes
	}
	abs, answering := join(ctx, lib, rels)
	for k, i := range at {
		st := media.CacheMissing
		// No look on a share that stopped answering: each would wait out its own bound.
		if answering && abs[k] != "" {
			st, _, _ = a.remux.Status(lib.Root, abs[k])
		}
		if st == media.CacheFailed {
			continue
		}
		cached := st == media.CacheReady
		files[i].DirectPlayable, files[i].RemuxCached = true, &cached
	}
}

// handleCheckCommunityChapters serves POST
// /admin/libraries/{id}/book/community-chapters?path=: check the book against the
// community's chapter list now, in the background (a long book's pauses take a
// while to find), and return the book page, community_checking set until the
// check is recorded. Metadata off -> 404 metadata_off; no book -> 404.
func (a *API) handleCheckCommunityChapters(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	// No book there: the check finds none and stops, and the page answers 404.
	a.chapterChecks.Start(a.baseCtx, catalog.Ref{LibraryID: lib.ID, Path: p})
	a.writeBookDetail(w, r, lib, p)
}

// editRequest is the body of a book edit (PATCH) and, without chapters, of a bulk
// edit. Its lists and maps are each decoded only when they hold at most their
// cap (editFields, editLabels, editChapterTitles, editChapterIndexes).
type editRequest struct {
	Set    editFieldValues `json:"set"`
	Revert editFields      `json:"revert"`
	// Add and Remove change a label list (genres, tags) by name instead of
	// setting it, so a bulk edit keeps each book's own labels
	// (catalog.BookEdit.Add).
	Add      editLabels `json:"add"`
	Remove   editLabels `json:"remove"`
	Source   string     `json:"source"`
	Chapters *struct {
		Set    editChapterTitles  `json:"set"`
		Revert editChapterIndexes `json:"revert"`
	} `json:"chapters"`
	// ChapterSource picks where the chapters come from: "files", "community"
	// or "auto" (catalog.BookEdit.ChapterSource).
	ChapterSource string `json:"chapter_source"`
	// Match records the community match the edit was taken from (the match
	// dialog's accept, with source "community"): the work and recording, with
	// or without an ASIN or ISBN (catalog.BookEdit.Match, made "accepted").
	// {"work_id": ""} unlinks the book: its recorded match goes with every
	// community value and cover (the admin's own edits stay), as a clear does
	// for a library; a match naming no work ({} or a null work_id) is refused.
	// One book at a time: a bulk edit refuses it.
	Match *matchRequest `json:"match"`
}

// The caps on an edit's own lists and maps (editRequest), far past what the
// console sends: the edit's 1 MiB alone would let a list of over three hundred
// thousand empty entries be decoded before anything counted it. One past its
// cap is errEditTooLong.
const (
	// maxEditFields caps the fields an edit sets, reverts, or adds labels to
	// or removes them from (catalog.OverrideFields).
	maxEditFields = 64
	// maxEditLabels caps the labels one field's list adds or removes: a
	// boundedList's.
	maxEditLabels = maxBulkBooks
	// maxEditChapters caps the chapter titles an edit sets and the chapters it
	// reverts (a book's chapters from its files may be more than the
	// metadata.MaxChapterListEntries an edited list holds), and the files a
	// chapter edit's basis names.
	maxEditChapters = 10_000
)

// errEditTooLong is a list or map of an edit past its cap: 400 too_large, on
// the one-book edit and the bulk edit alike (writeDecodeError).
var errEditTooLong = errors.New("a list in the edit holds too many entries")

// editErr is err, or errEditTooLong for a list or map past its cap.
func editErr(err error) error {
	if errors.Is(err, errTooManyEntries) {
		return errEditTooLong
	}
	return err
}

// editFields is the fields an edit reverts, at most maxEditFields.
type editFields []string

func (l *editFields) UnmarshalJSON(data []byte) (err error) {
	*l, err = decodeBounded[string](bytes.NewReader(data), maxEditFields)
	return editErr(err)
}

// editFieldValues is the values an edit sets, by field, at most maxEditFields.
type editFieldValues map[string]string

func (m *editFieldValues) UnmarshalJSON(data []byte) (err error) {
	*m, err = decodeBoundedMap[string, string](data, maxEditFields)
	return editErr(err)
}

// editLabels is the labels an edit adds or removes, by field: at most
// maxEditFields fields, each list at most maxEditLabels.
type editLabels map[string][]string

func (m *editLabels) UnmarshalJSON(data []byte) error {
	lists, err := decodeBoundedMap[string, boundedList[string]](data, maxEditFields)
	if err != nil {
		return editErr(err)
	}
	if lists == nil {
		*m = nil
		return nil
	}
	*m = make(editLabels, len(lists))
	for field, labels := range lists {
		(*m)[field] = labels
	}
	return nil
}

// editChapterTitles is the chapter titles an edit sets, by chapter index, at
// most maxEditChapters.
type editChapterTitles map[int]string

func (m *editChapterTitles) UnmarshalJSON(data []byte) (err error) {
	*m, err = decodeBoundedMap[int, string](data, maxEditChapters)
	return editErr(err)
}

// editChapterIndexes is the chapters an edit reverts, at most maxEditChapters.
type editChapterIndexes []int

func (l *editChapterIndexes) UnmarshalJSON(data []byte) (err error) {
	*l, err = decodeBounded[int](bytes.NewReader(data), maxEditChapters)
	return editErr(err)
}

// matchRequest is an edit's community match (editRequest.Match). WorkID is a
// pointer so a match naming no work ({} or a null work_id) is told apart from the
// Unlink's explicitly empty one, and refused (handleAdminEditBook). ASIN and
// ISBN are the identifiers the match offered, ticked or not (optional; recorded
// so a refresh never fills one back the admin unticked or reverted: server#330);
// one the field would refuse is 400 on field "match".
type matchRequest struct {
	WorkID      *string `json:"work_id"`
	RecordingID string  `json:"recording_id"`
	ASIN        string  `json:"asin"`
	ISBN        string  `json:"isbn"`
}

func (m *matchRequest) workID() string {
	if m == nil || m.WorkID == nil {
		return ""
	}
	return *m.WorkID
}

func (e editRequest) toEdit(userID int64) catalog.BookEdit {
	edit := catalog.BookEdit{Set: e.Set, Revert: e.Revert, Add: e.Add, Remove: e.Remove, Source: e.Source,
		ChapterSource: e.ChapterSource, UserID: userID}
	if e.Chapters != nil {
		edit.ChapterSet, edit.ChapterRevert = e.Chapters.Set, e.Chapters.Revert
	}
	if e.Match != nil {
		edit.Match = &catalog.MatchAnchor{WorkID: e.Match.workID(), RecordingID: e.Match.RecordingID, How: catalog.MatchHowAccepted,
			OfferedASIN: e.Match.ASIN, OfferedISBN: e.Match.ISBN}
	}
	return edit
}

// details are the edit for the audit log: the values set (a long one cut short),
// the fields reverted, the labels added and removed, where they came from, and
// how many chapter titles changed.
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
	if len(e.Add) > 0 {
		d["add"] = e.Add
	}
	if len(e.Remove) > 0 {
		d["remove"] = e.Remove
	}
	if e.Source != "" {
		d["source"] = e.Source
	}
	if e.Chapters != nil {
		if n := len(e.Chapters.Set) + len(e.Chapters.Revert); n > 0 {
			d["chapters"] = n
		}
	}
	if e.ChapterSource != "" {
		d["chapter_source"] = e.ChapterSource
	}
	if e.Match != nil {
		d["match"] = e.Match.workID() // "" = the book unlinked (its community values and cover gone too)
	}
	return d
}

func (e editRequest) empty() bool {
	return len(e.Set) == 0 && len(e.Revert) == 0 && len(e.Add) == 0 && len(e.Remove) == 0 && e.ChapterSource == "" &&
		(e.Chapters == nil || (len(e.Chapters.Set) == 0 && len(e.Chapters.Revert) == 0)) && e.Match == nil
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
		if errors.Is(err, errEditTooLong) {
			writeDecodeError(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.empty() {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if req.Match != nil && req.Match.WorkID == nil {
		// Only an explicitly empty work_id unlinks the book: a match naming no
		// work is a mistake, never read as one.
		a.writeCatalogError(w, &catalog.OverrideError{Field: "match", Reason: `work_id is required ("" unlinks the book)`},
			"edit book failed", "could not save the edit")
		return
	}
	edit := req.toEdit(userFrom(r.Context()).ID)
	err := a.cat.EditBook(r.Context(), lib.ID, p, edit)
	if errors.Is(err, catalog.ErrMatchRunBusy) {
		// A run left marked applying with nothing behind it (its finishing
		// write failed, or a stopped server left it) is settled, and the
		// Unlink tried once more; a live apply still refuses it.
		if n, serr := a.matchRuns.SettleOrphans(r.Context()); serr != nil {
			a.log.Warn("settle match runs for an unlink failed", "err", serr)
		} else if n > 0 {
			err = a.cat.EditBook(r.Context(), lib.ID, p, edit)
		}
	}
	if err != nil {
		a.writeBookError(w, err, "edit book failed", "could not save the edit", "library", lib.ID, "path", p)
		return
	}
	a.audit(r, "book.edit", lib.Name+": "+p, req.details())
	a.writeBookDetail(w, r, lib, p)
}

// handleAdminBulkEdit serves POST /admin/books/bulk: the same field edit applied
// to many books in one transaction (all or nothing; chapter edits are refused).
func (a *API) handleAdminBulkEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Books boundedList[catalog.Ref] `json:"books"`
		editRequest
	}
	// The body is read whole and measured before any of it becomes values:
	// only its books may use the room past maxBulkEditBytes. The edit's own
	// lists and maps have caps of their own (editRequest), but its values
	// (a description, a label) are only bounded by the bytes they take.
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, bulkEditBodyMax))
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	var sel struct {
		Books spanBytes `json:"books"`
	}
	// A body that doesn't decode here doesn't below either, and is answered there.
	if dec := json.NewDecoder(bytes.NewReader(body)); dec.Decode(&sel) == nil &&
		dec.InputOffset()-int64(sel.Books) > maxBulkEditBytes {
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge,
			fmt.Sprintf("the edit is larger than %d bytes", maxBulkEditBytes))
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	switch {
	case len(req.Books) == 0:
		writeError(w, http.StatusBadRequest, "books is required")
		return
	case req.empty():
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	case refsTooLong(w, req.Books):
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

// handleAdminMergeSpellings serves POST /admin/books/merge. The reply holds only
// the books the transaction changed and the prior overrides needed for Undo.
func (a *API) handleAdminMergeSpellings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID int64               `json:"library_id"`
		Field     string              `json:"field"`
		Sources   boundedList[string] `json:"sources"`
		Target    string              `json:"target"`
	}
	if err := decodeJSON(r, &req, maxBulkEditBytes); errors.Is(err, errTooManyEntries) {
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge, fmt.Sprintf("at most %d source spellings per merge", maxBulkBooks))
		return
	} else if err != nil {
		writeDecodeError(w, err)
		return
	}
	books, err := a.cat.MergeSpellings(r.Context(), req.LibraryID, req.Field, req.Sources, req.Target, userFrom(r.Context()).ID)
	if err != nil {
		writeMergeError(a, w, err, "merge spellings failed", "could not merge the spellings")
		return
	}
	if len(books) > 0 {
		details := (editRequest{Set: editFieldValues{req.Field: req.Target}}).details()
		details["books"] = len(books)
		details["merged"] = auditNames(req.Sources)
		if req.LibraryID != 0 {
			details["library"] = a.libraryName(r, req.LibraryID)
		}
		a.audit(r, "book.bulk_edit", "", details)
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": books})
}

// undoMergeBodyMax bounds an Undo's body: catalog.MaxMergeBooks books, each a
// path at catalog.MaxPathBytes and a previous value of up to 500 characters
// (an author, narrator or series), every one escaped, with room for the rest.
const undoMergeBodyMax = catalog.MaxMergeBooks*(catalog.MaxPathBytes+6*500+128) + 64<<10

// mergedBooks is a merge reply's books sent back for Undo: at most
// catalog.MaxMergeBooks, past boundedList's cap.
type mergedBooks []catalog.MergedBook

func (l *mergedBooks) UnmarshalJSON(data []byte) error {
	list, err := decodeBounded[catalog.MergedBook](bytes.NewReader(data), catalog.MaxMergeBooks)
	*l = list
	return err
}

// handleAdminUndoMerge serves POST /admin/books/merge/undo: a merge's reply
// (its books with their previous overrides) and its field and target, put back
// in one transaction (catalog.UndoMerge). A book a scan moved since, or edited
// since, is left as it is and named in "skipped"; the rest still go back.
func (a *API) handleAdminUndoMerge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Field  string      `json:"field"`
		Target string      `json:"target"`
		Books  mergedBooks `json:"books"`
	}
	if err := decodeJSON(r, &req, undoMergeBodyMax); err != nil {
		if errors.Is(err, errTooManyEntries) {
			err = catalog.ErrMergeTooLarge
		}
		writeMergeError(a, w, err, "", "")
		return
	}
	done, err := a.cat.UndoMerge(r.Context(), req.Field, req.Target, req.Books, userFrom(r.Context()).ID)
	if err != nil {
		writeMergeError(a, w, err, "undo merge failed", "could not undo the merge")
		return
	}
	if done.Restored > 0 {
		details := (editRequest{Revert: []string{req.Field}}).details()
		details["books"] = done.Restored
		a.audit(r, "book.bulk_edit", "", details)
	}
	writeJSON(w, http.StatusOK, done)
}

// writeMergeError answers a merge's or its Undo's error: a body that doesn't
// decode (op ""), a library that isn't there, too many books, else as
// writeCatalogError does.
func writeMergeError(a *API, w http.ResponseWriter, err error, op, genericMsg string) {
	switch {
	case errors.Is(err, catalog.ErrMergeTooLarge):
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge,
			fmt.Sprintf("a merge changes at most %d books; merge one library at a time", catalog.MaxMergeBooks))
	case op == "":
		writeDecodeError(w, err)
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "library not found")
	default:
		a.writeCatalogError(w, err, op, genericMsg)
	}
}

// auditNames is a list of names for an audit entry: the first 20, each cut
// as the edit's values are (editRequest.details).
func auditNames(names []string) []string {
	out := make([]string, 0, min(len(names), 20))
	for _, name := range names[:min(len(names), 20)] {
		if r := []rune(name); len(r) > 120 {
			name = string(r[:119]) + "…"
		}
		out = append(out, name)
	}
	return out
}

// handleAdminMatch serves GET /admin/libraries/{id}/book/match?path=: community
// works the book might be. ?q= adds that text to the book's own facts (its tags
// and its library path) and ?asin= / ?isbn= look an identifier up; with none,
// meta.Candidates matches the book's facts and looks its own identifiers, or its
// hints (an Audiobookshelf import's, else its tags': matchrun.QueryFor), up.
// That default search lists the book's recorded community match (book_matches)
// too when it misses it, last and marked recorded (recordedCandidate), so the
// dialog starts from the match the book has (server#331).
//
// Responses: metadata off -> 404 (code metadata_off); no book -> 404; upstream
// down -> 502; otherwise 200 {"candidates": [...]} (possibly empty).
func (a *API) handleAdminMatch(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if cands, _, ok := a.adminMatchCandidates(w, r, lib.ID, p); ok {
		writeJSON(w, http.StatusOK, map[string]any{"candidates": cands})
	}
}

// adminMatchCandidates is the match search for the book at p (?q=, ?asin=,
// ?isbn=, else the book's own facts), as handleAdminMatch answers it, having
// answered the error itself when ok is false. The match proposal reads its
// candidates rather than a lookup by work id, which lacks a search hit's cover
// and the ASIN it matched; the query it sent comes back too, so a lookup by id
// keeps the book's facts.
func (a *API) adminMatchCandidates(w http.ResponseWriter, r *http.Request, libraryID int64, p string) ([]meta.MatchCandidate, meta.MatchQuery, bool) {
	book, err := a.cat.GetBookByPath(r.Context(), libraryID, p)
	if err != nil {
		a.writeBookError(w, err, "load book for match failed", "could not load book", "library", libraryID, "path", p)
		return nil, meta.MatchQuery{}, false
	}
	query := r.URL.Query()
	// The library's language, as a match run sends it for a book saying none: one
	// grouped read, none for a book saying its own. Unreadable, the book is matched as
	// saying none.
	libLanguage, err := matchrun.LibraryLanguageOf(r.Context(), a.cat, libraryID, book)
	if err != nil {
		a.log.Warn("library languages for match failed", "err", err, "library", libraryID)
	}
	mq := matchrun.QueryFor(book, a.config().Metadata.PreferredRegion(), libLanguage)
	mq.Text, mq.ASIN = strings.TrimSpace(query.Get("q")), strings.TrimSpace(query.Get("asin"))
	mq.ISBN = strings.TrimSpace(query.Get("isbn"))
	if utf8.RuneCountInString(mq.Text) > maxMatchQuery || len(mq.ASIN) > maxMatchID || len(mq.ISBN) > maxMatchID {
		writeError(w, http.StatusBadRequest, "query too long")
		return nil, mq, false
	}
	cands, err := a.meta.Candidates(r.Context(), mq)
	if err != nil {
		a.log.Warn("match search failed", "err", err, "library", libraryID, "path", p)
		writeError(w, http.StatusBadGateway, "metadata service unavailable")
		return nil, mq, false
	}
	if mq.Text == "" && mq.ASIN == "" && mq.ISBN == "" {
		if c := a.recordedCandidate(r.Context(), libraryID, p, mq, cands); c != nil {
			cands = append(cands, *c)
		}
	}
	return cands, mq, true
}

// handleMatchProposal serves GET /admin/books/match-proposal?library_id=&path=
// &work_id=[&recording_id=][&q=|&asin=|&isbn=]: the match dialog's comparison of
// a chosen work and recording with the indexed book (matchrun.Preview, the
// overwrite plan a run applies). An omitted recording means the admin is
// comparing the work alone; the search parameters are the ones that found the
// candidate.
//
// Responses: metadata off -> 404 (code metadata_off); no library_id, or a work
// or recording id that isn't one -> 400; no book -> 404 (code book_not_found);
// the work or recording not found -> 404; upstream down -> 502; otherwise 200.
func (a *API) handleMatchProposal(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	q := r.URL.Query()
	id, ok := parseOptionalID(q.Get("library_id"))
	if !ok || id == 0 {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	work, recording := q.Get("work_id"), q.Get("recording_id")
	if strings.TrimSpace(work) == "" || !validWorkID(work) || !validWorkID(recording) {
		writeError(w, http.StatusBadRequest, "invalid work or recording id")
		return
	}
	_, path, _, status, msg := a.scopedPath(r, id, q.Get("path"))
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	candidates, mq, ok := a.adminMatchCandidates(w, r, id, path)
	if !ok {
		return
	}
	i := slices.IndexFunc(candidates, func(c meta.MatchCandidate) bool { return c.WorkID == work })
	var c *meta.MatchCandidate
	if i >= 0 {
		c = &candidates[i]
	} else {
		// A candidate found by a different search can also be named directly,
		// with the book's facts beside the id as recordedCandidate keeps them.
		byID := mq
		byID.WorkID = work
		named, err := a.meta.Candidates(r.Context(), byID)
		if err != nil {
			a.log.Warn("match proposal lookup failed", "work", work, "err", err)
			writeError(w, http.StatusBadGateway, "metadata service unavailable")
			return
		}
		if len(named) == 0 || named[0].WorkID != work {
			writeError(w, http.StatusNotFound, "match candidate not found")
			return
		}
		c = &named[0]
	}
	var rec *meta.MatchRecording
	if recording != "" {
		i := slices.IndexFunc(c.Recordings, func(m meta.MatchRecording) bool { return m.ID == recording })
		if i < 0 {
			writeError(w, http.StatusNotFound, "recording not found")
			return
		}
		rec = &c.Recordings[i]
	}
	st, err := a.cat.BookMatchState(r.Context(), id, path)
	if err != nil {
		a.writeBookError(w, err, "load book for proposal failed", "could not load book", "library", id, "path", path)
		return
	}
	writeJSON(w, http.StatusOK, matchrun.Preview(st, c, rec))
}

// recordedCandidate is the book's recorded community match as a match
// candidate (meta.MatchQuery.WorkID: its work by id, the recorded recording
// kept while the work lists it), marked Recorded, when the book has one that
// cands doesn't list; nil otherwise, and when the lookup fails or the
// community no longer has the work (the search's own answer stands).
func (a *API) recordedCandidate(ctx context.Context, libraryID int64, path string, mq meta.MatchQuery, cands []meta.MatchCandidate) *meta.MatchCandidate {
	m, err := a.cat.BookMatchAt(ctx, libraryID, path)
	if err != nil {
		a.log.Warn("load recorded match for match search failed", "err", err, "library", libraryID, "path", path)
		return nil
	}
	listed := func(workID string) bool {
		return slices.ContainsFunc(cands, func(c meta.MatchCandidate) bool { return c.WorkID == workID })
	}
	if m == nil || listed(m.WorkID) {
		return nil
	}
	// The book's facts stay beside the id, as a refresh's work lookup keeps
	// them: the recording is chosen by its length and whether it says it is a
	// dramatization.
	q := mq
	q.WorkID, q.RecordingID = m.WorkID, m.RecordingID
	found, err := a.meta.Candidates(ctx, q)
	if err != nil {
		a.log.Warn("recorded match lookup for match search failed", "err", err, "library", libraryID, "path", path)
		return nil
	}
	// A retired id follows its redirect: the work it was merged into may be
	// listed already.
	if len(found) == 0 || listed(found[0].WorkID) {
		return nil
	}
	c := found[0]
	c.Recorded = true
	return &c
}

// maxWorkBooks caps one POST /admin/books/works: more books than a real series
// holds, and a bound on the upstream lookups one request can start.
const maxWorkBooks = 100

// bookWork is one entry of POST /admin/books/works, in request order. WorkID is
// the community work the book's ASIN/ISBN resolves to, "" when it has none, has
// no match, or the lookup failed. Failed is true only for the last: the lookup
// failed or ran out of time, so asking again later may resolve it.
type bookWork struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	WorkID    string `json:"work_id"`
	Failed    bool   `json:"failed"`
}

// handleAdminBookWorks serves POST /admin/books/works {"books":[{library_id,path}]}:
// which community work each book is, so the Series screen can place an owned book
// on a series rail by identity rather than by its series_index. The resolution
// (cached, bounded, under the compose budget) is meta.Service.WorkIDs.
//
// Responses: metadata off -> 404 (code metadata_off); otherwise 200
// {"works": [...]}, each entry saying whether its own lookup failed. A ref naming
// no indexed book is answered with an empty work_id (not failed), like a book
// without identifiers.
func (a *API) handleAdminBookWorks(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	// maxWorkBooks paths at the cap fit decodeJSON's default 1 MiB; a longer
	// body is 400 too_large (writeDecodeError), as a list past maxWorkBooks
	// is, with one answer however far past (a boundedList stops at
	// maxBulkBooks, which is past this route's cap too).
	var req struct {
		Books boundedList[catalog.Ref] `json:"books"`
	}
	err := decodeJSON(r, &req, 0)
	switch {
	case errors.Is(err, errTooManyEntries), err == nil && len(req.Books) > maxWorkBooks:
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge,
			fmt.Sprintf("too many books in one request (at most %d)", maxWorkBooks))
		return
	case err != nil:
		writeDecodeError(w, err)
		return
	case len(req.Books) == 0:
		writeError(w, http.StatusBadRequest, "books is required")
		return
	case refsTooLong(w, req.Books):
		return
	}
	found, err := a.cat.BooksByRefs(r.Context(), req.Books)
	if err != nil {
		a.writeCatalogError(w, err, "load books for work ids failed", "could not load books")
		return
	}
	ids := make([]meta.BookIDs, len(found))
	for i, b := range found {
		ids[i] = meta.BookIDs{ASIN: b.ASIN, ISBN: b.ISBN}
	}
	works := a.meta.WorkIDs(r.Context(), ids)
	out := make([]bookWork, len(req.Books))
	for i, ref := range req.Books {
		out[i] = bookWork{LibraryID: ref.LibraryID, Path: ref.Path, WorkID: works[i].ID, Failed: works[i].Failed}
	}
	writeJSON(w, http.StatusOK, map[string]any{"works": out})
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
		if tooBig := (*http.MaxBytesError)(nil); !errors.As(err, &tooBig) {
			writeError(w, http.StatusBadRequest, "could not read the image")
			return
		}
		data = nil
		err = catalog.ErrCoverTooLarge
	}
	a.saveCustomCover(w, r, lib, p, data, err, catalog.SourceEdited)
}

// saveCustomCover keeps data as the book's custom cover from source
// (catalog.SetCover), unless err already says why not, then audits it (bytes and
// source) and answers.
func (a *API) saveCustomCover(w http.ResponseWriter, r *http.Request, lib *catalog.Library, p string,
	data []byte, err error, source string) {
	if err == nil {
		err = a.cat.SetCover(r.Context(), lib.ID, p, data, userFrom(r.Context()).ID, source)
	}
	if err != nil {
		a.writeBookError(w, err, "set cover failed", "could not save the cover", "library", lib.ID, "path", p)
		return
	}
	a.audit(r, "book.cover_set", lib.Name+": "+p, map[string]any{"bytes": len(data), "source": source})
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

// adminAudioFiles is the book page's files as a read-along map names them
// (catalog.BookAudioFiles over its Files: a single-file book's is its own).
func adminAudioFiles(d *catalog.AdminBookDetail) []metadata.ReadalongFile {
	files := make([]catalog.BookFile, len(d.Files))
	for i, f := range d.Files {
		files[i] = catalog.BookFile{RelPath: f.Path, Duration: f.Duration}
	}
	return catalog.BookAudioFiles(d.Book.Path, d.Book.IsFolder, d.Book.Duration, files)
}
