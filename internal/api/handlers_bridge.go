package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/names"
	"github.com/kodestar/audiosilo-server/internal/notify"
	"github.com/kodestar/audiosilo-server/internal/podcast"
)

// The Bridge (BRIDGE-PLAN.md Phase 0): the optional companion that writes to the
// libraries and runs jobs. The server never connects to it. An admin makes a
// pairing code (POST /admin/bridge/pair), the Bridge trades it for a bridge
// token (POST /bridge/pair) and from then on calls in: a heartbeat (PUT
// /bridge/status), a long-poll claim (GET /bridge/jobs/next), progress (PATCH
// /bridge/jobs/{id}), a book's files for its mapping check (GET
// /bridge/libraries/{id}/book) and a targeted rescan (POST /bridge/rescan). The
// bridge token works on those routes only (requireBridge); admins queue and
// cancel jobs from the console (/admin/bridge/*).

// SetBridge gives the API the Bridge's status and job queue, has admins told
// about each job that finishes, fails or needs attention, whatever moved it
// (the Bridge's update, a lease that ran out), and holds the queued jobs of a
// managed module while it is off (bridge.Service.ModuleOn). Call before
// Handler(); the launcher always does.
func (a *API) SetBridge(b *bridge.Service) {
	a.bridge = b
	a.cat.SetPodcastChanged(b.NotifyPodcast)
	a.prefetch = &podcast.Prefetcher{Catalog: a.cat, Bridge: b, Context: a.baseCtx}
	a.podcastDemand = &podcast.Demand{Catalog: a.cat, Bridge: b}
	if err := b.ExpirePrivateRequests(context.Background()); err != nil {
		a.log.Warn("expire private podcast requests failed", "err", err)
	}
	b.OnTransition = func(ctx context.Context, job bridge.Job, _ string) { a.notifyBridgeJob(ctx, &job) }
	b.OnEnded = func(ctx context.Context, job bridge.Job) {
		if bridge.IsPodcast(job.Kind) {
			if err := a.cat.FoldPodcastJob(ctx, job); err != nil {
				a.log.Warn("fold podcast outcome failed", "job", job.ID, "err", err)
			}
		}
	}
	b.ModuleOn = a.moduleOn
}

// bridgeKinds: the Bridge's own routes take a bridge token and nothing else, and
// no other route lists that kind, so a bridge token is refused everywhere else.
var bridgeKinds = []string{auth.KindBridge}

// requireBridge authenticates the paired Bridge's token (Authorization header
// only). Only a missing or refused token is a 401 (the Bridge then stops calling
// in and retries every few minutes), so a failure to check the token (the
// database) is a 503, which it retries at once.
func (a *API) requireBridge(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, failure, err := a.authenticate(r, false, bridgeKinds)
		switch {
		case failure == "":
			next(w, r.WithContext(ctx))
		case err != nil && !errors.Is(err, auth.ErrInvalidToken):
			a.log.Warn("bridge token check failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "could not check the token, try again")
		default:
			writeError(w, http.StatusUnauthorized, failure)
		}
	})
}

// bridgeInfo is the bridge block of GET /server, present only while the Bridge
// is online (its heartbeat under bridge.OnlineWindow old).
type bridgeInfo struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// bridgeOnline returns the bridge block for GET /server, or nil when no Bridge is
// online (or its status can't be read, which is logged, or the API was built
// without SetBridge: GET /server is every client's first call and must answer).
func (a *API) bridgeOnline(ctx context.Context) *bridgeInfo {
	if a.bridge == nil {
		return nil
	}
	h, err := a.bridge.Online(ctx)
	if err != nil {
		a.log.Warn("bridge status read failed", "err", err)
		return nil
	}
	if h == nil {
		return nil
	}
	return &bridgeInfo{Version: h.Version, Features: h.Features()}
}

// handleBridgeExchange serves POST /bridge/pair {code}: trade a pairing code for
// the bridge token (201 {token, server_id, server_name}). Public, and limited
// like redeeming an auth code; a wrong or expired code is 401.
func (a *API) handleBridgeExchange(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !allowAttempt(w, a.redeemLimiter, ip) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &req, 4<<10); err != nil || req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	token, err := a.auth.ExchangeBridgeCode(r.Context(), req.Code)
	if errors.Is(err, auth.ErrInvalidCode) {
		a.redeemLimiter.Fail(ip)
		writeError(w, http.StatusUnauthorized, "invalid or expired pairing code")
		return
	}
	if err != nil {
		a.log.Warn("bridge pairing failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not pair the bridge")
		return
	}
	a.redeemLimiter.Reset(ip)
	a.bridge.Forget()
	// No admin made this request: the pairing is recorded as the server's own.
	e := catalog.AuditEvent{Action: "bridge.paired", Via: catalog.ViaSystem} // the audit log keeps no addresses
	if err := a.cat.RecordAudit(context.WithoutCancel(r.Context()), e); err != nil {
		a.log.Warn("audit: record failed", "action", e.Action, "err", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"token": token, "server_id": a.config().ServerID, "server_name": a.config().DisplayName(),
	})
}

