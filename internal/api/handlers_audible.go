package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/names"
)

// Bridge Phase 4a: Audible accounts (catalog/audible.go). The Bridge's routes
// (a bridge token, header only) record the accounts it signed in to and the
// titles each poll found, list the people an account may be mapped to and a
// library's books for its matching, and backfill an ASIN it matched without
// doubt. The console's routes (admin, behind the audible module's toggle,
// mounted in api.go) show them read-only and ask for a poll now.

// maxAudibleTitlesBody bounds a titles call: 500 titles at their bounds.
const maxAudibleTitlesBody = 4 << 20

// handleBridgeUsers serves GET /bridge/users: who an Audible account may be
// mapped to (enabled adults and admins; never a kid, demo or disabled account).
func (a *API) handleBridgeUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.cat.BridgeUsers(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "bridge users failed", "could not list the users")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// audibleAccountID is the {account} path value when it is an account id the
// Bridge could have made, else it answers 404 and reports false.
func audibleAccountID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("account")
	if !bridge.AudibleAccountPattern.MatchString(id) {
		writeErrorCode(w, http.StatusNotFound, "unknown_account", "no such Audible account")
		return "", false
	}
	return id, true
}

// positiveLimit is the ?limit= query value, 0 when absent; a value that is not
// a positive number answers 400 and reports false.
func positiveLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "limit must be a positive number")
		return 0, false
	}
	return n, true
}

// writeAudibleError answers an Audible catalog error.
func (a *API) writeAudibleError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, catalog.ErrAudibleInvalid):
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, catalog.ErrAudiblePolicy):
		writeErrorCode(w, http.StatusBadRequest, "invalid_policy", err.Error())
	case errors.Is(err, catalog.ErrAudibleMarketplace):
		writeErrorCode(w, http.StatusBadRequest, "invalid_marketplace", err.Error())
	case errors.Is(err, catalog.ErrNotBookLibrary):
		writeErrorCode(w, http.StatusBadRequest, "not_books", "not a books library")
	case errors.Is(err, catalog.ErrAudibleUnknownUser):
		writeErrorCode(w, http.StatusNotFound, "unknown_user", "no such user, or not one an account may be mapped to")
	case errors.Is(err, catalog.ErrUnknownLibrary):
		writeErrorCode(w, http.StatusNotFound, "unknown_library", "no such library")
	case errors.Is(err, catalog.ErrUnknownAccount):
		writeErrorCode(w, http.StatusNotFound, "unknown_account", "no such Audible account")
	case errors.Is(err, catalog.ErrASINSet):
		writeErrorCode(w, http.StatusConflict, "asin_set", "the book already has an ASIN")
	default:
		a.writeCatalogError(w, err, op, "could not update the Audible account")
	}
}

// handleBridgeAudibleAccount serves PUT /bridge/audible/accounts/{account}:
// the Bridge's mapping of an account it signed in to (a user, a books library,
// a policy). 200 {"account": {...}}.
func (a *API) handleBridgeAudibleAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := audibleAccountID(w, r)
	if !ok {
		return
	}
	var in catalog.AudibleAccountInput
	if err := decodeJSON(r, &in, 4096); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	acct, err := a.cat.PutAudibleAccount(r.Context(), id, in)
	if err != nil {
		a.writeAudibleError(w, err, "put audible account failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": acct})
}

