package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/meta"
)

// LibriVox L6 (handlers_librivox.go): Add from LibriVox. An admin searches the
// community metadata database's LibriVox works (GET /admin/librivox/search),
// opens one (GET /admin/librivox/work: its LibriVox recordings, where each is
// on this server already and its open fetch), and adds a recording to a books
// library (POST /admin/librivox/fetch: the Bridge's librivox.fetch, the
// recording's identity resolved here; OnEnded's catalog.FoldLibrivoxFetch
// records the placed book as it). Admin only (requireAdmin), 404 metadata_off
// while the lookup is off; no managed module and no /server flag: the console
// offers it when the Bridge's features list "librivox".

const (
	// librivoxSearchDefault and librivoxSearchMax bound a search's page
	// (metaserve's own default and cap for works/search).
	librivoxSearchDefault = 20
	librivoxSearchMax     = 50
)

// langTag is a language filter as metaserve takes one: a primary subtag of 2
// or 3 letters and optional subtags ("en", "pt-br", "zh-hant-tw"). One tag
// only: the console asks for one language.
var langTag = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8}){0,3}$`)

// handleLibrivoxSearch serves GET /admin/librivox/search?q=&lang=&limit=: the
// works with a LibriVox recording, best first. 200 {"results": [{work_id,
// title, authors, language, release_date}]}; 400 for a missing or long q, a
// lang that is no language tag, a limit outside 1-50; 404 metadata_off; 502
// when the metadata service can't answer.
func (a *API) handleLibrivoxSearch(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	query := r.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	lang := strings.TrimSpace(query.Get("lang"))
	limit := librivoxSearchDefault
	switch {
	case q == "":
		writeError(w, http.StatusBadRequest, "q is required")
		return
	case utf8.RuneCountInString(q) > maxMatchQuery:
		writeError(w, http.StatusBadRequest, "query too long")
		return
	case lang != "" && !langTag.MatchString(lang):
		writeError(w, http.StatusBadRequest, "lang must be a language tag (en, pt-br)")
		return
	}
	if v := query.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > librivoxSearchMax {
			writeError(w, http.StatusBadRequest, "limit must be 1-50")
			return
		}
		limit = n
	}
	hits, err := a.meta.LibrivoxSearch(r.Context(), q, strings.ToLower(lang), limit)
	if err != nil {
		a.log.Warn("librivox search failed", "err", err)
		writeLookupFailed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

// librivoxJobRef is a recording's open librivox.fetch, as the work route
// lists it.
type librivoxJobRef struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

// librivoxRecording is one LibriVox recording of GET /admin/librivox/work:
// the metadata service's, plus the books on this server recorded as it
// (catalog.WorkRecordingBooks) and its open fetch in any library (null for none).
type librivoxRecording struct {
	meta.LibrivoxRecording
	InLibrary []catalog.Ref   `json:"in_library"`
	Job       *librivoxJobRef `json:"job"`
}

// handleLibrivoxWork serves GET /admin/librivox/work?id=: a work and its
// LibriVox recordings. 200 {"work": {id, title, authors, language},
// "recordings": [{recording_id, librivox_id, narrators, runtime_min,
// chapter_count, cover_url, url, in_library: [{library_id, path}], job:
// {id, state} | null}]}; 400 for a missing or malformed id; 404 metadata_off,
// 404 not_found for an unknown work; 502 when the metadata service can't
// answer.
func (a *API) handleLibrivoxWork(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || !validWorkID(id) {
		writeError(w, http.StatusBadRequest, "id must be a work id")
		return
	}
	work, err := a.meta.LibrivoxWork(r.Context(), id)
	if errors.Is(err, meta.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "no such work")
		return
	}
	if err != nil {
		a.log.Warn("librivox work failed", "err", err, "work", id)
		writeLookupFailed(w)
		return
	}
	projects := make([]int, 0, len(work.Recordings))
	for _, rec := range work.Recordings {
		projects = append(projects, rec.LibrivoxID)
	}
	open := map[int]*bridge.Job{}
	if a.bridge != nil {
		if open, err = a.bridge.OpenLibrivoxFetches(r.Context(), projects); err != nil {
			a.log.Warn("librivox work: open fetches failed", "err", err)
			writeError(w, http.StatusInternalServerError, "could not read the LibriVox jobs")
			return
		}
	}
	here, err := a.cat.WorkRecordingBooks(r.Context(), work.Work.ID)
	if err != nil {
		a.writeCatalogError(w, err, "librivox work: books failed", "could not read the library")
		return
	}
	recs := make([]librivoxRecording, 0, len(work.Recordings))
	for _, rec := range work.Recordings {
		in := here[rec.RecordingID]
		if in == nil {
			in = []catalog.Ref{} // the wire's in_library is a list, empty for none
		}
		out := librivoxRecording{LibrivoxRecording: rec, InLibrary: in}
		if j := open[rec.LibrivoxID]; j != nil {
			out.Job = &librivoxJobRef{ID: j.ID, State: j.State}
		}
		recs = append(recs, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{"work": work.Work, "recordings": recs})
}

// handleLibrivoxFetch serves POST /admin/librivox/fetch {library_id,
// librivox_id}: add a LibriVox project to a books library. The project's work
// and recording are resolved here (lookup?librivox_id=) and travel in the
// job's params. 202 {"job": {...}} with the queued job (or the open one for the
// same library and project); 400 for a malformed body; 404 library not found,
// 404 not_found (no such project), 404 metadata_off; 409 in_library {library_id,
// path} when the recording is already on this server (any library), 409
// not_book_library, kind_unavailable (no Bridge, or one that doesn't run it),
// library_not_writable; 502 when the metadata service can't answer. Audited
// (bridge.job) when queued.
func (a *API) handleLibrivoxFetch(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	var req struct {
		LibraryID  int64 `json:"library_id"`
		LibrivoxID int   `json:"librivox_id"`
	}
	if err := decodeJSON(r, &req, 4<<10); err != nil || req.LibraryID <= 0 || req.LibrivoxID <= 0 {
		writeError(w, http.StatusBadRequest, "library_id and librivox_id are required")
		return
	}
	lib, ok := a.libraryFor(w, r, req.LibraryID, "librivox fetch: library failed")
	if !ok {
		return
	}
	if lib.Kind == catalog.KindPodcasts {
		writeErrorCode(w, http.StatusConflict, "not_book_library", "This action needs a books library.")
		return
	}
	project, err := a.meta.LibrivoxRecordingOf(r.Context(), req.LibrivoxID)
	if errors.Is(err, meta.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "no such LibriVox project in the community metadata")
		return
	}
	if err != nil {
		a.log.Warn("librivox lookup failed", "err", err, "librivox_id", req.LibrivoxID)
		writeLookupFailed(w)
		return
	}
	in, err := a.cat.RecordingBooks(r.Context(), project.WorkID, project.RecordingID)
	if err != nil {
		a.writeCatalogError(w, err, "librivox fetch: books failed", "could not read the library")
		return
	}
	if len(in) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this recording is already on this server",
			"code": "in_library", "library_id": in[0].LibraryID, "path": in[0].Path})
		return
	}
	if a.bridge == nil {
		a.writeBridgeError(w, bridge.ErrKindUnavailable, "librivox fetch failed")
		return
	}
	job, created, err := a.bridge.Enqueue(r.Context(), bridge.LibrivoxFetch(bridge.LibrivoxFetchParams{
		LibrivoxID: req.LibrivoxID, WorkID: project.WorkID, RecordingID: project.RecordingID, LibraryID: lib.ID,
		Title: project.Title,
	}, userFrom(r.Context()).ID))
	if !a.writeBridgeError(w, err, "librivox fetch failed") {
		return
	}
	if created {
		a.audit(r, "bridge.job", "", map[string]any{"job": job.ID, "kind": job.Kind, "library": lib.ID,
			"librivox_id": req.LibrivoxID})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}
