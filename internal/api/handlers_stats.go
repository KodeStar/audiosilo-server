package api

import (
	"net/http"
)

// handleStats powers the admin Overview/Stats dashboard: catalog totals, per-
// library book counts, and a cross-user "who's listening" feed. It is admin-only
// (registered under requireAdmin), so listening data spans every user. With
// ?range= (7d, 30d, 90d, 1y or a year such as 2025) it answers the Activity page
// instead (handleActivityStats): {"activity": ...} without the Overview's figures.
func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("range") {
		a.handleActivityStats(w, r)
		return
	}
	ctx := r.Context()
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
	writeJSON(w, http.StatusOK, out)
}
