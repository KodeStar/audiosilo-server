package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
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

// SetBridge gives the API the Bridge's status and job queue. Call before
// Handler(); the launcher always does.
func (a *API) SetBridge(b *bridge.Service) { a.bridge = b }

// bridgeKinds: the Bridge's own routes take a bridge token and nothing else, and
// no other route lists that kind, so a bridge token is refused everywhere else.
var bridgeKinds = []string{auth.KindBridge}

// requireBridge authenticates the paired Bridge's token (Authorization header
// only).
func (a *API) requireBridge(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, failure := a.authenticate(r, false, bridgeKinds)
		if failure != "" {
			writeError(w, http.StatusUnauthorized, failure)
			return
		}
		next(w, r.WithContext(ctx))
	})
}

// bridgeInfo is the bridge block of GET /server, present only while the Bridge
// is online (its heartbeat under bridge.OnlineWindow old).
type bridgeInfo struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// bridgeOnline returns the bridge block for GET /server, or nil when no Bridge is
// online (or its status can't be read, which is logged).
func (a *API) bridgeOnline(ctx context.Context) *bridgeInfo {
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
	if !allowAttempt(w, a.redeemLimiter.Allowed(ip)) {
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
	e := catalog.AuditEvent{Action: "bridge.paired", Details: map[string]any{"ip": ip}, Via: catalog.ViaSystem}
	if err := a.cat.RecordAudit(context.WithoutCancel(r.Context()), e); err != nil {
		a.log.Warn("audit: record failed", "action", e.Action, "err", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"token": token, "server_id": a.config().ServerID, "server_name": a.config().DisplayName(),
	})
}

// bridgeLibrary is a server library as the Bridge maps it.
type bridgeLibrary struct {
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

// handleBridgeStatus serves PUT /bridge/status: the Bridge's heartbeat (its
// version, the job kinds it runs, its library mappings, workers and LAN page).
// It answers with the server's libraries, so the Bridge maps any it hasn't yet.
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
	if err := a.bridge.SaveHeartbeat(r.Context(), st); err != nil {
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
		out = append(out, bridgeLibrary{ID: l.ID, Name: l.Name, Root: l.Root})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server_time": time.Now().UTC().Format(time.RFC3339),
		"libraries":   out,
	})
}

// bridgeFile is one of a book's files as the server indexed it, with the size
// and modification time the server sees now, for the Bridge's mapping check.
type bridgeFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
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
		book *catalog.Book
		err  error
	)
	if p := catalog.CleanRelPath(r.URL.Query().Get("path")); p != "" {
		book, err = a.cat.GetBookByPath(r.Context(), lib.ID, p)
	} else {
		book, err = a.firstBook(r.Context(), lib.ID)
	}
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "no_book", "no indexed book there")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "bridge book failed", "could not load the book")
		return
	}
	files := make([]bridgeFile, 0, len(book.Files))
	for _, f := range book.Files {
		bf := bridgeFile{Path: f.RelPath, Size: f.Size}
		if full, err := library.SafeJoin(lib.Root, f.RelPath); err == nil {
			if info, err := os.Stat(full); err == nil {
				bf.Size, bf.ModTime = info.Size(), info.ModTime().Unix()
			}
		}
		files = append(files, bf)
	}
	writeJSON(w, http.StatusOK, map[string]any{"library_id": lib.ID, "path": book.RelPath, "files": files})
}