// handleBridgeAudibleAccountDelete serves DELETE /bridge/audible/accounts/
// {account}: the Bridge removed the account (its titles go with it). 204,
// known or not.
func (a *API) handleBridgeAudibleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := audibleAccountID(w, r)
	if !ok {
		return
	}
	if err := a.cat.DeleteAudibleAccount(r.Context(), id); err != nil {
		a.writeAudibleError(w, err, "delete audible account failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// audibleTitlesRequest is a titles call: a page of one poll's titles.
type audibleTitlesRequest struct {
	PollID string                      `json:"poll_id"`
	Final  bool                        `json:"final"`
	Titles []catalog.AudibleTitleInput `json:"titles"`
}

// handleBridgeAudibleTitles serves PUT /bridge/audible/accounts/{account}/
// titles: up to 500 titles of one poll; on its final call the titles the poll
// didn't report become gone. {"stored": N}. A call without its titles array
// is 400, never "the account owns nothing" (an empty poll sends []).
func (a *API) handleBridgeAudibleTitles(w http.ResponseWriter, r *http.Request) {
	id, ok := audibleAccountID(w, r)
	if !ok {
		return
	}
	var req audibleTitlesRequest
	if err := decodeJSON(r, &req, maxAudibleTitlesBody); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if req.Titles == nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "titles is required")
		return
	}
	if err := a.cat.PutAudibleTitles(r.Context(), id, req.PollID, req.Final, req.Titles); err != nil {
		a.writeAudibleError(w, err, "put audible titles failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"stored": len(req.Titles)})
}

// bridgeListBook is one book of GET /bridge/libraries/{id}/books: what the
// Bridge's matcher reads.
type bridgeListBook struct {
	Path      string                  `json:"path"`
	Title     string                  `json:"title"`
	Authors   []string                `json:"authors"`
	Narrators []string                `json:"narrators"`
	Series    []catalog.AudibleSeries `json:"series"`
	ASIN      string                  `json:"asin"`
	ISBN      string                  `json:"isbn"`
	Duration  float64                 `json:"duration"`
}

// bridgeSeries is every series a book is in, the main one first, each with its
// position as stored ("3", "1-3", "" for none).
func bridgeSeries(b *catalog.Book) []catalog.AudibleSeries {
	list := b.AllSeries()
	out := make([]catalog.AudibleSeries, 0, len(list))
	for _, s := range list {
		out = append(out, catalog.AudibleSeries{Name: s.Name, Position: catalog.FormatSeriesRange(s.Position, s.PositionEnd)})
	}
	return out
}

// handleBridgeBooks serves GET /bridge/libraries/{id}/books?after=&limit=: a
// books library's indexed books by path, a page at a time (at most 500), for
// the Bridge's Audible matching. {"books": [...], "next": cursor | null}.
func (a *API) handleBridgeBooks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "unknown_library", "no such library")
		return
	}
	limit, ok := positiveLimit(w, r)
	if !ok {
		return
	}
	books, next, err := a.cat.BridgeBooks(r.Context(), id, r.URL.Query().Get("after"), limit)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "unknown_library", "no such library")
		return
	case err != nil:
		a.writeAudibleError(w, err, "bridge books failed")
		return
	}
	out := make([]bridgeListBook, 0, len(books))
	for i := range books {
		b := &books[i]
		out = append(out, bridgeListBook{
			Path: b.RelPath, Title: b.Title, Authors: orEmpty(names.Split(b.Author)),
			Narrators: orEmpty(names.Split(b.Narrator)), Series: bridgeSeries(b),
			ASIN: b.ASIN, ISBN: b.ISBN, Duration: b.Duration,
		})
	}
	var cursor *string
	if next != "" {
		cursor = &next
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"books": out, "next": cursor})
}

