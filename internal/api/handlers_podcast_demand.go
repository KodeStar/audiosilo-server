package api

import (
	"context"
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

// podcastFetchStatuses reads fetch summaries for the caller in one batch. A
// demo account cannot start disk work, so it sees what is not ready as
// unavailable to it.
func (a *API) podcastFetchStatuses(r *http.Request, lib int64, paths []string) (map[string]bridge.FetchStatus, error) {
	if a.bridge == nil {
		return nil, nil
	}
	states, err := a.bridge.PodcastFetchStatuses(r.Context(), lib, paths)
	if err != nil || !userFrom(r.Context()).IsDemo {
		return states, err
	}
	for p, st := range states {
		states[p] = bridge.UnavailableFetchStatus(st, bridge.ErrFetchDemo)
	}
	return states, nil
}

// podcastPresent reports whether the episode's committed generation opens.
// Only an indexed, committed pin can be a present file worth opening:
// OpenPodcast answers ErrNotIndexable or ErrNotFound for every other one, and
// those mean absent; any other open failure is returned.
func (a *API) podcastPresent(ctx context.Context, lib *catalog.Library, rel string, pin catalog.PodcastPin) (bool, error) {
	if !pin.Present || !feed.ValidFetch(pin.Fetch) || pin.Fetch.DeletedAt != "" {
		return false, nil
	}
	f, _, err := library.OpenPodcast(ctx, a.cat, *lib, rel)
	if err == nil {
		_ = f.Close()
		return true, nil
	}
	if errors.Is(err, library.ErrNotIndexable) || errors.Is(err, catalog.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// prefetchStart and prefetchReconsider are the one-ahead queue preparation
// hooks: never for a demo account, which cannot start disk work.
func (a *API) prefetchStart(r *http.Request, current catalog.Ref, stream bool) func() {
	if a.prefetch == nil || userFrom(r.Context()).IsDemo {
		return func() {}
	}
	return a.prefetch.Start(callerOf(r.Context()), current, stream)
}
func (a *API) prefetchReconsider(r *http.Request) {
	if a.prefetch != nil && !userFrom(r.Context()).IsDemo {
		a.prefetch.Reconsider(callerOf(r.Context()))
	}
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
	ready, err := a.podcastPresent(r.Context(), lib, rel, pin)
	if err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	if a.bridge == nil && !ready {
		if r.Method == http.MethodPost {
			writeBridgeUnavailable(w)
			return
		}
		writeJSON(w, 200, map[string]any{"fetch": bridge.UnavailableFetchStatus(bridge.FetchStatus{}, bridge.ErrKindUnavailable)})
		return
	}
	if r.Method == http.MethodPost && !ready {
		raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: pin.ShowID, EpisodeID: pin.EpisodeID})
		_, _, err = a.bridge.EnsurePodcastFetch(r.Context(), bridge.NewJob{LibraryID: &lib.ID, Path: &rel, Params: raw, Priority: 100, RequestedBy: userFrom(r.Context()).ID}, bridge.FetchOptions{Retry: true})
		if !a.podcastQueueError(w, err) {
			return
		}
	}
	// A pin that opened is ready whatever the job rows say; one the index calls
	// present that did not open shows its open work or refusal instead.
	var st bridge.FetchStatus
	if ready {
		st = bridge.ReadyFetchStatus(pin.Fetch)
	} else if st, err = a.bridge.UnreadyFetchStatus(r.Context(), lib.ID, rel); err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	if userFrom(r.Context()).IsDemo && !ready {
		st = bridge.UnavailableFetchStatus(st, bridge.ErrFetchDemo)
	}
	if r.Method == http.MethodPost {
		a.prefetchStart(r, catalog.Ref{LibraryID: lib.ID, Path: rel}, false)
	}
	status = 200
	if r.Method == http.MethodPost && !ready {
		status = 202
	}
	writeJSON(w, status, map[string]any{"fetch": st})
}