// bridgeLibrary is a server library as the Bridge maps it.
type bridgeLibrary struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

// libraryFor loads library id for a Bridge route, answering 404 (or the catalog
// error) itself when it can't.
func (a *API) libraryFor(w http.ResponseWriter, r *http.Request, id int64, op string) (*catalog.Library, bool) {
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return nil, false
	}
	if err != nil {
		a.writeCatalogError(w, err, op, "could not load the library")
		return nil, false
	}
	return lib, true
}

// adminBridgeBook resolves the book a Bridge admin route names: the library (404
// for an unknown one) and the indexed book at path in it (404 book_not_found,
// which the console words). It answers the error itself and reports whether
// there was none.
func (a *API) adminBridgeBook(w http.ResponseWriter, r *http.Request, libraryID int64, path, op string) (*catalog.Library, *catalog.Book, bool) {
	lib, ok := a.libraryFor(w, r, libraryID, op)
	if !ok {
		return nil, nil, false
	}
	if lib.Kind == catalog.KindPodcasts {
		writeErrorCode(w, 409, "not_book_library", "This action needs a books library.")
		return nil, nil, false
	}
	book, err := a.cat.GetBookByPath(r.Context(), lib.ID, path)
	if err != nil {
		a.writeBookError(w, err, op, "could not load the book")
		return nil, nil, false
	}
	return lib, book, true
}

// handleBridgeStatus serves PUT /bridge/status: the Bridge's heartbeat (its
// version, the job kinds it runs, its library mappings, workers and LAN page,
// its estimates and, since Phase 2b, its setup checklist). It answers with the
// server's libraries, so the Bridge maps any it hasn't yet, and accepts: the
// optional top-level blocks this server reads (bridge.HeartbeatAccepts), which
// the Bridge sends only after a reply lists them, since an older server refuses
// a heartbeat with a field it doesn't know.
func (a *API) handleBridgeStatus(w http.ResponseWriter, r *http.Request) {
	var st bridge.Status
	if err := decodeJSON(r, &st, bridge.MaxStatusBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	if err := st.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.bridge.SaveHeartbeat(r.Context(), credentialFrom(r.Context()).ID, st); err != nil {
		if errors.Is(err, bridge.ErrNotPaired) {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		a.log.Warn("bridge heartbeat failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save the status")
		return
	}
	libs, err := a.cat.ListLibraries(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "bridge heartbeat failed", "could not list libraries")
		return
	}
	out := make([]bridgeLibrary, 0, len(libs))
	for _, l := range libs {
		out = append(out, bridgeLibrary{Kind: l.Kind, ID: l.ID, Name: l.Name, Root: l.Root})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server_time": time.Now().UTC().Format(time.RFC3339),
		"libraries":   out,
		"accepts":     bridge.HeartbeatAccepts(),
	})
}

// bridgeFile is one of a book's files as the server indexed it, with the size
// and modification time the server sees now, for the Bridge's mapping check,
// and its indexed length in seconds (ffprobe's; a single-file book's is the
// book's), which a read-along map's whole-book times are built from (Phase 3c;
// an older Bridge ignores it).
type bridgeFile struct {
	Path     string  `json:"path"`
	Size     int64   `json:"size"`
	ModTime  int64   `json:"mod_time"`
	Duration float64 `json:"duration"`
}

// handleBridgeBook serves GET /bridge/libraries/{id}/book?path=: one indexed
// book's files with their sizes and modification times (Unix seconds, read now),
// for the Bridge's mapping check and its jobs. Without a path it answers the
// library's first indexed book, the mapping check's sample. Only an indexed book
// answers (never an on-demand index); 404 code no_book when there is none.
func (a *API) handleBridgeBook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "invalid library id")
	if !ok {
		return
	}
	lib, ok := a.libraryFor(w, r, id, "bridge book failed")
	if !ok {
		return
	}
	var (
		book  *catalog.Book
		files []bridgeFile
		err   error
	)
	p := catalog.CleanRelPath(r.URL.Query().Get("path"))
	if p != "" {
		if book, err = a.cat.GetBookByPath(r.Context(), lib.ID, p); err == nil {
			files, _ = statBookFiles(lib, book)
		}
	} else {
		book, files, err = a.sampleBook(r.Context(), lib)
	}
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "no_book", "no indexed book there")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "bridge book failed", "could not load the book")
		return
	}
	out := map[string]any{"library_id": lib.ID, "path": book.RelPath, "files": files}
	if p != "" {
		// What a job about the book reads (a path was asked for: never the
		// mapping check's sample).
		out["book"] = a.bridgeBookFacts(r.Context(), book)
	}
	writeJSON(w, http.StatusOK, out)
}

