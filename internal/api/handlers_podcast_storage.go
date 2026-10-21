package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

func (a *API) podcastStorageLibrary(w http.ResponseWriter, r *http.Request) (*catalog.Library, bool) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErrorCode(w, 404, "library_not_found", "Library not found.")
		return nil, false
	}
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, 404, "library_not_found", "Library not found.")
		return nil, false
	}
	if err != nil {
		a.writeCatalogError(w, err, "load podcast library failed", "Could not load library.")
		return nil, false
	}
	if lib.Kind != catalog.KindPodcasts {
		writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
		return nil, false
	}
	return lib, true
}
func (a *API) podcastStorageWritable(w http.ResponseWriter, r *http.Request, lib *catalog.Library, kind string) bool {
	if !a.moduleOn(bridge.ModulePodcasts) {
		writeModuleOff(w)
		return false
	}
	h, ok := a.podcastBridgeReady(w, r, kind)
	if !ok {
		return false
	}
	if !h.Writable(lib.ID) {
		writeErrorCode(w, 409, "library_not_writable", "The Bridge cannot write to this library.")
		return false
	}
	return true
}
func (a *API) handleAdminPodcastRetention(w http.ResponseWriter, r *http.Request) {
	lib, ok := a.podcastStorageLibrary(w, r)
	if !ok {
		return
	}
	showPath := r.URL.Query().Get("path")
	if !feed.ShowPath(showPath) {
		writeErrorCode(w, 400, "invalid_request", "A show path is required.")
		return
	}
	show, err := a.cat.PodcastShow(r.Context(), lib.ID, showPath, 0)
	if errors.Is(err, catalog.ErrNotFound) {
		writeShowNotFound(w)
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "load retention show failed", "Could not load show.")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"retention": show.Retention})
		return
	}
	if !a.podcastStorageWritable(w, r, lib, bridge.KindPodcastRetain) {
		return
	}
	var policy *catalog.Policy
	if r.Method == http.MethodPut {
		// Pointer fields distinguish omitted values from the valid days=0.
		var req struct {
			Mode *string `json:"mode"`
			Keep *int    `json:"keep"`
			Days *int    `json:"days"`
		}
		if decodeJSON(r, &req, 4096) != nil || req.Mode == nil || req.Keep == nil || req.Days == nil {
			writeErrorCode(w, 400, "invalid_request", "All retention fields are required.")
			return
		}
		policy = &catalog.Policy{Mode: *req.Mode, Keep: *req.Keep, Days: *req.Days}
		if !policy.Valid() {
			writeErrorCode(w, 400, "invalid_request", "Invalid retention policy.")
			return
		}
	}
	if err = a.cat.SetPodcastPolicy(r.Context(), lib.ID, showPath, show.ShowID, policy); err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	a.audit(r, "library.update", lib.Name, map[string]any{"podcast": showPath, "retention": policy})
	a.bridge.WakePodcasts()
	if policy == nil {
		w.WriteHeader(204)
	} else {
		writeJSON(w, 200, map[string]any{"retention": policy})
	}
}
func (a *API) handleAdminPodcastFetch(w http.ResponseWriter, r *http.Request) {
	lib, ok := a.podcastStorageLibrary(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if decodeJSON(r, &req, 4096) != nil || !feed.EpisodePath(path.Dir(req.Path), req.Path) {
		writeErrorCode(w, 400, "invalid_request", "An episode path is required.")
		return
	}
	pin, err := a.cat.PodcastPin(r.Context(), lib.ID, req.Path)
	if err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	if !a.podcastStorageWritable(w, r, lib, bridge.KindPodcastFetch) {
		return
	}
	// Only an indexed, committed pin can be a present file worth opening:
	// OpenPodcast answers ErrNotIndexable for every other one.
	if pin.Present && feed.ValidFetch(pin.Fetch) && pin.Fetch.DeletedAt == "" {
		f, _, openErr := library.OpenPodcast(r.Context(), a.cat, *lib, req.Path)
		if openErr == nil {
			_ = f.Close()
			writeJSON(w, 200, map[string]bool{"downloaded": true})
			return
		}
		if !errors.Is(openErr, library.ErrNotIndexable) && !errors.Is(openErr, catalog.ErrNotFound) {
			a.writePodcastStorageError(w, openErr)
			return
		}
	}
	raw, _ := json.Marshal(bridge.EpisodeParams{ShowID: pin.ShowID, EpisodeID: pin.EpisodeID})
	job, created, err := a.bridge.Enqueue(r.Context(), bridge.NewJob{Kind: bridge.KindPodcastFetch, LibraryID: &lib.ID, Path: &req.Path, Params: raw, Priority: 20, RequestedBy: userFrom(r.Context()).ID})
	if !a.podcastQueueError(w, err) {
		return
	}
	if created {
		a.audit(r, "bridge.job", req.Path, map[string]any{"job": job.ID, "kind": job.Kind, "library": lib.ID})
	}
	writeJSON(w, 202, map[string]any{"request": map[string]any{"id": job.ID, "state": job.State}})
}
func (a *API) handleBridgePodcastRetention(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	lib, ok := a.podcastStorageLibrary(w, r)
	if !ok {
		return
	}
	show, id := r.URL.Query().Get("path"), r.URL.Query().Get("show_id")
	if !feed.ShowPath(show) || !feed.ValidHex(id, 16) {
		writeErrorCode(w, 400, "invalid_request", "A show path and identity are required.")
		return
	}
	p, err := a.cat.PodcastPolicy(r.Context(), lib.ID, show, id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeShowNotFound(w)
		return
	}
	if err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"retention": p})
}
func (a *API) handleBridgePodcastRetentionCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	lib, ok := a.podcastStorageLibrary(w, r)
	if !ok {
		return
	}
	var req catalog.RetentionRequest
	if decodeJSON(r, &req, 4096) != nil || !feed.ValidHex(req.ShowID, 16) || !feed.EpisodePath(path.Dir(req.Path), req.Path) || req.Generation < 1 || !feed.ValidHex(req.SHA256, 64) || req.Bytes <= 0 {
		writeErrorCode(w, 400, "invalid_request", "A committed episode identity is required.")
		return
	}
	check, err := a.cat.PodcastRetentionCheck(r.Context(), lib.ID, req)
	if err != nil {
		a.writePodcastStorageError(w, err)
		return
	}
	writeJSON(w, 200, check)
}
func (a *API) writePodcastStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeErrorCode(w, 404, "episode_not_found", "Episode not found.")
	case writeLibraryKindError(w, err):
	default:
		a.writeCatalogError(w, err, "podcast storage failed", "Could not load podcast storage.")
	}
}
func (a *API) servePodcastStream(w http.ResponseWriter, r *http.Request, lib *catalog.Library, rel string) {
	release := a.cat.MarkPodcastStream(catalog.Ref{LibraryID: lib.ID, Path: rel})
	defer release()
	f, pin, err := library.OpenPodcast(r.Context(), a.cat, *lib, rel)
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, 404, "episode_not_found", "Episode not found.")
		return
	}
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left during the checks: no one to answer
		}
		if !errors.Is(err, library.ErrNotIndexable) {
			a.writeCatalogError(w, err, "open podcast failed", "Could not load episode.")
			return
		}
		writeErrorCode(w, 404, "not_downloaded", "episode not downloaded")
		return
	}
	defer f.Close()
	if r.URL.Query().Get("transcode") == "1" {
		done, start, ok := a.beginTranscode(w, r, lib.ID, rel)
		if !ok {
			return
		}
		defer done()
		media.TranscodeOpened(w, r, f, a.ffmpeg, start, a.log)
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"podcast-g%d-%s"`, pin.Generation, pin.SHA256))
	w.Header().Set("Cache-Control", "private, no-cache")
	media.ServeOpened(w, r, f, r.URL.Query().Get("download") == "1")
}
