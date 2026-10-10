package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/matchrun"
)

// Bulk community matching (Health > Not matched > Find matches): transport only;
// the runs are internal/matchrun's. Every endpoint is admin-only, and every one
// but clearing the matches (which sends nothing out) 404s metadata_off while
// community metadata is off, as every outbound call stops.

// Bounds on the bulk match endpoints.
const (
	maxMatchItemsPage = 200
	// maxMatchPicks caps one apply's include and exclude lists (item ids).
	maxMatchPicks = 20000
)

// handleListMatchRuns serves GET /admin/match-runs: the kept runs, newest first,
// with the preferred marketplace a new run would use.
func (a *API) handleListMatchRuns(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	runs, err := a.cat.ListMatchRuns(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "list match runs failed", "could not list match runs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "region": a.config().Metadata.PreferredRegion()})
}

// handleStartMatchRun serves POST /admin/match-runs {"library_id"?, "mode"?}: match
// the unmatched books of a library (or all) in the background; mode "repick"
// instead looks again at community-matched ASINs for the preferred marketplace's,
// and mode "refresh" looks the books with an ASIN or ISBN up by it, to fill in
// their details.
// 202 with the run; 409 match_run_busy while a run is working; 400 for an unknown
// mode, and no_region for a repick with no preferred marketplace set.
func (a *API) handleStartMatchRun(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	var req struct {
		LibraryID int64  `json:"library_id"`
		Mode      string `json:"mode"`
	}
	if err := decodeJSONOptional(r, &req, 0); err != nil || req.LibraryID < 0 {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	region := a.config().Metadata.PreferredRegion()
	switch req.Mode {
	case "":
		req.Mode = catalog.MatchModeMatch
	case catalog.MatchModeMatch, catalog.MatchModeRefresh:
	case catalog.MatchModeRepick:
		if region == "" {
			writeErrorCode(w, http.StatusBadRequest, codeNoRegion, "set a preferred Audible marketplace first")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, `mode must be "match", "repick" or "refresh"`)
		return
	}
	libName, ok := a.optionalLibraryName(w, r, req.LibraryID, "could not start the match run")
	if !ok {
		return
	}
	run, err := a.matchRuns.Start(r.Context(), a.baseCtx, matchrun.StartOptions{
		LibraryID: req.LibraryID, Mode: req.Mode, Region: region, UserID: userFrom(r.Context()).ID,
	})
	if errors.Is(err, matchrun.ErrBusy) {
		writeErrorCode(w, http.StatusConflict, codeMatchRunBusy, "a match run is already working")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "start match run failed", "could not start the match run")
		return
	}
	action := "book.match_run"
	switch run.Mode {
	case catalog.MatchModeRepick:
		action = "book.asin_repick"
	case catalog.MatchModeRefresh:
		action = "book.match_refresh"
	}
	a.audit(r, action, libName, map[string]any{"run": run.ID, "books": run.Total, "region": region})
	writeJSON(w, http.StatusAccepted, run)
}

// optionalLibraryName is the name of library id for an audit target ("" for 0,
// every library), answering 404 (or 500 with failed) itself when it can't (ok
// false then).
func (a *API) optionalLibraryName(w http.ResponseWriter, r *http.Request, id int64, failed string) (string, bool) {
	if id == 0 {
		return "", true
	}
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "library not found")
		return "", false
	}
	if err != nil {
		a.writeCatalogError(w, err, "load library failed", failed, "library", id)
		return "", false
	}
	return lib.Name, true
}

// matchRunFor loads the run named by the {id} path value, answering 400/404 itself
// (nil then).
func (a *API) matchRunFor(w http.ResponseWriter, r *http.Request) *catalog.MatchRun {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return nil
	}
	run, err := a.cat.GetMatchRun(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such match run")
		return nil
	}
	if err != nil {
		a.writeCatalogError(w, err, "load match run failed", "could not load the match run")
		return nil
	}
	return run
}

// handleGetMatchRun serves GET /admin/match-runs/{id}: one run, with its progress
// and counts (the console polls it while the run works).
func (a *API) handleGetMatchRun(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	if run := a.matchRunFor(w, r); run != nil {
		writeJSON(w, http.StatusOK, run)
	}
}