// bridgeBook is a book's facts for a Bridge job (GET /bridge/libraries/{id}/book
// with a path): the effective values, its credits split as the server reads
// them (names.Split), its series position as stored ("" for none), its
// community work id when the metadata cache knows it ("" otherwise) and its
// ebook.
type bridgeBook struct {
	Title       string   `json:"title"`
	Authors     []string `json:"authors"`
	Narrators   []string `json:"narrators"`
	Series      string   `json:"series"`
	SeriesIndex string   `json:"series_index"`
	ASIN        string   `json:"asin"`
	ISBN        string   `json:"isbn"`
	Duration    float64  `json:"duration"`
	WorkID      string   `json:"work_id"`
	// Ebook is the book's ebook as the server indexed it (Book.ebook: the
	// reader's, the one a readalong.align job aligns), omitted when it has
	// none (Phase 3c; an older Bridge ignores it).
	Ebook *catalog.Ebook `json:"ebook,omitempty"`
}

// bridgeBookFacts is book's bridgeBook. The work id comes from the metadata
// cache alone (meta.Service.CachedWorkID), never an upstream call: the Bridge
// asks this for every job, and a book nobody has looked up yet simply has none.
func (a *API) bridgeBookFacts(ctx context.Context, book *catalog.Book) bridgeBook {
	out := bridgeBook{
		Title: book.Title, Authors: orEmpty(names.Split(book.Author)), Narrators: orEmpty(names.Split(book.Narrator)),
		Series: book.Series, SeriesIndex: catalog.FormatSeriesRange(book.SeriesIndex, book.SeriesIndexEnd),
		ASIN: book.ASIN, ISBN: book.ISBN, Duration: book.Duration, Ebook: book.Ebook,
	}
	if a.metadataOn() {
		out.WorkID, _ = a.meta.CachedWorkID(ctx, book.ASIN, book.ISBN)
	}
	return out
}

