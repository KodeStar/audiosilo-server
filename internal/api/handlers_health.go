package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// The admin console's Health and Jobs screens (Phase 3). Transport only: the
// issues are catalog queries (issues.go), the queue and schedules live in the
// scanner (library/jobs.go), the history in catalog (scanruns.go).

// offlineLibrary is a library whose root can't be read right now, with what the
// scanner kept for it (nothing was pruned).
type offlineLibrary struct {
	LibraryID int64  `json:"library_id"`
	Name      string `json:"name"`
	Root      string `json:"root"`
	Books     int    `json:"books"`
	Listeners int    `json:"listeners"`
}

// handleIssues serves GET /admin/issues: each issue category's count, ignored
// count and a few books to show, the offline libraries, and when a scan last
// finished. "Not matched" is left out while community metadata is off (there is
// nothing to match against).
func (a *API) handleIssues(w http.ResponseWriter, r *http.Request) {
	kinds := slices.DeleteFunc(slices.Clone(catalog.IssueKinds), func(k string) bool {
		return k == catalog.IssueUnmatched && !a.metadataOn()
	})
	counts, err := a.cat.IssueCounts(r.Context(), kinds)
	if err != nil {
		a.writeCatalogError(w, err, "issue counts failed", "could not count issues")
		return
	}
	offline, err := a.offlineLibraries(r)
	if err != nil {
		a.writeCatalogError(w, err, "issue counts failed", "could not check the libraries")
		return
	}
	checked, err := a.cat.LastScanFinished(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "issue counts failed", "could not read scan history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": counts, "offline": offline, "checked_at": checked,
	})
}

// offlineLibraries lists the libraries whose root can't be read right now, with
// their indexed books and listeners.
func (a *API) offlineLibraries(r *http.Request) ([]offlineLibrary, error) {
	ctx := r.Context()
	libs, err := a.cat.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := a.cat.CountBooksByLibrary(ctx)
	if err != nil {
		return nil, err
	}
	available := a.scanner.RootsAvailable(libs, counts)
	offline := []offlineLibrary{}
	var listeners map[int64]int
	for _, l := range libs {
		if available[l.ID] {
			continue
		}
		if listeners == nil {
			if listeners, err = a.cat.ListenersByLibrary(ctx); err != nil {
				return nil, err
			}
		}
		offline = append(offline, offlineLibrary{LibraryID: l.ID, Name: l.Name, Root: l.Root,
			Books: counts[l.ID], Listeners: listeners[l.ID]})
	}
	return offline, nil
}