// handleMatchRunItems serves GET /admin/match-runs/{id}/items: a page of the
// run's books (?outcome=auto|review|none|error, ?after=<item id>, ?limit=), each
// with the best candidate, the book as it is now and what each scope would change;
// next_after reads the next page (0 = the last).
func (a *API) handleMatchRunItems(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	q := r.URL.Query()
	outcome := q.Get("outcome")
	switch outcome {
	case "", catalog.OutcomeAuto, catalog.OutcomeReview, catalog.OutcomeNone, catalog.OutcomeError:
	default:
		writeError(w, http.StatusBadRequest, "unknown outcome")
		return
	}
	after, ok := parseOptionalID(q.Get("after"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid after")
		return
	}
	limit := min(max(queryInt(r, "limit", 50), 1), maxMatchItemsPage)
	run := a.matchRunFor(w, r)
	if run == nil {
		return
	}
	items, next, err := a.matchRuns.Items(r.Context(), run, outcome, after, limit)
	if err != nil {
		a.writeCatalogError(w, err, "list match run items failed", "could not list the match run", "run", run.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_after": next})
}

// handleApplyMatchRun serves POST /admin/match-runs/{id}/apply {"scope",
// "include"?, "exclude"?}: write the run's confident matches (less exclude, plus
// the include items, which may be ones that needed review) in the background,
// under scope ids | fill | overwrite (a refresh run: fill | overwrite, as it never
// changes an identifier). 202 with the run; 404 for a run that isn't there (or was
// cleared meanwhile); 409 match_run_busy while a run is working,
// match_run_not_ready unless this one is ready.
func (a *API) handleApplyMatchRun(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	var req struct {
		Scope   string  `json:"scope"`
		Include []int64 `json:"include"`
		Exclude []int64 `json:"exclude"`
	}
	if err := decodeJSON(r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !matchrun.ValidScope(req.Scope) {
		writeError(w, http.StatusBadRequest, `scope must be "ids", "fill" or "overwrite"`)
		return
	}
	if len(req.Include) > maxMatchPicks || len(req.Exclude) > maxMatchPicks {
		writeErrorCode(w, http.StatusBadRequest, codeTooLarge, "too many items in one apply")
		return
	}
	run := a.matchRunFor(w, r)
	if run == nil {
		return
	}
	started, err := a.matchRuns.Apply(r.Context(), a.baseCtx, run.ID, matchrun.ApplyOptions{
		Scope: req.Scope, Include: req.Include, Exclude: req.Exclude, UserID: userFrom(r.Context()).ID,
	})
	switch {
	case errors.Is(err, matchrun.ErrBusy):
		writeErrorCode(w, http.StatusConflict, codeMatchRunBusy, "a match run is already working")
		return
	case errors.Is(err, matchrun.ErrScope):
		writeError(w, http.StatusBadRequest, "scope must be one of "+strings.Join(matchrun.ScopesFor(run.Mode), ", "))
		return
	case errors.Is(err, catalog.ErrRunNotReady):
		writeErrorCode(w, http.StatusConflict, codeMatchRunNotReady, "the match run is not ready to apply")
		return
	case errors.Is(err, catalog.ErrNotFound): // cleared since matchRunFor loaded it
		writeError(w, http.StatusNotFound, "no such match run")
		return
	case err != nil:
		a.writeCatalogError(w, err, "apply match run failed", "could not apply the match run", "run", run.ID)
		return
	}
	details := map[string]any{"run": started.ID, "books": started.ApplyTotal}
	if matchrun.ScopesFor(started.Mode) != nil {
		details["scope"] = req.Scope // a repick writes its ASIN whatever the scope
	}
	a.audit(r, "book.match_apply", started.LibraryName, details)
	writeJSON(w, http.StatusAccepted, started)
}

// handleCancelMatchRun serves POST /admin/match-runs/{id}/cancel: stop the run
// while it works. Matching stops (cancelled); applying stops after the books in
// hand (ready again, the rest still to apply). 409 match_run_not_running when it
// isn't working.
func (a *API) handleCancelMatchRun(w http.ResponseWriter, r *http.Request) {
	if a.metadataOff(w) {
		return
	}
	run := a.matchRunFor(w, r)
	if run == nil {
		return
	}
	if !a.matchRuns.Cancel(run.ID) {
		writeErrorCode(w, http.StatusConflict, codeMatchRunNotRunning, "the match run isn't working")
		return
	}
	a.audit(r, "book.match_stop", run.LibraryName, map[string]any{"run": run.ID})
	w.WriteHeader(http.StatusNoContent)
}

// handleClearCommunityMatches serves DELETE /admin/community-matches
// (?library_id=, else every library): undo the community matches so the books
// can be matched from fresh (matchrun.Runner.Clear). It sends nothing out, so it
// works with community metadata off, or no service configured, too. 200 with what it removed; 404 for an
// unknown library; 409 match_run_busy while a run is working.
func (a *API) handleClearCommunityMatches(w http.ResponseWriter, r *http.Request) {
	libraryID, ok := parseOptionalID(r.URL.Query().Get("library_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid library_id")
		return
	}
	libName, ok := a.optionalLibraryName(w, r, libraryID, "could not clear the matches")
	if !ok {
		return
	}
	cleared, err := a.matchRuns.Clear(r.Context(), libraryID)
	if errors.Is(err, matchrun.ErrBusy) {
		writeErrorCode(w, http.StatusConflict, codeMatchRunBusy, "a match run is already working")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "clear community matches failed", "could not clear the matches")
		return
	}
	a.audit(r, "book.match_clear", libName,
		map[string]any{"books": cleared.Books, "covers": cleared.Covers, "runs": cleared.Runs})
	writeJSON(w, http.StatusOK, cleared)
}