// firstBook is a library's first indexed book with its files, the mapping
// check's sample.
func (a *API) firstBook(ctx context.Context, libraryID int64) (*catalog.Book, error) {
	page, err := a.cat.ListBooks(ctx, catalog.ListOptions{LibraryID: libraryID, Sort: "title", Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(page.Books) == 0 {
		return nil, catalog.ErrNotFound
	}
	return a.cat.GetBookByPath(ctx, libraryID, page.Books[0].RelPath)
}

// handleBridgeClaim serves GET /bridge/jobs/next?wait=<seconds>&kind=...: the
// next queued job of a kind the Bridge runs, narrowed to the kind= values when it
// sends any (the kinds it has room for now), leased to it (200 job), waiting up
// to wait (at most bridge.MaxWait) for one; 204 when none came (at once when no
// kind is left), 409 no_heartbeat before its first heartbeat.
func (a *API) handleBridgeClaim(w http.ResponseWriter, r *http.Request) {
	wait := time.Duration(queryInt(r, "wait", 0)) * time.Second
	job, err := a.bridge.ClaimFor(r.Context(), r.URL.Query()["kind"], wait)
	switch {
	case errors.Is(err, bridge.ErrNoHeartbeat):
		writeErrorCode(w, http.StatusConflict, "no_heartbeat", "send a status first")
	case err != nil:
		a.log.Warn("bridge claim failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not claim a job")
	case job == nil:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, job)
	}
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
	writeJSON(w, http.StatusOK, job)
}

// writeBridgeError maps a bridge error (400 invalid, 404 no job, 409 not held,
// finished or a kind the Bridge doesn't run, else 500) and reports whether there
// was none.
func (a *API) writeBridgeError(w http.ResponseWriter, err error, op string) bool {
	switch {
	case err == nil:
		return true
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
	default:
		a.log.Warn(op, "err", err)
		writeError(w, http.StatusInternalServerError, "could not update the job")
	}
	return false
}

// handleBridgeRescan serves POST /bridge/rescan {library_id, path}: read one path
// again now, after the Bridge changed files there (a placed or renamed book).
// It indexes what is at the path, a new book included, and answers 200 {path}
// with the book's indexed path; 404 code not_indexable when nothing indexable is
// there. It re-reads the audio: Phase 1 adds a cheaper refresh for files the
// Bridge writes into a book's .audiosilo/.
func (a *API) handleBridgeRescan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID int64  `json:"library_id"`
		Path      string `json:"path"`
	}
	if err := decodeJSON(r, &req, 4<<10); err != nil || req.LibraryID <= 0 || catalog.CleanRelPath(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "library_id and path are required")
		return
	}
	lib, ok := a.libraryFor(w, r, req.LibraryID, "bridge rescan failed")
	if !ok {
		return
	}
	book, ok := a.rescanPath(w, lib, catalog.CleanRelPath(req.Path))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": book.RelPath})
}

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
	}
	for _, l := range libs {
		al := adminBridgeLibrary{bridgeLibrary: bridgeLibrary{ID: l.ID, Name: l.Name, Root: l.Root}}
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
	writeJSON(w, http.StatusCreated, map[string]string{
		"code": code, "expires_at": expires, "server_url": a.baseURL(r),
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
// job waits). A path names an indexed book (404 otherwise) and the job is about
// that book's own path.
func (a *API) handleAdminBridgeQueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind      string          `json:"kind"`
		LibraryID *int64          `json:"library_id"`
		Path      *string         `json:"path"`
		Params    json.RawMessage `json:"params"`
	}
	if err := decodeJSON(r, &req, 32<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	nj := bridge.NewJob{Kind: req.Kind, LibraryID: req.LibraryID, Params: req.Params, RequestedBy: userFrom(r.Context()).ID}
	if req.Path != nil {
		if p := catalog.CleanRelPath(*req.Path); p != "" {
			nj.Path = &p
		}
	}
	if nj.LibraryID != nil {
		lib, ok := a.libraryFor(w, r, *nj.LibraryID, "bridge queue failed")
		if !ok {
			return
		}
		if nj.Path != nil {
			book, err := a.cat.GetBookByPath(r.Context(), lib.ID, *nj.Path)
			if errors.Is(err, catalog.ErrNotFound) {
				writeError(w, http.StatusNotFound, "book not found")
				return
			}
			if err != nil {
				a.writeCatalogError(w, err, "bridge queue failed", "could not load the book")
				return
			}
			nj.Path = &book.RelPath
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

// handleAdminBridgeJobs serves GET /admin/bridge/jobs?library_id=&path=&state=
// &before=&limit=: jobs newest first (limit <= 200, default 50), with
// next_before for the next page.
func (a *API) handleAdminBridgeJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := bridge.Filter{State: q.Get("state"), Limit: queryInt(r, "limit", bridge.DefaultListLimit)}
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
	writeJSON(w, http.StatusOK, job)
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
	writeJSON(w, http.StatusOK, job)
}