// orEmpty is ss, or an empty list for none (the wire's lists are never null).
func orEmpty(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// statBookFiles is a book's files with the size and modification time the
// server sees now (the indexed size when a file can't be read), and whether
// every one is there.
func statBookFiles(lib *catalog.Library, book *catalog.Book) ([]bridgeFile, bool) {
	indexed := book.Files
	if len(indexed) == 0 {
		// A single-file book (a loose file at the root, a folder of one book per
		// file) has no book_files rows: the book is its own file.
		indexed = []catalog.BookFile{{RelPath: book.RelPath, Size: book.Size, Duration: book.Duration}}
	}
	files := make([]bridgeFile, 0, len(indexed))
	present := true
	for _, f := range indexed {
		bf := bridgeFile{Path: f.RelPath, Size: f.Size, Duration: f.Duration}
		info, err := os.Stat(filepathOf(lib, f.RelPath))
		if err == nil {
			bf.Size, bf.ModTime = info.Size(), info.ModTime().Unix()
		} else {
			present = false
		}
		files = append(files, bf)
	}
	return files, present
}

// filepathOf is a library-relative path on the server's disk ("" when it would
// leave the library, which then fails its stat).
func filepathOf(lib *catalog.Library, rel string) string {
	full, err := library.SafeJoin(lib.Root, rel)
	if err != nil {
		return ""
	}
	return full
}

// sampleBooks is how many books the mapping check's sample is chosen from.
const sampleBooks = 5

// sampleBook is the mapping check's sample: the first of the library's first
// books (by title) whose files are all on the server's disk, so one book deleted
// before a scan prunes it doesn't make the whole library look unmapped; the
// first book when none is whole.
func (a *API) sampleBook(ctx context.Context, lib *catalog.Library) (*catalog.Book, []bridgeFile, error) {
	if lib.Kind == catalog.KindPodcasts {
		return nil, nil, catalog.ErrNotFound
	}
	page, err := a.cat.ListBooks(ctx, catalog.ListOptions{LibraryID: lib.ID, Sort: "title", Limit: sampleBooks})
	if err != nil {
		return nil, nil, err
	}
	var (
		first      *catalog.Book
		firstFiles []bridgeFile
	)
	for _, b := range page.Books {
		book, err := a.cat.GetBookByPath(ctx, lib.ID, b.RelPath)
		if errors.Is(err, catalog.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		files, present := statBookFiles(lib, book)
		if present {
			return book, files, nil
		}
		if first == nil {
			first, firstFiles = book, files
		}
	}
	if first == nil {
		return nil, nil, catalog.ErrNotFound
	}
	return first, firstFiles, nil
}

// handleBridgeClaim serves GET /bridge/jobs/next?wait=<seconds>&kind=...: the
// next queued job of a kind the Bridge runs, narrowed to the kind= values when it
// sends any (the kinds it has room for now), leased to it (200 job), waiting up
// to wait (at most bridge.MaxWait) for one; 204 when none came (at once when no
// kind is left, and when the server is stopping), 409 no_heartbeat before its
// first heartbeat, 401 once a pairing or an unpair revoked its token mid-wait.
func (a *API) handleBridgeClaim(w http.ResponseWriter, r *http.Request) {
	wait := time.Duration(queryInt(r, "wait", 0)) * time.Second
	// The wait ends with the server too: a paired Bridge always has a claim
	// waiting, which would otherwise hold a graceful shutdown for up to its whole
	// wait (Shutdown never cancels a request's context).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(a.baseCtx, cancel)()
	var floor *int32
	if values, exists := r.URL.Query()["min_priority"]; exists {
		if len(values) != 1 {
			writeErrorCode(w, 400, "invalid_request", "Invalid minimum priority.")
			return
		}
		n, err := strconv.ParseInt(values[0], 10, 32)
		if err != nil {
			writeErrorCode(w, 400, "invalid_request", "Invalid minimum priority.")
			return
		}
		v := int32(n)
		floor = &v
	}
	job, err := a.bridge.ClaimForPriority(ctx, credentialFrom(r.Context()).ID, r.URL.Query()["kind"], wait, floor)
	switch {
	case errors.Is(err, bridge.ErrNoHeartbeat):
		writeErrorCode(w, http.StatusConflict, "no_heartbeat", "send a status first")
	case errors.Is(err, bridge.ErrNotPaired):
		writeError(w, http.StatusUnauthorized, "invalid or expired token")
	case err != nil && ctx.Err() != nil:
		// The server is stopping, the Bridge left or the deadline passed:
		// nothing was claimed.
		claimEnded(ctx, w)
	case err != nil:
		a.log.Warn("bridge claim failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not claim a job")
	case job == nil:
		claimEnded(ctx, w)
	default:
		// Claim cleanup outlives a disconnected request, but is bounded so a
		// database fault cannot hold this handler indefinitely.
		delivered := false
		defer func() {
			if delivered {
				return
			}
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			if err := a.bridge.ReleaseUnsentClaim(cleanup, job); err != nil {
				a.log.Warn("bridge claim release failed", "job_id", job.ID, "err", err)
			}
		}()
		if ctx.Err() != nil {
			claimEnded(ctx, w)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		writeErr := json.NewEncoder(w).Encode(job)
		if writeErr == nil {
			if err := http.NewResponseController(w).Flush(); !errors.Is(err, http.ErrNotSupported) {
				writeErr = err
			}
		}
		// The reply's last chunk follows this return, so a Bridge that left
		// meanwhile never read the job. A flushed reply is still delivered when
		// the server starts stopping or the deadline passes after the flush:
		// releasing it then would hand the same attempt out twice.
		delivered = writeErr == nil && !errors.Is(r.Context().Err(), context.Canceled)
	}
}

// claimEnded answers a claim whose wait ended early: 503 at the request
// deadline (as the timeout middleware answered before claims bypassed its
// buffering), else 204 (the server is stopping or the Bridge left).
func claimEnded(ctx context.Context, w http.ResponseWriter) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "request timed out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBridgeUpdate serves PATCH /bridge/jobs/{id} {attempt, state?, progress?,
// result?, error?}: the Bridge's report on a job it holds, renewing its lease.
// The answer is the job as stored; its cancel_requested asks the Bridge to stop.
// 409 code not_held when the claim no longer holds it (it ended, was cancelled,
// or its lease ran out and it was claimed again).
func (a *API) handleBridgeUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "invalid job id")
	if !ok {
		return
	}
	var u bridge.Update
	if err := decodeJSON(r, &u, 128<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid update")
		return
	}
	job, err := a.bridge.UpdateHeld(r.Context(), id, u)
	if !a.writeBridgeError(w, err, "bridge job update failed") {
		return
	}
	writeJSON(w, http.StatusOK, bridge.RedactedJob(*job))
}

// notifyBridgeJob reports a job that just finished, failed or parked
// (notify.BridgeJobEnded), naming its book: the title as indexed now, else its
// path. Best effort: a book or library that can't be read is reported by path.
func (a *API) notifyBridgeJob(ctx context.Context, job *bridge.Job) {
	ev := notify.BridgeJob{ID: job.ID, Kind: job.Kind, State: job.State, Path: job.Target(),
		Code: job.ParkCode(), Error: job.Error, SharedNothing: job.SharedNothing()}
	if c, ok := job.ReadalongCoverage(); ok {
		ev.Coverage = &c
	}
	if job.LibraryID != nil {
		ev.LibraryID = *job.LibraryID
		if lib, err := a.cat.GetLibrary(ctx, ev.LibraryID); err == nil {
			ev.Library = lib.Name
		}
		if ev.Path != "" {
			if book, err := a.cat.GetBookByPath(ctx, ev.LibraryID, ev.Path); err == nil {
				ev.Title = book.Title
				// Whether the chapters a detect job found are the ones listeners
				// get (the Bridge rescans before it reports done).
				ev.ChaptersSource = cmp.Or(book.ChaptersSource, catalog.ChaptersFromFiles)
			}
		}
	}
	a.rt.Notify.BridgeJobEnded(ctx, ev)
}

// writeBridgeError maps a bridge error (400 invalid, 404 no job, 409 not held,
// finished, a kind the Bridge doesn't run, a library it can't write to, a
// contribution it can't make, an open job of another kind that refuses it
// (generate_open, detect_open, describe_open, contribute_open, readalong_open), a detect for a book whose
// chapter source an admin chose, else 500) and reports whether there was none.
func (a *API) writeBridgeError(w http.ResponseWriter, err error, op string) bool {
	switch {
	case err == nil:
		return true
	case writeLibraryKindError(w, err):
	case errors.Is(err, bridge.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, bridge.ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found")
	case errors.Is(err, bridge.ErrNotHeld):
		writeErrorCode(w, http.StatusConflict, "not_held", "the job is no longer held by this claim")
	case errors.Is(err, bridge.ErrFinished):
		writeErrorCode(w, http.StatusConflict, "finished", "the job has already finished")
	case errors.Is(err, bridge.ErrKindUnavailable):
		writeErrorCode(w, http.StatusConflict, "kind_unavailable", "the paired bridge doesn't run that kind of job")
	case errors.Is(err, bridge.ErrLibraryNotWritable):
		writeErrorCode(w, http.StatusConflict, "library_not_writable", "the bridge can't write to that library")
	case errors.Is(err, bridge.ErrCannotContribute):
		writeErrorCode(w, http.StatusConflict, "cannot_contribute", "the bridge can't contribute to the community")
	case errors.Is(err, bridge.ErrGenerateOpen):
		writeErrorCode(w, http.StatusConflict, "generate_open",
			"a generate job is still open for this book; queue this once it has finished")
	case errors.Is(err, bridge.ErrDetectOpen):
		writeErrorCode(w, http.StatusConflict, "detect_open",
			"a chapter detection job is still open for this book; queue this once it has finished")
	case errors.Is(err, bridge.ErrContributeOpen):
		writeErrorCode(w, http.StatusConflict, "contribute_open",
			"this book's characters and recaps are being shared with the community; generate them again once that has finished")
	case errors.Is(err, bridge.ErrDescribeOpen):
		writeErrorCode(w, http.StatusConflict, "describe_open",
			"a description job is still open for this book; detect its chapters once it has finished")
	case errors.Is(err, bridge.ErrReadalongOpen):
		writeErrorCode(w, http.StatusConflict, "readalong_open",
			"a read-along job is still open for this book; queue this once it has finished")
	case errors.Is(err, bridge.ErrChaptersChosen):
		writeErrorCode(w, http.StatusConflict, "chapters_chosen",
			"an admin chose this book's chapter source, so chapters found in the narration would never be used; set it back to automatic first")
	default:
		a.log.Warn(op, "err", err)
		writeError(w, http.StatusInternalServerError, "could not update the job")
	}
	return false
}

// handleBridgeRescan serves POST /bridge/rescan {library_id, path, mode?, from?}:
// read one path again now, after the Bridge changed files there.
//   - mode "full" (or none): index what is at the path, a new book included
//     (re-reading the audio), and answer 200 {path} with the book's indexed path.
//     With from, the book's path before a move the Bridge made: its durable state
//     follows it as a scan's move detection would (Scanner.IndexMoved; a from that
//     isn't that book moved is ignored).
//   - mode "attachments": read only the book's .audiosilo/ attachments, never its
//     audio (Scanner.RefreshAttachments: a changed chapters.json re-resolves
//     the book's chapters), and answer 200 {path, attachments: {characters,
//     recaps, description, chapters, bridge}} with what is now indexed. from
//     is refused here.
//
// 404 code not_indexable when no book is there (for attachments: no indexed one).
func (a *API) handleBridgeRescan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID int64   `json:"library_id"`
		Path      string  `json:"path"`
		Mode      string  `json:"mode"`
		From      *string `json:"from"`
	}
	if err := decodeJSON(r, &req, 8<<10); err != nil || req.LibraryID <= 0 || catalog.CleanRelPath(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "library_id and path are required")
		return
	}
	rawFrom := ""
	if req.From != nil {
		rawFrom = *req.From
	}
	from := catalog.CleanRelPath(rawFrom)
	switch {
	case req.Mode != "" && req.Mode != rescanFull && req.Mode != rescanAttachments:
		writeError(w, http.StatusBadRequest, "mode must be full or attachments")
		return
	case rawFrom != "" && from == "":
		writeError(w, http.StatusBadRequest, "invalid from")
		return
	case from != "" && req.Mode == rescanAttachments:
		writeError(w, http.StatusBadRequest, "from is only for a full rescan")
		return
	}
	lib, ok := a.libraryFor(w, r, req.LibraryID, "bridge rescan failed")
	if !ok {
		return
	}
	p := catalog.CleanRelPath(req.Path)
	if lib.Kind == catalog.KindPodcasts {
		if req.Mode == rescanAttachments || req.From != nil {
			writeError(w, 400, "Podcast rescans take a show path only.")
			return
		}
		// Detached from the request like a book's rescan (rescanAt), so a long
		// show's index is still saved when the Bridge's request gives up.
		ctx, cancel := context.WithTimeout(a.baseCtx, rescanBookTimeout)
		defer cancel()
		show, n, err := a.scanner.IndexPodcastPath(ctx, *lib, p)
		if errors.Is(err, library.ErrNotIndexable) {
			writeErrorCode(w, 404, "not_indexable", "No valid feed.json.")
			return
		}
		if err != nil {
			a.writeCatalogError(w, err, "rescan podcast failed", "Could not index show.")
			return
		}
		a.bridge.WakePodcasts()
		writeJSON(w, 200, map[string]any{"path": show, "episodes": n})
		return
	}

	if req.Mode == rescanAttachments {
		got, ok := rescanAt(a, w, lib, p, func(ctx context.Context) (*library.AttachmentsRefresh, error) {
			return a.scanner.RefreshAttachments(ctx, *lib, p)
		})
		if ok {
			writeJSON(w, http.StatusOK, got)
		}
		return
	}
	book, ok := rescanAt(a, w, lib, p, func(ctx context.Context) (*catalog.Book, error) {
		return a.scanner.IndexMoved(ctx, *lib, p, from)
	})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": book.RelPath})
}

