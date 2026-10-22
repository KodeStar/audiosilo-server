package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func writePodcastFetchPending(w http.ResponseWriter, st bridge.FetchStatus) {
	code, message := "podcast_fetching", "Episode is being fetched."
	if st.State == "failed" {
		code = "podcast_fetch_failed"
		message = "The episode could not be fetched."
	}
	if st.State == "unavailable" || st.State == "absent" {
		code = "podcast_fetch_unavailable"
		message = "The episode cannot be fetched right now."
	}
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 503, map[string]any{"error": message, "code": code, "fetch": st})
}
func (a *API) podcastFetchQueueError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, bridge.ErrFetchModuleOff):
		writeModuleOff(w)
	case errors.Is(err, bridge.ErrKindUnavailable):
		writeBridgeUnavailable(w)
	default:
		return a.podcastQueueError(w, err)
	}
	return false
}
func (a *API) handlePodcastFetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rel := r.URL.Query().Get("path")
	if r.Method == http.MethodPost {
		var req struct {
			Path string `json:"path"`
		}
		if decodeJSON(r, &req, 4096) != nil {
			writeErrorCode(w, 400, "invalid_request", "An episode path is required.")
			return
		}
		rel = req.Path
	}
	if !feed.EpisodePath(path.Dir(rel), rel) {
		writeErrorCode(w, 400, "invalid_request", "An episode path is required.")
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErrorCode(w, 404, "library_not_found", "Library not found.")
		return
	}
	lib, rel, _, status, msg := a.scopedPath(r, id, rel)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if lib.Kind != catalog.KindPodcasts {
		writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
		return
	}
	pin, err := a.cat.PodcastPin(r.Context(), lib.ID, rel)
	if err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	ready := false
	if pin.Present && feed.ValidFetch(pin.Fetch) && pin.Fetch.DeletedAt == "" {
		f, _, openErr := library.OpenPodcast(r.Context(), a.cat, *lib, rel)
		if openErr == nil {
			ready = true
			_ = f.Close()
		} else if !errors.Is(openErr, library.ErrNotIndexable) && !errors.Is(openErr, catalog.ErrNotFound) {
			a.writePodcastStorageError(w, openErr)
			return
		}
	}
	if r.Method == http.MethodPost && !ready {
		raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: pin.ShowID, EpisodeID: pin.EpisodeID})
		_, _, err = a.bridge.EnsurePodcastFetch(r.Context(), bridge.NewJob{LibraryID: &lib.ID, Path: &rel, Params: raw, Priority: 100, RequestedBy: userFrom(r.Context()).ID}, bridge.FetchOptions{Retry: true})
		if !a.podcastFetchQueueError(w, err) {
			return
		}
	}
	states, err := a.bridge.PodcastFetchStatuses(r.Context(), lib.ID, []string{rel})
	if err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	st := states[rel]
	if !ready && st.State == "ready" {
		st = bridge.FetchStatus{State: "absent"}
	}
	if userFrom(r.Context()).IsDemo && !ready {
		st.State = "unavailable"
		st.RetryAfterSeconds = 2
		st.Error = bridge.FetchErrorFor("demo_account")
	}
	if r.Method == http.MethodPost && a.prefetch != nil {
		a.prefetch.Start(callerOf(r.Context()), catalog.Ref{LibraryID: lib.ID, Path: rel}, false)
	}
	status = 200
	if r.Method == http.MethodPost && !ready {
		status = 202
	}
	writeJSON(w, status, map[string]any{"fetch": st})
}
