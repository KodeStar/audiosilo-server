package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// The Bridge's confirmation dialog (GET /admin/bridge/estimate): what generating
// a book's characters and recaps would read and cost, what the book already has,
// and the earlier books of its series that have none, so an admin can queue
// those first.

// maxEarlier bounds the estimate's earlier books, and maxEarlierChecked how many
// of the nearest earlier books are checked against the community to find them.
const (
	maxEarlier        = 10
	maxEarlierChecked = 3 * maxEarlier
)

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
	Writable bool `json:"writable"`
	// CanDescribe: the Bridge is online and runs sidecars.describe (a
	// description from the notes it kept, Phase 1c); an older Bridge doesn't.
	CanDescribe bool          `json:"can_describe"`
	Has         estimateHas   `json:"has"`
	Earlier     []earlierBook `json:"earlier"`
	OpenJob     *bridge.Job   `json:"open_job,omitempty"`
}

// estimateHas is what a book already has: community content (its community
// work, meta.Service.Coverages: characters, recaps, a community description)
// and generated content (its indexed .audiosilo/ attachments). CommunityUnknown: the community couldn't be asked right now, so
// community is no answer (never read it as "none").
type estimateHas struct {
	Community        hasContent `json:"community"`
	Local            hasContent `json:"local"`
	CommunityUnknown bool       `json:"community_unknown,omitempty"`
}

type hasContent struct {
	Characters  bool `json:"characters"`
	Recaps      bool `json:"recaps"`
	Description bool `json:"description"`
}

// earlierBook is an earlier book of the series with no recaps yet, with what
// generating it is expected to cost (bridge.EstimateSidecars from its audio's
// length: asr_hours, agent_cost_usd null when unknown), so the dialog can quote
// the whole run it queues.
type earlierBook struct {
	LibraryID    int64    `json:"library_id"`
	Path         string   `json:"path"`
	Title        string   `json:"title"`
	SeriesIndex  string   `json:"series_index"`
	ASRHours     float64  `json:"asr_hours"`
	AgentCostUSD *float64 `json:"agent_cost_usd"`
}

// detectEstimate is GET /admin/bridge/estimate's answer for chapters.detect:
// speech recognition and no agent, so the ASR time alone (null while the
// Bridge has sent no figures, or the audio's length is unknown), whether the
// Bridge can write to the library and is online, and the book's open
// chapters.detect job.
type detectEstimate struct {
	Kind            string      `json:"kind"`
	LibraryID       int64       `json:"library_id"`
	Path            string      `json:"path"`
	DurationSeconds float64     `json:"duration_seconds"`
	ASRHours        *float64    `json:"asr_hours"`
	Writable        bool        `json:"writable"`
	Online          bool        `json:"online"`
	OpenJob         *bridge.Job `json:"open_job,omitempty"`
}