// The modes of POST /bridge/rescan.
const (
	rescanFull        = "full"
	rescanAttachments = "attachments"
)

// adminBridge is GET /admin/bridge: the pairing, the last heartbeat and whether
// it is recent enough to count as online, and every server library with the
// Bridge's mapping of it (nil where it sent none).
type adminBridge struct {
	Paired   bool           `json:"paired"`
	PairedAt string         `json:"paired_at,omitempty"`
	PairedBy *auth.UserRef  `json:"paired_by"`
	Pairing  *bridgePairing `json:"pairing"`
	Online   bool           `json:"online"`
	// HeartbeatAt is the last heartbeat (RFC3339), "" before the first.
	HeartbeatAt string               `json:"heartbeat_at,omitempty"`
	Version     string               `json:"version,omitempty"`
	Kinds       []string             `json:"kinds"`
	Features    []string             `json:"features"`
	Libraries   []adminBridgeLibrary `json:"libraries"`
	Workers     []bridge.Worker      `json:"workers"`
	UIURL       string               `json:"ui_url,omitempty"`
	// Estimates are the Bridge's costing figures (omitted when it sent none).
	Estimates *bridge.Estimates `json:"estimates,omitempty"`
	// Setup is the Bridge's onboarding checklist, with its GitHub login
	// (omitted when it sent none: an older Bridge).
	Setup *bridge.Setup `json:"setup,omitempty"`
}

