package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// The Bridge's confirmation dialog (GET /admin/bridge/estimate): what generating
// a book's characters and recaps would read and cost, what the book already has,
// and the earlier books of its series that have none, so an admin can queue
// those first.

// maxEarlier bounds the estimate's earlier books.
const maxEarlier = 10

// bridgeEstimate is GET /admin/bridge/estimate's answer. The cost figures are
// bridge.EstimateSidecars' (source, asr_hours, agent_cost_usd: null while the
// Bridge has no history to cost it from, subscription, agent_ready).
type bridgeEstimate struct {
	Kind      string `json:"kind"`
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	bridge.SidecarsCost
	// Ebook is the .epub the run reads (library-relative; source ebook), omitted
	// for audio.
	Ebook           string  `json:"ebook,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	// Writable: the Bridge last reported the library mapped and writable.
	Writable bool          `json:"writable"`
	Has      estimateHas   `json:"has"`
	Earlier  []earlierBook `json:"earlier"`
	OpenJob  *bridge.Job   `json:"open_job,omitempty"`
}

// estimateHas is what a book already has: community content (the /meta
// enrichment) and generated content (its indexed .audiosilo/ attachments).
type estimateHas struct {
	Community hasContent `json:"community"`
	Local     hasContent `json:"local"`
}

type hasContent struct {
	Characters bool `json:"characters"`
	Recaps     bool `json:"recaps"`
}

// earlierBook is an earlier book of the series with no recaps yet.
type earlierBook struct {
	LibraryID   int64  `json:"library_id"`
	Path        string `json:"path"`
	Title       string `json:"title"`
	SeriesIndex string `json:"series_index"`
}

// handleAdminBridgeEstimate serves GET /admin/bridge/estimate?library_id=&path=
// &kind=sidecars.generate (kind may be left out): the estimate for queueing that
// job on an indexed book. 400 without a library or a path, or for a kind with
// no estimate; 404 for an unknown library or book; 409 kind_unavailable when the
// paired Bridge doesn't run the kind.
//
// brief=1 is the cheap answer for a label or a batch (the book card, a whole
// series): the same shape, but no look into the book's folder for an ebook
// (source audio, ebook omitted) and no scan of the series (earlier []).
func (a *API) handleAdminBridgeEstimate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		kind = bridge.KindSidecars
	}
	if kind != bridge.KindSidecars {
		writeError(w, http.StatusBadRequest, "no estimate for that kind")
		return
	}
	brief := q.Get("brief") == "1"
	libraryID, ok := parseOptionalID(q.Get("library_id"))
	path := catalog.CleanRelPath(q.Get("path"))
	if !ok || libraryID <= 0 || path == "" {
		writeError(w, http.StatusBadRequest, "library_id and path are required")
		return
	}
	ctx := r.Context()
	lib, book, ok := a.adminBridgeBook(w, r, libraryID, path, "bridge estimate failed")
	if !ok {
		return
	}
	h, err := a.bridge.LastHeartbeat(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not read the bridge")
		return
	}
	if h == nil || !h.Offers(kind) {
		a.writeBridgeError(w, bridge.ErrKindUnavailable, "bridge estimate failed")
		return
	}

	out := bridgeEstimate{Kind: kind, LibraryID: lib.ID, Path: book.RelPath, DurationSeconds: book.Duration,
		Writable: h.Writable(lib.ID), Earlier: []earlierBook{}}
	if !brief {
		out.Ebook = library.BookEbook(lib.Root, book.RelPath, book.IsFolder)
	}
	var est *bridge.SidecarsEstimates
	if h.Estimates != nil {
		est = h.Estimates.Sidecars
	}
	out.SidecarsCost = bridge.EstimateSidecars(book.Duration, out.Ebook != "", est)

	out.Has.Community = a.communityContent(ctx, book)
	if out.Has.Local, err = a.localContent(ctx, book); err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not read the book's attachments")
		return
	}
	if !brief {
		if out.Earlier, err = a.earlierWithoutRecaps(ctx, book); err != nil {
			a.writeCatalogError(w, err, "bridge estimate failed", "could not list the series")
			return
		}
	}
	if out.OpenJob, err = a.bridge.OpenJob(ctx, kind, lib.ID, book.RelPath); err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not read the jobs")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// communityContent is what the book's community work carries, from its /meta
// enrichment (the shared, cached lookup). Metadata off, no identifier or no
// match is nothing; so is a failed lookup (logged): the dialog still opens.
func (a *API) communityContent(ctx context.Context, book *catalog.Book) hasContent {
	if !a.metadataOn() || (book.ASIN == "" && book.ISBN == "") {
		return hasContent{}
	}
	env, err := a.meta.Enrich(ctx, book.ASIN, book.ISBN)
	if err != nil {
		if !errors.Is(err, meta.ErrNotFound) && ctx.Err() == nil {
			a.log.Warn("meta lookup for the bridge estimate failed", "err", err, "library", book.LibraryID, "path", book.RelPath)
		}
		return hasContent{}
	}
	return hasContent{Characters: env.HasCharacters(), Recaps: env.HasRecaps()}
}

// localContent is what the book's generated attachments carry (a stored file is
// never empty: metadata.ParseCharacters and ParseRecaps drop one).
func (a *API) localContent(ctx context.Context, book *catalog.Book) (hasContent, error) {
	att, err := a.cat.BookAttachments(ctx, book.ID)
	if err != nil || att == nil {
		return hasContent{}, err
	}
	return hasContent{Characters: att.Characters != nil, Recaps: att.Recaps != nil}, nil
}

// earlierWithoutRecaps is the books of the library before book in its series
// with no generated recaps (catalog.EarlierInSeries, any spelling of the
// series), in reading order, less those whose cached community enrichment holds
// recaps (never a lookup: the series' books are many); at most maxEarlier.
func (a *API) earlierWithoutRecaps(ctx context.Context, book *catalog.Book) ([]earlierBook, error) {
	members, err := a.cat.EarlierInSeries(ctx, book.LibraryID, book)
	if err != nil {
		return nil, err
	}
	out := []earlierBook{}
	for _, m := range members {
		if len(out) == maxEarlier {
			break
		}
		if a.metadataOn() && a.meta.CachedEnrichment(ctx, m.ASIN, m.ISBN).HasRecaps() {
			continue
		}
		out = append(out, earlierBook{LibraryID: m.LibraryID, Path: m.RelPath, Title: m.Title,
			SeriesIndex: catalog.FormatSeriesRange(m.Position, m.PositionEnd)})
	}
	return out, nil
}