// handleAdminBridgeEstimate serves GET /admin/bridge/estimate?library_id=&path=
// &kind=sidecars.generate (kind may be left out) or kind=chapters.detect
// (detectEstimate; brief means nothing to it): the estimate for queueing that
// job on an indexed book. 400 without a library or a path, or for a kind with
// no estimate; 404 for an unknown library or book; 409 kind_unavailable when the
// paired Bridge doesn't run the kind.
//
// brief=1 is the cheap answer for a label or a batch (the book card, a whole
// series): the same shape, but no look into the book's folder for an ebook
// (source audio, ebook omitted) and no scan of the series (earlier []). Both
// carry can_describe and has.*.description, which the book card's "Write a
// description" reads.
func (a *API) handleAdminBridgeEstimate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		kind = bridge.KindSidecars
	}
	if kind != bridge.KindSidecars && kind != bridge.KindDetectChapters {
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
	var est *bridge.SidecarsEstimates
	if h.Estimates != nil {
		est = h.Estimates.Sidecars
	}
	if kind == bridge.KindDetectChapters {
		out := detectEstimate{Kind: kind, LibraryID: lib.ID, Path: book.RelPath, DurationSeconds: book.Duration,
			ASRHours: bridge.EstimateASRHours(book.Duration, est), Writable: h.Writable(lib.ID), Online: h.Online(time.Now())}
		if out.OpenJob, err = a.bridge.OpenJob(ctx, kind, lib.ID, book.RelPath); err != nil {
			a.writeCatalogError(w, err, "bridge estimate failed", "could not read the jobs")
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	out := bridgeEstimate{Kind: kind, LibraryID: lib.ID, Path: book.RelPath, DurationSeconds: book.Duration,
		Writable: h.Writable(lib.ID), CanDescribe: h.Online(time.Now()) && h.Offers(bridge.KindDescribe), Earlier: []earlierBook{}}
	if !brief {
		out.Ebook = library.BookEbook(lib.Root, book.RelPath, book.IsFolder)
	}
	out.SidecarsCost = bridge.EstimateSidecars(book.Duration, out.Ebook != "", est)

	out.Has.Community, out.Has.CommunityUnknown = a.communityContent(ctx, book)
	if out.Has.Local, err = a.localContent(ctx, book); err != nil {
		a.writeCatalogError(w, err, "bridge estimate failed", "could not read the book's attachments")
		return
	}
	if !brief {
		if out.Earlier, err = a.earlierWithoutRecaps(ctx, book, est); err != nil {
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

// communityContent is what the book's community work carries
// (meta.Service.Coverages: its cached enrichment, else the console's bounded
// lookup, never a full Enrich per book, which a series batch would multiply),
// and whether that couldn't be had right now (unknown, logged: never to be read
// as "none"). Metadata off, no identifier or no match is nothing.
func (a *API) communityContent(ctx context.Context, book *catalog.Book) (has hasContent, unknown bool) {
	if !a.metadataOn() || (book.ASIN == "" && book.ISBN == "") {
		return hasContent{}, false
	}
	c := a.meta.Coverages(ctx, []meta.BookIDs{{ASIN: book.ASIN, ISBN: book.ISBN}})[0]
	if !c.Known && ctx.Err() == nil {
		a.log.Warn("community lookup for the bridge estimate failed", "library", book.LibraryID, "path", book.RelPath)
	}
	return hasContent{Characters: c.Characters, Recaps: c.Recaps, Description: c.Description}, !c.Known
}

// localContent is what the book's generated attachments carry (a stored file is
// never empty: metadata.ParseCharacters, ParseRecaps and ParseDescription drop
// one), read without decoding them.
func (a *API) localContent(ctx context.Context, book *catalog.Book) (hasContent, error) {
	p, err := a.cat.BookAttachmentParts(ctx, book.ID)
	return hasContent{Characters: p.Characters, Recaps: p.Recaps, Description: p.Description}, err
}

// earlierWithoutRecaps is the books of the library before book in its series
// with no recaps yet, the nearest maxEarlier (a book's recaps build on the ones
// just before it), in reading order: those with no generated recaps
// (catalog.EarlierInSeries, any spelling of the series) and, metadata on, whose
// community work has none either (meta.Service.Coverages, batched a chunk at a
// time over the nearest maxEarlierChecked at most; a book it can't ask about
// right now is left out, never offered on a guess). Each carries what generating
// it is expected to cost, from est (audio: no look into its folder).
func (a *API) earlierWithoutRecaps(ctx context.Context, book *catalog.Book, est *bridge.SidecarsEstimates) ([]earlierBook, error) {
	members, err := a.cat.EarlierInSeries(ctx, book.LibraryID, book)
	if err != nil {
		return nil, err
	}
	out := []earlierBook{} // nearest first, until reversed
	for end, checked := len(members), 0; end > 0 && len(out) < maxEarlier && checked < maxEarlierChecked; end -= maxEarlier {
		chunk := members[max(0, end-maxEarlier):end]
		checked += len(chunk)
		var cov []meta.Coverage
		if a.metadataOn() {
			ids := make([]meta.BookIDs, len(chunk))
			for i, m := range chunk {
				ids[i] = meta.BookIDs{ASIN: m.ASIN, ISBN: m.ISBN}
			}
			cov = a.meta.Coverages(ctx, ids)
		}
		for i := len(chunk) - 1; i >= 0 && len(out) < maxEarlier; i-- {
			if cov != nil && (cov[i].Recaps || !cov[i].Known) {
				continue
			}
			m := chunk[i]
			c := bridge.EstimateSidecars(m.Duration, false, est)
			out = append(out, earlierBook{LibraryID: m.LibraryID, Path: m.RelPath, Title: m.Title,
				SeriesIndex: catalog.FormatSeriesRange(m.Position, m.PositionEnd),
				ASRHours:    c.ASRHours, AgentCostUSD: c.AgentCostUSD})
		}
	}
	slices.Reverse(out)
	return out, nil
}