// bridgePairing is a pairing code waiting to be exchanged.
type bridgePairing struct {
	ExpiresAt string `json:"expires_at"`
}

type adminBridgeLibrary struct {
	bridgeLibrary
	Mapping *bridge.LibraryStatus `json:"mapping"`
}

// handleAdminBridge serves GET /admin/bridge.
func (a *API) handleAdminBridge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := a.auth.GetBridgePairing(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "bridge status failed", "could not read the bridge")
		return
	}
	// LastHeartbeat is nil unless the Bridge is paired (the same rule as p.Paired).
	h, err := a.bridge.LastHeartbeat(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "bridge status failed", "could not read the bridge")
		return
	}
	libs, err := a.cat.ListLibraries(ctx)
	if err != nil {
		a.writeCatalogError(w, err, "bridge status failed", "could not list libraries")
		return
	}
	out := adminBridge{Paired: p.Paired, PairedAt: p.PairedAt, PairedBy: p.PairedBy,
		Kinds: []string{}, Features: []string{}, Workers: []bridge.Worker{}, Libraries: []adminBridgeLibrary{}}
	if p.CodeExpiresAt != "" {
		out.Pairing = &bridgePairing{ExpiresAt: p.CodeExpiresAt}
	}
	if h != nil {
		out.Online = h.Online(time.Now())
		out.HeartbeatAt = h.At.UTC().Format(time.RFC3339)
		out.Version, out.Kinds, out.Features, out.Workers, out.UIURL = h.Version, h.Kinds, h.Features(), h.Workers, h.UIURL
		out.Estimates, out.Setup = h.Estimates, h.Setup
	}
	for _, l := range libs {
		al := adminBridgeLibrary{bridgeLibrary: bridgeLibrary{Kind: l.Kind, ID: l.ID, Name: l.Name, Root: l.Root}}
		if h != nil {
			al.Mapping = h.Library(l.ID)
		}
		out.Libraries = append(out.Libraries, al)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminBridgePair serves POST /admin/bridge/pair: a pairing code for the
// Bridge to exchange (201 {code, expires_at, server_url}), shown once. It
// replaces a code still waiting; a paired Bridge keeps working until the new
// code is exchanged.
func (a *API) handleAdminBridgePair(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	u := userFrom(r.Context())
	code, expires, err := a.auth.IssueBridgePairCode(r.Context(), u.ID)
	if err != nil {
		a.log.Warn("bridge pairing code failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not make a pairing code")
		return
	}
	a.audit(r, "bridge.code", "", nil)
	// The address to give the Bridge: the home address when there is one (plain
	// http on the LAN for a self-signed server, whose certificate the Bridge can't
	// verify), else the address this request used.
	serverURL := a.baseURL(r)
	if home := a.addresses(r).Home; home != "" {
		serverURL = home
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"code": code, "expires_at": expires, "server_url": serverURL,
	})
}

