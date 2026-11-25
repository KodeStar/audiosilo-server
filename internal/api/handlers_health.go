package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// rescanBookTimeout bounds one book's re-read (POST .../book/rescan).
const rescanBookTimeout = 10 * time.Minute

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
// nothing to match against), and "unknown ASIN" while the unknown-ASIN report
// is off or has no answers yet (unknownASINListed: only its lookups find such a
// book, so its count would read as all clear when nothing was checked).
//
// Counting is a pass over the whole library (about 100 ms at 5,000 books), so
// the answer is kept in the list cache (listcache.go) under the content
// generation, keyed on what it is computed from besides the database: the
// kinds and the offline libraries (a filesystem probe, cached for a few
// seconds in library.RootsAvailable, so made before the lookup, listeners
// included). It is exact: any content write (a fix, an ignore, a scan) moves
// the generation, and the next request counts again (server#245). Two tables
// written as user state are read: how many people have progress on each
// duplicate copy (its ranking), so the key also takes the listener generation
// (a progress row added or removed moves it, a save to an existing row does
// not; one moved to another book is a content write: server#263); and which
// ASINs the unknown-ASIN report found unknown (unknown_asin), so a row added
// or removed there, or its answer changed, is a content write, a lookup that
// only renews its times is not (store's userStateTables).
func (a *API) handleIssues(w http.ResponseWriter, r *http.Request) {
	gen, cacheable := a.cat.ContentGeneration() // before any read (listcache.go)
	listeners := a.cat.ListenerGeneration()
	unknownListed := a.unknownASINListed(r.Context())
	kinds := slices.DeleteFunc(slices.Clone(catalog.IssueKinds), func(k string) bool {
		return (k == catalog.IssueUnmatched && !a.metadataOn()) ||
			(k == catalog.IssueUnknownASIN && !unknownListed)
	})
	offline, err := a.offlineLibraries(r)
	if err != nil {
		a.writeCatalogError(w, err, "issue counts failed", "could not check the libraries")
		return
	}
	write := a.cachedList(w, r, gen, cacheable, nil, kinds, offline, listeners)
	if write == nil {
		return
	}
	counts, err := a.cat.IssueCounts(r.Context(), kinds)
	if err != nil {
		a.writeCatalogError(w, err, "issue counts failed", "could not count issues")
		return
	}
	checked, err := a.cat.LastScanFinished(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "issue counts failed", "could not read scan history")
		return
	}
	write(map[string]any{"categories": counts, "offline": offline, "checked_at": checked})
}

