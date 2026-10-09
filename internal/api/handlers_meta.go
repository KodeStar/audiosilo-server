package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// handleMeta resolves a book's asin/isbn against the community metadata API and
// returns a composed enrichment envelope. Transport-only: scope + path resolution
// reuse authorizedPath/bookForPath (exactly like item), the composition and cache
// live in internal/meta.
//
// Responses:
//   - metadata disabled: 404 (clients gate on the `metadata` capability, so they
//     never request this).
//   - book has neither asin nor isbn, or the lookup found no match: 200 {"matched": false}.
//   - upstream unreachable/error: 502.
//   - match: 200 {"matched": true, ...} (see internal/meta.Enrichment).
//
// Every rail entry the caller owns carries `local`, the book to open for it
// (localRails), resolved for this caller on a copy of the rails.
//
// Optional query params (the `meta_bundle` capability), each applied to a
// per-request copy of the matched envelope; unknown values are ignored, so a
// newer client degrades to the full envelope rather than a 400:
//   - include=previous: adds `previous`, the works before this one in its
//     series, nearest first (meta.Service.Previous; a failed one is left out).
//   - spoilers=hide: gates the current work by the CALLER's saved progress on
//     this book (meta.HideSpoilers; no progress = not started), and drops each
//     previous work's ending.
func (a *API) handleMeta(w http.ResponseWriter, r *http.Request) {
	if !a.metadataOn() {
		writeError(w, http.StatusNotFound, "metadata lookup not enabled")
		return
	}
	lib, path, scope, status, msg := a.authorizedScope(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	book, ok := a.bookAt(w, r, lib, scope, path, "no book at that path", "could not load book")
	if !ok {
		return
	}
	if book.ASIN == "" && book.ISBN == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"matched": false})
		return
	}

	env, err := a.meta.Enrich(r.Context(), book.ASIN, book.ISBN)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeJSON(w, http.StatusOK, map[string]bool{"matched": false})
		return
	case err != nil:
		a.log.Warn("meta lookup failed", "err", err, "library", lib.ID, "path", path)
		writeError(w, http.StatusBadGateway, "metadata service unavailable")
		return
	}

	// The caller's place in the book is read first: a failure here fails the
	// request (hiding was asked for, so the full envelope is not a fallback)
	// before any previous work is fetched upstream.
	q := r.URL.Query()
	hide := q.Get("spoilers") == "hide"
	var (
		chapter  int
		finished bool
	)
	if hide {
		if chapter, finished, err = a.listeningChapter(r.Context(), lib.ID, book); err != nil {
			a.writeCatalogError(w, err, "load progress for meta failed", "could not load progress", "library", lib.ID, "path", path)
			return
		}
	}

	// env is shared with the meta cache and every other caller: it is never
	// modified. Every per-request change below goes onto out, a shallow copy, and
	// a change to anything nested in it (the work, the rails) replaces that part
	// with its own copy first, never writes through to env.
	cp := *env
	out := &cp
	// `local` is an extra like `previous`: when the caller's books can't be
	// placed, the envelope goes out without it (the shared rails, unannotated)
	// rather than failing a lookup that succeeded - and that a client which never
	// reads `local` asked for.
	if rails, _, err := a.localRails(r.Context(), catalog.Ref{LibraryID: lib.ID, Path: book.RelPath}, env); err != nil {
		if r.Context().Err() == nil {
			a.log.Warn("place owned books for meta failed", "err", err, "library", lib.ID, "path", path)
		}
	} else {
		out.Series = rails
	}
	if queryHas(q["include"], "previous") {
		out.Previous = a.meta.Previous(r.Context(), env)
	}
	if hide {
		out = meta.HideSpoilers(out, chapter, finished)
	}
	writeJSON(w, http.StatusOK, out)
}

// localRails is env's rails placed for the caller (meta.Service.PlaceOwned), and
// the books it placed them from: the caller's books in every library they reach
// in a series named like a rail, their main one or another (catalog.SeriesBooks
// over their UserScopes).
// requested is the book env is for. env is shared and never modified.
func (a *API) localRails(ctx context.Context, requested catalog.Ref, env *meta.Enrichment) ([]meta.MetaSeries, []catalog.Book, error) {
	if len(env.Series) == 0 {
		return env.Series, nil, nil
	}
	u := userFrom(ctx)
	scopes, err := a.cat.UserScopes(ctx, u.ID, u.Role == auth.RoleAdmin)
	if err != nil {
		return nil, nil, err
	}
	books, err := a.cat.SeriesBooks(ctx, scopes, meta.SeriesNames(env.Series))
	if err != nil {
		return nil, nil, err
	}
	// A book in several series is a candidate in each, at its position there.
	cands := make([]meta.LocalBook, 0, len(books))
	for _, b := range books {
		for _, s := range b.AllSeries() {
			cands = append(cands, meta.LocalBook{MetaLocal: meta.MetaLocal{LibraryID: b.LibraryID, Path: b.RelPath},
				Series: s.Name, SeriesIndex: s.Position, ASIN: b.ASIN, ISBN: b.ISBN})
		}
	}
	return a.meta.PlaceOwned(ctx, env, meta.MetaLocal(requested), cands), books, nil
}