// handleAdminBridgeUnpair serves DELETE /admin/bridge: revoke the Bridge's token
// and forget it (204). Its jobs stay; queued ones wait for the next Bridge.
func (a *API) handleAdminBridgeUnpair(w http.ResponseWriter, r *http.Request) {
	if err := a.auth.UnpairBridge(r.Context()); err != nil {
		a.log.Warn("bridge unpair failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not unpair the bridge")
		return
	}
	a.bridge.Forget()
	a.audit(r, "bridge.unpair", "", nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminBridgeQueue serves POST /admin/bridge/jobs {kind, library_id?,
// path?, params?}: queue a job (201) or return the open one already queued for
// that kind, library and path (200). The kind must be one the server knows (400)
// and the paired Bridge runs (409 code kind_unavailable; it may be offline: the
// job waits), and a kind that writes (sidecars.generate, sidecars.describe)
// needs a library the Bridge last reported mapped and writable (409 code
// library_not_writable); a job is refused while an open job of another kind
// refuses it (bridge.Enqueue: 409 code generate_open, detect_open,
// describe_open, contribute_open or readalong_open), a chapters.detect job for
// a book whose chapter source an admin chose (409 code chapters_chosen: the
// list it finds would never be used), and a readalong.align job for a book the
// index holds no ebook for (409 code no_ebook). A job that files to the community
// (sidecars.contribute, meta.correct, sidecars.generate with contribute
// "issue") is 404 code module_off while the contributions module is off, and
// 409 code cannot_contribute unless the Bridge says it can; a meta.correct
// job's changes must be the book's own edits (400 code not_edited,
// checkCorrections). A path names an indexed book (404 otherwise) and the job
// is about that book's own path.
func (a *API) handleAdminBridgeQueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind      string          `json:"kind"`
		LibraryID *int64          `json:"library_id"`
		Path      *string         `json:"path"`
		Params    json.RawMessage `json:"params"`
	}
	// The params bound is the kind's, on the params as sent (bridge.Enqueue;
	// for meta.correct bridge.CheckCorrect, as checkCorrections re-encodes them
	// first); the body holds them and a little more.
	if err := decodeJSON(r, &req, bridge.MaxParamsBytes+4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if bridge.Internal(req.Kind) {
		writeError(w, 400, "This job kind is server-internal.")
		return
	}

	// A job that needs a managed module (contributions: filing to the
	// community) is unknown while the module is off, as the module's routes are.
	if m := bridge.ModuleFor(req.Kind, req.Params); m != "" && !a.moduleOn(m) {
		writeModuleOff(w)
		return
	}
	nj := bridge.NewJob{Kind: req.Kind, LibraryID: req.LibraryID, Params: req.Params, RequestedBy: userFrom(r.Context()).ID}
	if req.Path != nil {
		if p := catalog.CleanRelPath(*req.Path); p != "" {
			nj.Path = &p
		}
	}
	switch {
	case nj.LibraryID != nil && nj.Path != nil:
		_, book, ok := a.adminBridgeBook(w, r, *nj.LibraryID, *nj.Path, "bridge queue failed")
		if !ok {
			return
		}
		nj.Path = &book.RelPath
		if nj.Kind == bridge.KindCorrect {
			params, ok := a.checkCorrections(w, r, book, nj.Params)
			if !ok {
				return
			}
			nj.Params = params
		}
		if nj.Kind == bridge.KindReadalong && book.Ebook == nil {
			writeErrorCode(w, http.StatusConflict, "no_ebook", "this book has no ebook to align with its narration")
			return
		}
		if nj.Kind == bridge.KindDetectChapters {
			choice, err := a.cat.ChapterChoice(r.Context(), book.LibraryID, book.RelPath)
			if err != nil {
				a.writeCatalogError(w, err, "bridge queue failed", "could not read the book's chapter choice")
				return
			}
			if choice != "" {
				a.writeBridgeError(w, bridge.ErrChaptersChosen, "bridge queue failed")
				return
			}
		}
	case nj.LibraryID != nil:
		if _, ok := a.libraryFor(w, r, *nj.LibraryID, "bridge queue failed"); !ok {
			return
		}
	}
	job, created, err := a.bridge.Enqueue(r.Context(), nj)
	if !a.writeBridgeError(w, err, "bridge queue failed") {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		a.audit(r, "bridge.job", job.Target(), map[string]any{"job": job.ID, "kind": job.Kind, "library": job.LibraryID})
	}
	writeJSON(w, status, job)
}

// codeNotEdited is a meta.correct change of a field the book doesn't hold an
// admin's edit of, or whose corrected value isn't the book's value now.
const codeNotEdited = "not_edited"

// checkCorrections checks a meta.correct job's params against the book
// (bridge.CheckCorrect first, 400 for what it refuses): each change must be a
// field an admin edited on this book with the corrected value the book's edited
// value now (catalog.MatchState.EditedValue), else 400 not_edited naming the
// field.
// It returns the params with each corrected value as the book holds it and
// source stamped as this server (audiosilo-server/<version>), whatever the
// request said; it answers the error itself and reports whether there was none.
func (a *API) checkCorrections(w http.ResponseWriter, r *http.Request, book *catalog.Book, raw json.RawMessage) (json.RawMessage, bool) {
	p, err := bridge.CheckCorrect(raw)
	if err != nil {
		a.writeBridgeError(w, err, "bridge queue failed")
		return nil, false
	}
	st, err := a.cat.BookMatchState(r.Context(), book.LibraryID, book.RelPath)
	if err != nil {
		a.writeBookError(w, err, "bridge queue failed", "could not load the book")
		return nil, false
	}
	for i := range p.Changes {
		c := &p.Changes[i]
		v, ok := st.EditedValue(c.Field, c.Corrected)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": codeNotEdited, "field": c.Field,
				"error": c.Field + " isn't an edit made on this server, or the book's value changed since"})
			return nil, false
		}
		c.Corrected = v
	}
	p.Source = "audiosilo-server/" + Version
	out, err := json.Marshal(p)
	if err != nil {
		a.writeBridgeError(w, err, "bridge queue failed")
		return nil, false
	}
	return out, true
}