// handleBridgeEnrichment serves PUT /bridge/libraries/{id}/enrichment?path=
// {"asin": ".."}: the ASIN of an Audible title the Bridge matched to the book
// without doubt, for a book with none. 204; 404 no such book (none in a books
// library); 409 asin_set when it has another, or an admin's ASIN edit.
func (a *API) handleBridgeEnrichment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "book_not_found", "no such book")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" || len(path) > catalog.MaxPathBytes {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "a book path is required")
		return
	}
	var req struct {
		ASIN string `json:"asin"`
	}
	if err := decodeJSON(r, &req, 1024); err != nil || !catalog.ValidASIN(req.ASIN) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "an ASIN is required")
		return
	}
	switch err := a.cat.BackfillASIN(r.Context(), id, path, req.ASIN); {
	case errors.Is(err, catalog.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "book_not_found", "no such book")
	case err != nil:
		a.writeAudibleError(w, err, "bridge enrichment failed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleAdminAudibleAccounts serves GET /admin/audible/accounts: every account
// the Bridge mapped, with its titles counted.
func (a *API) handleAdminAudibleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.cat.AudibleAccounts(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "audible accounts failed", "could not list the Audible accounts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// handleAdminAudibleTitles serves GET /admin/audible/titles?account=&state=
// &matched=&q=&after=&limit=: the titles, by title, keyset-paged.
// {"titles": [...], "next": cursor | ""}.
func (a *API) handleAdminAudibleTitles(w http.ResponseWriter, r *http.Request) {
	var ok bool
	q := r.URL.Query()
	f := catalog.AudibleTitleFilter{Account: q.Get("account"), State: q.Get("state"), Query: q.Get("q"), After: q.Get("after")}
	switch q.Get("matched") {
	case "":
	case "1", "true":
		f.Matched = new(true)
	case "0", "false":
		f.Matched = new(false)
	default:
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "matched must be 1 or 0")
		return
	}
	if f.Limit, ok = positiveLimit(w, r); !ok {
		return
	}
	page, err := a.cat.AudibleTitles(r.Context(), f)
	if err != nil {
		a.writeAudibleError(w, err, "audible titles failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleAdminAudiblePoll serves POST /admin/audible/accounts/{account}/poll:
// Check now. Queues the account's audible.poll (201), or answers its open one
// (200), raised to a person's priority when the scheduler queued it; 404
// unknown_account, 409 account_paused, 409 kind_unavailable.
func (a *API) handleAdminAudiblePoll(w http.ResponseWriter, r *http.Request) {
	id, ok := audibleAccountID(w, r)
	if !ok {
		return
	}
	acct, err := a.cat.AudibleAccount(r.Context(), id)
	if err != nil {
		a.writeAudibleError(w, err, "audible poll failed")
		return
	}
	if acct.Paused {
		writeErrorCode(w, http.StatusConflict, "account_paused", "the account's library was deleted; map it again on the Bridge")
		return
	}
	nj := bridge.AudiblePoll(id, 0)
	nj.RequestedBy = userFrom(r.Context()).ID
	job, created, err := a.bridge.Enqueue(r.Context(), nj)
	if !a.writeBridgeError(w, err, "audible poll failed") {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if err := a.cat.MarkAudibleQueued(r.Context(), id); err != nil {
			a.log.Warn("audible poll: record queue time failed", "err", err)
		}
		a.audit(r, "bridge.job", "", map[string]any{"job": job.ID, "kind": job.Kind, "account": id})
	}
	writeJSON(w, status, job)
}

// handleAdminAudibleAccountDelete serves DELETE /admin/audible/accounts/
// {account}: the console removes an account from this server (its titles go
// with it, its open poll is cancelled), for one the Bridge can no longer
// remove itself. It signs nothing out of Amazon: that is the Bridge's. 204,
// known or not; audited when there was one.
func (a *API) handleAdminAudibleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := audibleAccountID(w, r)
	if !ok {
		return
	}
	known := true
	if _, err := a.cat.AudibleAccount(r.Context(), id); errors.Is(err, catalog.ErrUnknownAccount) {
		known = false
	} else if err != nil {
		a.writeAudibleError(w, err, "delete audible account failed")
		return
	}
	if err := a.cat.DeleteAudibleAccount(r.Context(), id); err != nil {
		a.writeAudibleError(w, err, "delete audible account failed")
		return
	}
	if known {
		a.audit(r, "bridge.audible.remove", "", map[string]any{"account": id})
	}
	w.WriteHeader(http.StatusNoContent)
}
