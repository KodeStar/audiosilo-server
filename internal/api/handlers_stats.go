package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// handleStats powers the admin Overview/Stats dashboard: catalog totals, per-
// library book counts, and a cross-user "who's listening" feed. It is admin-only
// (registered under requireAdmin), so listening data spans every user. With
// ?range= (7d, 30d, 90d, 1y or a year such as 2025) it adds "activity", the
// Activity page for that period (catalog.Activity); without it the answer is the
// Overview's, as before.
func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var activity *catalog.Activity
	if r.URL.Query().Has("range") {
		var err error
		activity, err = a.activityStats(r)
		if err != nil {
			a.writeCatalogError(w, err, "activity stats failed", "could not load activity")
			return
		}
	}

	counts, err := a.cat.CountBooksByLibrary(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not count books")
		return
	}
	libs, err := a.cat.ListLibraries(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list libraries")
		return
	}
	type libStat struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		BookCount int    `json:"book_count"`
	}
	libStats := make([]libStat, 0, len(libs))
	total := 0
	for _, l := range libs {
		n := counts[l.ID]
		total += n
		libStats = append(libStats, libStat{ID: l.ID, Name: l.Name, BookCount: n})
	}

	users, err := a.auth.ListUsers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list users")
		return
	}
	listening, err := a.cat.ListeningOverview(ctx, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load listening activity")
		return
	}

	out := map[string]any{
		"total_books":     total,
		"total_libraries": len(libs),
		"total_users":     len(users),
		"libraries":       libStats,
		"listening":       listening,
	}
	if activity != nil {
		out["activity"] = activity
	}
	writeJSON(w, http.StatusOK, out)
}