// listeningChapter is where the caller is in book, for spoilers=hide: the
// chapter number holding their saved position (meta.ChapterAt over the book's
// chapter offsets) and whether they finished it. Read from the CALLER's own
// progress only; no saved progress is chapter 0, not finished.
func (a *API) listeningChapter(ctx context.Context, libraryID int64, book *catalog.Book) (chapter int, finished bool, err error) {
	p, err := a.cat.GetProgress(ctx, userFrom(ctx).ID, catalog.Ref{LibraryID: libraryID, Path: book.RelPath})
	if err != nil || p == nil {
		return 0, false, err
	}
	starts := make([]float64, len(book.Chapters))
	for i, ch := range book.Chapters {
		starts[i] = ch.BookOffset
	}
	return meta.ChapterAt(starts, p.Position), p.Finished, nil
}

// queryHas reports whether a list-valued query param (repeated, or
// comma-separated: include=previous,other) names want.
func queryHas(values []string, want string) bool {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(part) == want {
				return true
			}
		}
	}
	return false
}

// handleMetaWork returns one community work document by its metadata-site work
// id - the "catch me up on the previous book" lookup, since the series rails in
// the /libraries/{id}/meta envelope carry sibling work ids but not their
// characters/recaps.
//
// It is deliberately NOT library-scoped: the community metadata is global,
// read-only, already-public data keyed by a work id that carries no information
// about this server's content, so there is no path to authorize (a work id is
// not addressable to a book). Plain auth (any signed-in user, including demo
// and share-scoped accounts, matching the scope-independent parts of the /meta
// route) is the right gate. The id rides in a QUERY param because work ids are
// slugs that may contain characters awkward in a path segment; internal/meta
// URL-escapes it before calling upstream.
//
// Responses:
//   - metadata disabled: 404 (clients gate on the `metadata` capability).
//   - missing/blank id: 400.
//   - malformed id (too long / control characters): 400.
//   - unknown work id upstream: 404.
//   - upstream unreachable/error: 502.
//   - match: 200 {"work": {...}} (see internal/meta.MetaWork).
func (a *API) handleMetaWork(w http.ResponseWriter, r *http.Request) {
	if !a.metadataOn() {
		writeError(w, http.StatusNotFound, "metadata lookup not enabled")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !validWorkID(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	work, err := a.meta.Work(r.Context(), id)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such work")
	case err != nil:
		// Safe to log the id verbatim: validWorkID has already bounded its length
		// and rejected control characters/newlines.
		a.log.Warn("meta work lookup failed", "err", err, "work", id)
		writeError(w, http.StatusBadGateway, "metadata service unavailable")
	default:
		writeJSON(w, http.StatusOK, struct {
			Work *meta.MetaWork `json:"work"`
		}{work})
	}
}

// maxWorkIDLen bounds an accepted work id. Real metadata-site work slugs are
// tens of bytes ("the-martian"); 200 leaves generous headroom while keeping the
// value small enough to be a safe cache key and log field.
const maxWorkIDLen = 200

// validWorkID reports whether a work id is plausible enough to spend a cache
// entry and an outbound upstream GET on. This is transport-level input hygiene,
// not a slug grammar (the id space belongs to the metadata site, so the server
// must stay permissive about its contents): it only rejects the two shapes that
// have a cost here regardless of what upstream would say - an oversized id (Go
// accepts a ~1MB request line, and the id becomes a cache key and a log field)
// and one carrying control characters or newlines (log injection). Everything
// else is passed through and answered by upstream, with a 404 for an unknown id.
func validWorkID(id string) bool {
	if len(id) > maxWorkIDLen {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// metaStore adapts the catalog's meta_cache rows to meta.Store, so neither
// package imports the other. The store is best effort (a failed read is a miss,
// a failed write is dropped), so this is where its failures are logged. A read
// or write cut short by its own deadline or a cancelled request is routine, not
// a fault, and is not logged.
type metaStore struct {
	cat *catalog.Catalog
	log *slog.Logger
}

func (m metaStore) Load(ctx context.Context, key string) (meta.StoredEntry, bool) {
	e, err := m.cat.GetMetaCache(ctx, key)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("reading the meta cache failed", "err", err)
		}
		return meta.StoredEntry{}, false
	}
	if e == nil {
		return meta.StoredEntry{}, false
	}
	return meta.StoredEntry(*e), true
}

func (m metaStore) Save(ctx context.Context, e meta.StoredEntry) {
	if err := m.cat.PutMetaCache(ctx, catalog.MetaCacheEntry(e)); err != nil && ctx.Err() == nil {
		m.log.Warn("writing the meta cache failed", "err", err)
	}
}
