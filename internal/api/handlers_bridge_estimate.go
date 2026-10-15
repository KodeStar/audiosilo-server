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

// bridgeEstimate is GET /admin/bridge/estimate's answer.
type bridgeEstimate struct {
	Kind      string `json:"kind"`
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	// Source is what the run reads: ebook when an .epub sits directly in the
	// book's folder (Ebook, library-relative), else audio.
	Source          string  `json:"source"`
	Ebook           string  `json:"ebook,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	ASRHours        float64 `json:"asr_hours"`
	// AgentCostUSD is null while the Bridge has no history to cost it from.
	AgentCostUSD *float64 `json:"agent_cost_usd"`
	Subscription bool     `json:"subscription"`
	AgentReady   bool     `json:"agent_ready"`
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
	libraryID, ok := parseOptionalID(q.Get("library_id"))
	path := catalog.CleanRelPath(q.Get("path"))
	if !ok || libraryID <= 0 || path == "" {
		writeError(w, http.StatusBadRequest, "library_id and path are required")
		return
	}
	ctx := r.Context()
	lib, ok := a.libraryFor(w, r, libraryID, "bridge estimate failed")
	if !ok {
		return
	}
	book, err := a.cat.GetBookByPath(ctx, lib.ID, path)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not load the book")
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
	out.Ebook = library.BookEbook(lib.Root, book.RelPath, book.IsFolder)
	var est *bridge.SidecarsEstimates
	if h.Estimates != nil {
		est = h.Estimates.Sidecars
	}
	cost := bridge.EstimateSidecars(book.Duration, out.Ebook != "", est)
	out.Source, out.ASRHours, out.AgentCostUSD = cost.Source, cost.ASRHours, cost.AgentCostUSD
	out.Subscription, out.AgentReady = cost.Subscription, cost.AgentReady

	out.Has.Community = a.communityContent(ctx, book)
	if out.Has.Local, err = a.localContent(ctx, lib.ID, book.RelPath); err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not read the book's attachments")
		return
	}
	if out.Earlier, err = a.earlierWithoutRecaps(ctx, lib.ID, book); err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not list the series")
		return
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

// cachedCommunityRecaps reports whether the cached enrichment of book (never a
// lookup: the series' books are many) holds community recaps.
func (a *API) cachedCommunityRecaps(ctx context.Context, book *catalog.Book) bool {
	if !a.metadataOn() {
		return false
	}
	env, _ := a.meta.CachedEnrichment(ctx, book.ASIN, book.ISBN)
	return env.HasRecaps()
}

// bookAttachments is the book's indexed .audiosilo/ attachments (nil for none).
func (a *API) bookAttachments(ctx context.Context, libraryID int64, path string) (*catalog.Attachments, error) {
	if a.attachments != nil {
		return a.attachments(ctx, libraryID, path)
	}
	return a.cat.BookAttachments(ctx, libraryID, path)
}

// localContent is what the book's generated attachments carry.
func (a *API) localContent(ctx context.Context, libraryID int64, path string) (hasContent, error) {
	att, err := a.bookAttachments(ctx, libraryID, path)
	if err != nil || att == nil {
		return hasContent{}, err
	}
	return hasContent{Characters: att.Characters != nil && len(att.Characters.Characters) > 0,
		Recaps: att.Recaps != nil && (len(att.Recaps.Recaps) > 0 || att.Recaps.InShort != "" || att.Recaps.Ending != "")}, nil
}

// earlierWithoutRecaps is the books of the library before book in its series
// (catalog.EarlierInSeries, any spelling of the series), in reading order, that
// have no recaps yet, community (the cached enrichment only) or generated (their
// attachments); at most maxEarlier.
func (a *API) earlierWithoutRecaps(ctx context.Context, libraryID int64, book *catalog.Book) ([]earlierBook, error) {
	members, err := a.cat.EarlierInSeries(ctx, libraryID, book)
	if err != nil {
		return nil, err
	}
	out := []earlierBook{}
	for _, m := range members {
		if len(out) == maxEarlier {
			break
		}
		if a.cachedCommunityRecaps(ctx, &m.Book) {
			continue
		}
		local, err := a.localContent(ctx, libraryID, m.RelPath)
		if err != nil {
			return nil, err
		}
		if local.Recaps {
			continue
		}
		out = append(out, earlierBook{LibraryID: libraryID, Path: m.RelPath, Title: m.Title,
			SeriesIndex: catalog.FormatSeriesRange(m.Position, m.PositionEnd)})
	}
	return out, nil
}