// offlineLibraries lists the libraries whose root can't be read right now,
// podcast libraries among them (server#179), with their indexed books (a
// podcast library's episodes) and listeners.
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
// lists instead the groups an admin said are different books). ?across=1 also
// lists direct pairs across libraries. Each copy names its library. With
// ?library_id=, only comparisons involving that library are included.
// A page at a time: ?limit= groups (at most 500, the default) after ?cursor=,
// the last page's next_cursor; total counts every page's groups.
func (a *API) handleDuplicates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, ok := parseOptionalID(q.Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	page, err := a.cat.DuplicatePage(r.Context(), catalog.DuplicateQuery{
		LibraryID: libID,
		Ignored:   q.Get("ignored") == "true",
		Across:    q.Get("across") == "1" || q.Get("across") == "true",
		Limit:     queryInt(r, "limit", 0),
		Cursor:    q.Get("cursor"),
	})
	if err != nil {
		a.writeCatalogError(w, err, "duplicates failed", "could not find duplicates")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleSeriesSpellings serves GET /admin/issues/series-spellings: the series a
// library's books spell more than one way, each with its spellings and the one to
// offer (?library_id= narrows it; ?ignored=true lists those with an ignored book).
func (a *API) handleSeriesSpellings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, ok := parseOptionalID(q.Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	groups, err := a.cat.SeriesSpellingGroups(r.Context(), libID, q.Get("ignored") == "true")
	if err != nil {
		a.writeCatalogError(w, err, "series spellings failed", "could not find series spellings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// ignoreRequest is the body of POST/DELETE /admin/issues/ignore.
type ignoreRequest struct {
	Kind     string                   `json:"kind"`
	Books    boundedList[catalog.Ref] `json:"books"`
	GroupKey string                   `json:"group_key"`
}

// handleIgnoreIssue serves POST /admin/issues/ignore: stop showing these books
// under a category (idempotent; path-keyed, so it survives rescans and moves).
func (a *API) handleIgnoreIssue(w http.ResponseWriter, r *http.Request) {
	a.applyIgnore(w, r, "issue.ignore", func(req ignoreRequest) error {
		return a.cat.IgnoreIssue(r.Context(), req.Kind, req.Books, userFrom(r.Context()).ID)
	})
}

// handleUnignoreIssue serves DELETE /admin/issues/ignore: show them again (Undo).
func (a *API) handleUnignoreIssue(w http.ResponseWriter, r *http.Request) {
	a.applyIgnore(w, r, "issue.unignore", func(req ignoreRequest) error {
		return a.cat.UnignoreIssue(r.Context(), req.Kind, req.Books)
	})
}

func (a *API) applyIgnore(w http.ResponseWriter, r *http.Request, action string, apply func(ignoreRequest) error) {
	var req ignoreRequest
	if !decodeBody(w, r, &req, selectionBodyMax) {
		return
	}
	switch {
	case len(req.Books) == 0:
		writeError(w, http.StatusBadRequest, "books is required")
		return
	case refsTooLong(w, req.Books):
		return
	}
	for i, b := range req.Books {
		req.Books[i].Path = catalog.CleanRelPath(b.Path)
		if req.Books[i].Path == "" || b.LibraryID <= 0 {
			writeError(w, http.StatusBadRequest, "every book needs a library_id and a path")
			return
		}
	}
	if req.GroupKey != "" {
		if req.Kind != catalog.IssueDuplicate || len(req.Books) != 1 || len(req.GroupKey) != 64 {
			writeError(w, http.StatusBadRequest, "group_key needs one member of a duplicate group")
			return
		}
		refs, err := a.cat.DuplicateSelection(r.Context(), req.GroupKey, req.Books[0])
		if err != nil {
			a.writeCatalogError(w, err, "duplicate selection failed", "could not find duplicate group")
			return
		}
		if len(refs) == 0 {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
			} else {
				writeError(w, http.StatusBadRequest, "duplicate group has changed")
			}
			return
		}
		req.Books = refs
	}
	if err := apply(req); err != nil {
		a.writeCatalogError(w, err, "ignore issue failed", "could not save", "kind", req.Kind)
		return
	}
	a.audit(r, action, "", map[string]any{"kind": req.Kind, "books": len(req.Books)})
	w.WriteHeader(http.StatusNoContent)
}

// handleRescanBook serves POST /admin/libraries/{id}/book/rescan?path=: read one
// book's files again now (tags, ffprobe, cover, read problems), outside the job
// queue (it's one book), and return its page. A path the library no longer has a
// book at (gone, or ignored) is a 404 with code not_indexable. A read still
// going where the request's waits on another read of its book end
// (flightWaitEnd, ahead of the request timeout) is answered 202
// {"still_reading": true}: it goes on
// and is saved (the book's next look shows it), so the console says so rather
// than meeting the request timeout's bare 503 (server#224).
func (a *API) handleRescanBook(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	// No wait cap (lookCtx's): the read is not given up on at the request's
	// time, the answer is (flightWaitEnd). A look that gets no answer in
	// fileWait still ends it, as a 503 share_not_answering when in time.
	// Another read of the book under way (a second click's racing the first,
	// a scan's) is waited on for as long as it moves on, then this one reads
	// the book itself, so a fix made since that read began is picked up
	// (WithUncappedWait).
	look := library.WithUncappedWait(library.WithLookTimeout(a.baseCtx, a.fileWait))
	rr := a.startReread(lib, p, func() error {
		_, err := readAt(a, look, func(ctx context.Context) (*catalog.Book, error) {
			return a.scanner.ReindexPath(ctx, *lib, p)
		})
		return err
	})
	wait := time.NewTimer(time.Until(a.flightWaitEnd(r.Context())))
	defer wait.Stop()
	var err error
	select {
	case err = <-rr.done:
	case <-wait.C:
		if rr.leave() {
			writeJSON(w, http.StatusAccepted, map[string]bool{"still_reading": true})
			return
		}
		err = <-rr.done // finished as the answer fell due: its own answer
	}
	if err != nil {
		a.writeRescanError(w, lib, p, err)
		return
	}
	a.writeBookDetail(w, r, lib, p)
}

// reread is an admin's Re-read on a goroutine of its own (startReread): done
// gets its outcome for the request, unless the request answered
// still_reading first (leave), when the read logs its outcome itself.
type reread struct {
	done  chan error
	state atomic.Int32 // rereadRunning, rereadEnded or rereadLeft
}

const (
	rereadRunning int32 = iota
	rereadEnded         // its outcome is on done, for the request
	rereadLeft          // the request answered still_reading: the read logs its outcome
)

// startReread runs read, a Re-read of what is at p in lib, counted in
// a.bookReads, so one that outlives its answer ends before the database closes
// (WaitMediaCaches; the base context ends it), and its outcome logged on that
// goroutine too. A panic is the read's failure, as when it ran on the
// request's goroutine (net/http recovered it there): unrecovered off it, it
// would end the server.
func (a *API) startReread(lib *catalog.Library, p string, read func() error) *reread {
	rr := &reread{done: make(chan error, 1)}
	if !a.startBookRead(func() {
		var err error
		defer func() {
			if v := recover(); v != nil {
				a.log.Error("re-read panicked", "library", lib.ID, "path", p, "panic", v, "stack", string(debug.Stack()))
				err = fmt.Errorf("re-read panicked: %v", v)
			}
			if !rr.state.CompareAndSwap(rereadRunning, rereadEnded) {
				a.logLateReread(lib, p, err)
				return
			}
			rr.done <- err
		}()
		err = read()
	}) {
		rr.state.Store(rereadEnded)
		rr.done <- library.ErrStopping
	}
	return rr
}

// leave reports whether the read was still going as its request's answer fell
// due: it then logs its own outcome when it ends. False when it has ended, its
// outcome on done (sent just after it ended).
func (rr *reread) leave() bool {
	return rr.state.CompareAndSwap(rereadRunning, rereadLeft)
}

// logLateReread logs how a Re-read answered still_reading ended (err): the
// admin was told it goes on and Health lists only the read problems it
// records, so a failure is a warning; a stop, or a path with no book, is none.
func (a *API) logLateReread(lib *catalog.Library, p string, err error) {
	if err == nil || errors.Is(err, library.ErrStopping) ||
		errors.Is(err, library.ErrNotIndexable) || errors.Is(err, library.ErrOutsideRoot) {
		return
	}
	a.log.Warn("re-read failed after its still_reading answer", "library", lib.ID, "path", p, "err", err)
}

// rescanAt runs read, a re-read of what is at p in lib, as readAt does under
// a.lookCtx (each look given fileWait, the waits on another read of the book
// ending flightWaitMargin ahead of the request timeout, so a share that
// stopped answering is a 503 share_not_answering with Retry-After, not the
// timeout's), and answers its error itself (writeRescanError), reporting
// whether there was none.
func rescanAt[T any](a *API, w http.ResponseWriter, lib *catalog.Library, p string, read func(context.Context) (T, error)) (T, bool) {
	out, err := readAt(a, a.lookCtx(a.baseCtx), read)
	if err != nil {
		a.writeRescanError(w, lib, p, err)
		return out, false
	}
	return out, true
}

// readAt runs read, a re-read of what is at a path, on look (a.baseCtx with
// the request's look bounds): detached from the request timeout, so a slow
// re-read is still saved, and bounded by rescanBookTimeout. A read the
// server's stop ended is library.ErrStopping.
func readAt[T any](a *API, look context.Context, read func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(look, rescanBookTimeout)
	defer cancel()
	out, err := read(ctx)
	if err != nil && a.baseCtx.Err() != nil {
		// The server's stop ended the read (its context is the server's): a
		// 503 to try again, as a waiting /item's, not a failure.
		err = errors.Join(library.ErrStopping, err)
	}
	return out, err
}

// writeRescanError answers a re-read's error: a path with no book (gone, or
// ignored) a 404 with code not_indexable, anything else the catalog error's
// answer.
func (a *API) writeRescanError(w http.ResponseWriter, lib *catalog.Library, p string, err error) {
	if errors.Is(err, library.ErrNotIndexable) || errors.Is(err, library.ErrOutsideRoot) {
		writeErrorCode(w, http.StatusNotFound, codeNotIndexable, "there is no book at that path any more")
		return
	}
	a.writeCatalogError(w, err, "rescan failed", "could not rescan the book", "library", lib.ID, "path", p)
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
// running one (it stops before its prune, so nothing is removed; a prune already
// under way finishes first).
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
	libID, ok := parseOptionalID(q.Get("library_id"))
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
