package api

import (
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The caller's own listening (player redesign Phase 1b, capability user_stats):
// their stats, their listening per day and their yearly goal. Transport-only;
// the privacy rules (only the caller's listening, book rows only within their
// current access) live in catalog.UserStatsFor and friends. Days, hours and the
// goal's year are in server time, as the admin Activity page.

// handleMyStats answers GET /me/stats?range=: {"stats": UserStats}.
func (a *API) handleMyStats(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	label, from, to, err := catalog.ParseActivityRange(r.URL.Query().Get("range"), time.Now(), time.Local)
	if err == nil {
		var scopes []catalog.Scope
		if scopes, err = a.cat.UserScopes(r.Context(), u.ID, u.Role == auth.RoleAdmin); err == nil {
			var stats *catalog.UserStats
			if stats, err = a.cat.UserStatsFor(r.Context(), label, from, to, time.Local, u.ID, scopes); err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
				return
			}
		}
	}
	a.writeCatalogError(w, err, "user stats failed", "could not load your stats", "user", u.ID)
}

// handleMyListening answers GET /me/listening?range=: the caller's listening per
// day over the period.
func (a *API) handleMyListening(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	label, from, to, err := catalog.ParseActivityRange(r.URL.Query().Get("range"), time.Now(), time.Local)
	if err == nil {
		var days *catalog.UserListening
		if days, err = a.cat.UserListeningFor(r.Context(), label, from, to, time.Local, u.ID); err == nil {
			writeJSON(w, http.StatusOK, days)
			return
		}
	}
	a.writeCatalogError(w, err, "user listening failed", "could not load your listening", "user", u.ID)
}

// writeGoal answers with the caller's goal and this year's finished books.
func (a *API) writeGoal(w http.ResponseWriter, r *http.Request, userID int64) {
	status, err := a.cat.GoalStatusFor(r.Context(), userID, time.Now(), time.Local)
	if err != nil {
		a.writeCatalogError(w, err, "listening goal failed", "could not load your goal", "user", userID)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleGetGoal answers GET /me/goal: {goal|null, year, finished}.
func (a *API) handleGetGoal(w http.ResponseWriter, r *http.Request) {
	a.writeGoal(w, r, userFrom(r.Context()).ID)
}

// handlePutGoal sets the caller's goal from {books_per_year: 1..1000} and answers
// as GET.
func (a *API) handlePutGoal(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var body struct {
		BooksPerYear int `json:"books_per_year"`
	}
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.cat.SetListeningGoal(r.Context(), u.ID, body.BooksPerYear); err != nil {
		a.writeCatalogError(w, err, "save listening goal failed", "could not save your goal", "user", u.ID)
		return
	}
	a.writeGoal(w, r, u.ID)
}

// handleDeleteGoal clears the caller's goal (idempotent).
func (a *API) handleDeleteGoal(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := a.cat.DeleteListeningGoal(r.Context(), u.ID); err != nil {
		a.writeCatalogError(w, err, "delete listening goal failed", "could not clear your goal", "user", u.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
