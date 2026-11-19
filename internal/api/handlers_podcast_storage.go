package api

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast"
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

// handleAdminPodcastFetch is POST /admin/libraries/{id}/podcasts/fetch: an
// admin's download of the episode at path, 200 downloaded when its committed
// generation opens, else its fetch asked of the Bridge (202). An episode the
// share refuses the server for good (library.ErrEpisodeRefused: a file whose
// permissions a copy or a restore left wrong) is fetched too: the stream is
// the 404, queuing nothing, but this is the admin asking for the file again,
// and the Bridge writes a new one in its place.
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
	present, err := a.podcastPresent(r.Context(), lib, req.Path, pin)
	if errors.Is(err, library.ErrEpisodeRefused) {
		present, err = false, nil // not one the server can open: fetched again
	}
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left during the checks: no one to answer
		}
		a.writePodcastStorageError(w, err)
		return
	}
	if present {
		writeJSON(w, 200, map[string]bool{"downloaded": true})
		return
	}
	job, created, err := podcast.EnsureFetch(r.Context(), a.cat, a.bridge, lib.ID, req.Path, pin,
		bridge.NewJob{Priority: bridge.PriorityDownload, RequestedBy: userFrom(r.Context()).ID}, bridge.FetchOptions{Retry: true})
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
	// bridge.ErrNotFound: a fetch's target left the index (checkPodcastTarget);
	// library.ErrEpisodeRefused: the share refuses the episode for good.
	case errors.Is(err, catalog.ErrNotFound), errors.Is(err, bridge.ErrNotFound), errors.Is(err, library.ErrEpisodeRefused):
		writeErrorCode(w, 404, "episode_not_found", "Episode not found.")
	case writeLibraryKindError(w, err):
	default:
		a.writeCatalogError(w, err, "podcast storage failed", "Could not load podcast storage.")
	}
}

// writePodcastIdentityBusy answers a stream for an episode the feed gave another
// identity while its previous identity's fetch is still stopping (it stops at
// the Bridge's next report): a retryable 503 with Retry-After, under the code
// and body the 409 it replaces had, and no fetch status (server#179). The code
// is the contract with shipped players (CROSS-REPO.md section 15): their
// stream probe reads identity_changed on any status as "drop the cached item
// and episode lists, offer Try again", where a fetch status would stop them
// without that, and a download's wait asks again after a 5xx.
func writePodcastIdentityBusy(w http.ResponseWriter) {
	setRetryAfter(w, 2*time.Second)
	w.Header().Set("Cache-Control", "no-store")
	writeErrorCode(w, 503, "identity_changed", "The podcast identity changed.")
}

// servePodcastStream answers GET /stream for the episode at rel in podcast
// library lib: its committed generation opened (library.OpenPodcast, its checks
// and open one metadata.Bounded call up to the stream's deadline: given up on,
// or failing as a share does that a retry may get past, a 503
// share_not_answering; refused for good, library.ErrEpisodeRefused, the 404 an
// episode gone is, never a fetch that would meet the same refusal), else
// fetched on demand through the Bridge.
func (a *API) servePodcastStream(w http.ResponseWriter, r *http.Request, lib *catalog.Library, rel string, deadline time.Time) {
	release := a.cat.MarkPodcastStream(catalog.Ref{LibraryID: lib.ID, Path: rel})
	defer release()
	f, pin, err := library.OpenPodcastBounded(r.Context(), a.cat, *lib, rel, metadata.WaitLeft(deadline))
	if metadata.GaveUp(err) {
		writeShareNotAnswering(w)
		return
	}
	if errors.Is(err, catalog.ErrNotFound) || errors.Is(err, library.ErrEpisodeRefused) {
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
		if a.bridge == nil {
			writePodcastFetchPending(w, bridge.UnavailableFetchStatus(bridge.FetchStatus{}, bridge.ErrKindUnavailable))
			return
		}
		var status bridge.FetchStatus
		f, pin, status, err = a.podcastDemand.Fetch(r.Context(), *lib, rel, userFrom(r.Context()).ID, userFrom(r.Context()).IsDemo)
		if err != nil {
			switch {
			case r.Context().Err() != nil: // the client left: no one to answer
			case errors.Is(err, bridge.ErrIdentityChanged):
				writePodcastIdentityBusy(w)
			default:
				a.writePodcastStorageError(w, err)
			}
			return
		}
		if f == nil {
			if status.RequestID != nil {
				a.prefetchStart(r, catalog.Ref{LibraryID: lib.ID, Path: rel}, false)
			}
			writePodcastFetchPending(w, status)
			return
		}
	}
	defer f.Close()
	defer a.prefetchStart(r, catalog.Ref{LibraryID: lib.ID, Path: rel}, true)()
	if r.URL.Query().Get("transcode") == "1" {
		h, r := a.beginTranscode(w, r, lib.ID, rel)
		if h == nil {
			return
		}
		defer a.endTranscode(h)
		h.ended = media.TranscodeOpened(h.writer(w), r, f, a.ffmpeg, h.ask.start, h.ask.download, transcodeIdleFor(r, h.ask), a.log)
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"podcast-g%d-%s"`, pin.Generation, pin.SHA256))
	w.Header().Set("Cache-Control", "private, no-cache")
	media.ServeOpened(w, r, f, r.URL.Query().Get("download") == "1")
}