// handleDuplicates serves GET /admin/issues/duplicates: groups of books that look
// like the same book within a library (?library_id= narrows it; ?ignored=true
// includes the groups an admin said are different books).
func (a *API) handleDuplicates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, ok := parseLibraryID(q.Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	groups, err := a.cat.DuplicateGroups(r.Context(), libID, q.Get("ignored") == "true")
	if err != nil {
		a.writeCatalogError(w, err, "duplicates failed", "could not find duplicates")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// ignoreRequest is the body of POST/DELETE /admin/issues/ignore.
type ignoreRequest struct {
	Kind  string        `json:"kind"`
	Books []catalog.Ref `json:"books"`
}

// handleIgnoreIssue serves POST /admin/issues/ignore: stop showing these books
// under a category (idempotent; path-keyed, so it survives rescans and moves).
func (a *API) handleIgnoreIssue(w http.ResponseWriter, r *http.Request) {
	a.applyIgnore(w, r, func(req ignoreRequest) error {
		return a.cat.IgnoreIssue(r.Context(), req.Kind, req.Books, userFrom(r.Context()).ID)
	})
}

// handleUnignoreIssue serves DELETE /admin/issues/ignore: show them again (Undo).
func (a *API) handleUnignoreIssue(w http.ResponseWriter, r *http.Request) {
	a.applyIgnore(w, r, func(req ignoreRequest) error {
		return a.cat.UnignoreIssue(r.Context(), req.Kind, req.Books)
	})
}

func (a *API) applyIgnore(w http.ResponseWriter, r *http.Request, apply func(ignoreRequest) error) {
	var req ignoreRequest
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch {
	case len(req.Books) == 0:
		writeError(w, http.StatusBadRequest, "books is required")
		return
	case len(req.Books) > maxBulkBooks:
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge, "too many books at once (at most 1000)")
		return
	}
	for i, b := range req.Books {
		req.Books[i].Path = catalog.CleanRelPath(b.Path)
		if req.Books[i].Path == "" || b.LibraryID <= 0 {
			writeError(w, http.StatusBadRequest, "every book needs a library_id and a path")
			return
		}
	}
	if err := apply(req); err != nil {
		a.writeCatalogError(w, err, "ignore issue failed", "could not save", "kind", req.Kind)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRescanBook serves POST /admin/libraries/{id}/book/rescan?path=: read one
// book's files again now (tags, ffprobe, cover, read problems), outside the job
// queue (it's one book), and return its page. A path the library no longer has a
// book at (gone, or ignored) is a 404 with code not_indexable.
func (a *API) handleRescanBook(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if _, err := a.scanner.IndexPath(r.Context(), *lib, p); err != nil {
		if errors.Is(err, library.ErrNotIndexable) || errors.Is(err, library.ErrOutsideRoot) {
			writeErrorCode(w, http.StatusNotFound, codeNotIndexable, "there is no book at that path any more")
			return
		}
		a.writeCatalogError(w, err, "rescan book failed", "could not rescan the book", "library", lib.ID, "path", p)
		return
	}
	a.writeBookDetail(w, r, lib.ID, p)
}

// scheduledScan is one library's next scheduled scan.
type scheduledScan struct {
	LibraryID   int64  `json:"library_id"`
	LibraryName string `json:"library_name"`
	Schedule    string `json:"schedule"`
	NextAt      string `json:"next_at"`
}

// handleJobs serves GET /admin/jobs: the running scan (with progress), the queue,
// and every scheduled library's next scan.
func (a *API) handleJobs(w http.ResponseWriter, r *http.Request) {
	running, queued := a.scanner.Jobs()
	libs, err := a.cat.ListLibraries(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "list jobs failed", "could not list libraries")
		return
	}
	next, err := a.scanner.NextScans(r.Context(), libs)
	if err != nil {
		a.writeCatalogError(w, err, "list jobs failed", "could not read the schedules")
		return
	}
	schedules := []scheduledScan{}
	for _, l := range libs {
		if at, ok := next[l.ID]; ok {
			schedules = append(schedules, scheduledScan{LibraryID: l.ID, LibraryName: l.Name,
				Schedule: l.ScanSchedule, NextAt: at.UTC().Format(time.RFC3339)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": running, "queued": queued, "schedules": schedules})
}

// handleCancelJob serves DELETE /admin/jobs/{id}: drop a queued scan or stop the
// running one (it stops before pruning; nothing is removed).
func (a *API) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	if !a.scanner.Cancel(id) {
		writeError(w, http.StatusNotFound, "no such job (it may have finished)")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleScanRuns serves GET /admin/scan-runs: recorded scans, newest first,
// without logs (?library_id=, ?before=<run id> for the next page, ?limit=).
func (a *API) handleScanRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, ok := parseLibraryID(q.Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid before")
			return
		}
		before = n
	}
	limit := queryInt(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	runs, err := a.cat.ListScanRuns(r.Context(), libID, before, limit)
	if err != nil {
		a.writeCatalogError(w, err, "list scan runs failed", "could not read scan history")
		return
	}
	resp := map[string]any{"runs": runs}
	if len(runs) == limit {
		resp["next_before"] = runs[len(runs)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleScanRun serves GET /admin/scan-runs/{id}: one recorded scan with its log.
func (a *API) handleScanRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, err := a.cat.GetScanRun(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such scan")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "get scan run failed", "could not read the scan")
		return
	}
	writeJSON(w, http.StatusOK, run)
}