// handleAdminBridgeJobs serves GET /admin/bridge/jobs?library_id=&path=&state=
// &before=&limit=: jobs newest first (limit <= 200, default 50), with
// next_before for the next page.
func (a *API) handleAdminBridgeJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := bridge.Filter{Kind: q.Get("kind"), State: q.Get("state"), Limit: queryInt(r, "limit", bridge.DefaultListLimit)}
	libraryID, ok := parseOptionalID(q.Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	if libraryID > 0 {
		f.LibraryID = &libraryID
	}
	if f.Before, ok = parseOptionalID(q.Get("before")); !ok {
		writeError(w, http.StatusBadRequest, "invalid before")
		return
	}
	if q.Has("path") {
		p := catalog.CleanRelPath(q.Get("path"))
		f.Path = &p
	}
	jobs, next, err := a.bridge.List(r.Context(), f)
	if err != nil {
		a.log.Warn("bridge jobs failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list jobs")
		return
	}
	out := map[string]any{"jobs": jobs}
	if next > 0 {
		out["next_before"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminBridgeJob serves GET /admin/bridge/jobs/{id}.
func (a *API) handleAdminBridgeJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "invalid job id")
	if !ok {
		return
	}
	job, err := a.bridge.Job(r.Context(), id)
	if !a.writeBridgeError(w, err, "bridge job failed") {
		return
	}
	writeJSON(w, http.StatusOK, bridge.RedactedJob(*job))
}

// handleAdminBridgeCancel serves POST /admin/bridge/jobs/{id}/cancel: cancel a
// queued or parked job at once, or ask the Bridge to stop a running one (200 the
// job; 409 code finished when it already ended).
func (a *API) handleAdminBridgeCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "invalid job id")
	if !ok {
		return
	}
	job, err := a.bridge.Cancel(r.Context(), id)
	if !a.writeBridgeError(w, err, "bridge job cancel failed") {
		return
	}
	a.audit(r, "bridge.cancel", job.Target(), map[string]any{"job": job.ID, "kind": job.Kind})
	writeJSON(w, http.StatusOK, bridge.RedactedJob(*job))
}
