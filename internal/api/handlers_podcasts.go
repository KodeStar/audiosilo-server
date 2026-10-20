package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

type podcastRequest struct {
	ID        int64                 `json:"id"`
	LibraryID *int64                `json:"library_id"`
	State     string                `json:"state"`
	Feed      string                `json:"feed"`
	Private   bool                  `json:"private"`
	ShowPath  *string               `json:"show_path"`
	Error     *catalog.PodcastError `json:"error"`
	CreatedAt string                `json:"created_at"`
	UpdatedAt string                `json:"updated_at"`
}

func writeShowNotFound(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusNotFound, "show_not_found", "Show not found.")
}

func writeBridgeUnavailable(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusConflict, "bridge_unavailable", "No online Bridge can check feeds.")
}

func podcastRequestOf(job bridge.Job) podcastRequest {
	j := bridge.RedactedJob(job)
	var p bridge.SubscribeParams
	_ = json.Unmarshal(j.Params, &p)
	r := podcastRequest{ID: j.ID, LibraryID: j.LibraryID, State: j.State, Feed: j.FeedRedacted, Private: p.Private, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt}
	if r.Feed == "" {
		r.Feed = p.FeedURL
	}
	if r.Feed == "" {
		r.Feed = "https://" + p.FeedHost + "/..."
	}
	if p.ShowPath != "" {
		r.ShowPath = &p.ShowPath
	}
	result, _ := bridge.PodcastOutcomeOf(j)
	if result.ShowPath != "" {
		r.ShowPath = &result.ShowPath
	}
	if j.State == bridge.StateFailed {
		r.Error = &catalog.PodcastError{Code: result.Code, Message: result.Message}
	}
	return r
}
func (a *API) podcastReadScope(w http.ResponseWriter, r *http.Request) (*catalog.Library, catalog.Scope, bool) {
	lib, s, ok := a.browseScope(w, r)
	if !ok {
		return nil, s, false
	}
	if lib.Kind != catalog.KindPodcasts {
		writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
		return nil, s, false
	}
	return lib, s, true
}
func (a *API) handlePodcastShows(w http.ResponseWriter, r *http.Request) {
	_, s, ok := a.podcastReadScope(w, r)
	if !ok {
		return
	}
	shows, err := a.cat.PodcastShows(r.Context(), s, userFrom(r.Context()).ID, false)
	if err != nil {
		a.writeCatalogError(w, err, "list podcast shows failed", "Could not load shows.")
		return
	}
	writeJSON(w, 200, map[string]any{"shows": shows})
}
func (a *API) handlePodcastEpisodes(w http.ResponseWriter, r *http.Request) {
	lib, s, ok := a.podcastReadScope(w, r)
	if !ok {
		return
	}
	show := r.URL.Query().Get("show")
	if !feed.ShowPath(show) || !s.VisibleInBrowse(show) {
		writeShowNotFound(w)
		return
	}
	if _, err := a.cat.PodcastShow(r.Context(), lib.ID, show, userFrom(r.Context()).ID); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			writeShowNotFound(w)
		} else {
			a.writeCatalogError(w, err, "load podcast show failed", "Could not load show.")
		}
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = "newest"
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErrorCode(w, 400, "invalid_request", "Invalid limit.")
			return
		}
		limit = n
	}
	// A per-episode grant is supported: keyset pagination must filter before the
	// limit so a denied episode never leaks through the dedicated feed endpoint.
	page, err := a.cat.PodcastEpisodesWithin(r.Context(), lib.ID, show, order, limit, r.URL.Query().Get("cursor"), s)
	if err != nil {
		a.writeCatalogError(w, err, "list podcast episodes failed", "Could not load episodes.")
		return
	}
	writeJSON(w, 200, page)
}
func (a *API) handleMyPodcasts(w http.ResponseWriter, r *http.Request) {
	scopes, ok := a.callerScopes(w, r, "Could not load podcasts.")
	if !ok {
		return
	}
	user := userFrom(r.Context()).ID
	shows := []catalog.PodcastShow{}
	// The scopes already passed the client gate; the caller's podcast libraries
	// are those of them of the podcasts kind.
	libs, err := a.cat.ListLibraries(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "list podcast libraries failed", "Could not load podcasts.")
		return
	}
	podcastLib := map[int64]bool{}
	for _, l := range libs {
		podcastLib[l.ID] = l.Kind == catalog.KindPodcasts
	}
	allowed := map[int64]bool{}
	for _, s := range scopes {
		if !podcastLib[s.LibraryID] {
			continue
		}
		allowed[s.LibraryID] = true
		list, err := a.cat.PodcastShows(r.Context(), s, user, true)
		if err != nil {
			a.writeCatalogError(w, err, "list followed podcasts failed", "Could not load podcasts.")
			return
		}
		shows = append(shows, list...)
	}
	requests := []podcastRequest{}
	if a.bridge != nil {
		jobs, err := a.bridge.SubscribeRequests(r.Context(), user)
		if err != nil {
			a.writeCatalogError(w, err, "list podcast requests failed", "Could not load requests.")
			return
		}
		for _, j := range jobs {
			if j.LibraryID != nil && allowed[*j.LibraryID] {
				requests = append(requests, podcastRequestOf(j))
			}
		}
	}
	writeJSON(w, 200, map[string]any{"shows": shows, "requests": requests})
}

// podcastActionLibrary checks the gate and grant before the private-show scope:
// knowing a private URL is the proof needed to follow that particular show.
func (a *API) podcastActionLibrary(w http.ResponseWriter, r *http.Request, id int64) (*catalog.Library, bool) {
	libs, err := a.cat.CallerLibraries(r.Context(), callerOf(r.Context()))
	if err != nil {
		a.writeCatalogError(w, err, "podcast access failed", "Could not check access.")
		return nil, false
	}
	for _, lib := range libs {
		if lib.ID == id {
			if lib.Kind != catalog.KindPodcasts {
				writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
				return nil, false
			}
			return &lib, true
		}
	}
	writeErrorCode(w, 403, "no_library_access", "No access to this library.")
	return nil, false
}
func (a *API) handlePodcastFollow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID int64   `json:"library_id"`
		Path      *string `json:"path"`
		FeedURL   *string `json:"feed_url"`
		Private   *bool   `json:"private"`
	}
	if decodeJSON(r, &req, 8<<10) != nil || req.LibraryID <= 0 || (req.Path == nil) == (req.FeedURL == nil) || req.Path != nil && req.Private != nil {
		writeErrorCode(w, 400, "invalid_request", "Send a path or a feed URL.")
		return
	}
	lib, ok := a.podcastActionLibrary(w, r, req.LibraryID)
	if !ok {
		return
	}
	user := userFrom(r.Context()).ID
	var show *catalog.PodcastShow
	var err error
	norm := ""
	private := req.Private != nil && *req.Private
	if req.Path != nil {
		p := *req.Path
		s, scopeErr := a.cat.CallerScope(r.Context(), callerOf(r.Context()), lib.ID)
		if scopeErr != nil {
			a.writeCatalogError(w, scopeErr, "podcast scope failed", "Could not check access.")
			return
		}
		if !feed.ShowPath(p) || !s.VisibleInBrowse(p) {
			writeShowNotFound(w)
			return
		}
		show, err = a.cat.PodcastShow(r.Context(), lib.ID, p, user)
		if errors.Is(err, catalog.ErrNotFound) {
			writeShowNotFound(w)
			return
		}
	} else {
		whole, grantErr := a.cat.PodcastWholeGrant(r.Context(), callerOf(r.Context()), lib.ID)
		if grantErr != nil {
			a.writeCatalogError(w, grantErr, "podcast grant failed", "Could not check access.")
			return
		}
		if !whole {
			writeErrorCode(w, 403, "no_library_access", "Following a URL needs access to the whole library.")
			return
		}
		norm, err = feed.NormalizeFeedURL(*req.FeedURL)
		if err != nil {
			writeErrorCode(w, 400, "invalid_feed_url", "Invalid feed URL.")
			return
		}
		private = private || feed.IsPrivateFeedURL(norm)
		show, err = a.cat.PodcastShowByHash(r.Context(), lib.ID, feed.FeedURLHash(norm), user)
	}
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		a.writeCatalogError(w, err, "find podcast failed", "Could not load show.")
		return
	}
	if show != nil {
		state, err := a.cat.PodcastState(r.Context(), lib.ID, show.Path)
		if err != nil {
			a.writeCatalogError(w, err, "load feed state failed", "Could not load show.")
			return
		}
		if state.ErrorCode != "feed_secret_missing" || req.Path != nil {
			if err := a.cat.FollowPodcast(r.Context(), user, lib.ID, show.Path); err != nil {
				a.writeCatalogError(w, err, "follow podcast failed", "Could not follow show.")
				return
			}
			show.Following = true
			writeJSON(w, 200, map[string]any{"state": "following", "show": show})
			return
		}
	}
	if !a.podcastBridgeReady(w, r, bridge.KindPodcastSubscribe) {
		return
	}
	showPath := ""
	if show != nil {
		showPath = show.Path
		// The URL that restores a private show is that show's secret, whatever
		// the request's flag or the URL's look says: it stays out of bridge_jobs.
		private = private || show.Private
	}
	j, _, err := a.bridge.EnqueueSubscribe(r.Context(), lib.ID, user, norm, private, showPath)
	if !a.podcastQueueError(w, err) {
		return
	}
	writeJSON(w, 202, map[string]any{"state": "subscribing", "request": podcastRequestOf(*j)})
}
func (a *API) podcastQueueError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, bridge.ErrTooManyRequests):
		writeErrorCode(w, 429, "too_many_requests", "At most five follow requests may be open.")
	case errors.Is(err, bridge.ErrLibraryNotWritable):
		writeErrorCode(w, 409, "library_not_writable", "The Bridge cannot write to this library.")
	case errors.Is(err, bridge.ErrKindUnavailable):
		writeBridgeUnavailable(w)
	default:
		a.writeCatalogError(w, err, "podcast queue failed", "Could not queue the request.")
	}
	return false
}
func (a *API) podcastBridgeReady(w http.ResponseWriter, r *http.Request, kind string) bool {
	if a.bridge == nil {
		writeBridgeUnavailable(w)
		return false
	}
	h, err := a.bridge.Online(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "podcast Bridge status failed", "Could not check the Bridge.")
		return false
	}
	if h == nil || !h.Offers(kind) {
		writeBridgeUnavailable(w)
		return false
	}
	return true
}
func (a *API) handlePodcastUnfollow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("library_id"), 10, 64)
	p := r.URL.Query().Get("path")
	if err != nil || id <= 0 || !feed.ShowPath(p) {
		writeErrorCode(w, 400, "invalid_request", "A library and show are required.")
		return
	}
	if _, ok := a.podcastActionLibrary(w, r, id); !ok {
		return
	}
	if err := a.cat.UnfollowPodcast(r.Context(), userFrom(r.Context()).ID, id, p); err != nil {
		a.writeCatalogError(w, err, "unfollow podcast failed", "Could not unfollow show.")
		return
	}
	w.WriteHeader(204)
}
func (a *API) handlePodcastCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID int64  `json:"library_id"`
		Path      string `json:"path"`
	}
	if decodeJSON(r, &req, 8<<10) != nil || req.LibraryID <= 0 || !feed.ShowPath(req.Path) {
		writeErrorCode(w, 400, "invalid_request", "A library and show are required.")
		return
	}
	lib, ok := a.podcastActionLibrary(w, r, req.LibraryID)
	if !ok {
		return
	}
	scope, err := a.cat.CallerScope(r.Context(), callerOf(r.Context()), lib.ID)
	if err != nil {
		a.writeCatalogError(w, err, "podcast scope failed", "Could not check access.")
		return
	}
	show, err := a.cat.PodcastShow(r.Context(), lib.ID, req.Path, userFrom(r.Context()).ID)
	if errors.Is(err, catalog.ErrNotFound) || err == nil && (!show.Following || !scope.VisibleInBrowse(req.Path)) {
		writeShowNotFound(w)
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "check podcast failed", "Could not load show.")
		return
	}
	if !a.podcastBridgeReady(w, r, bridge.KindPodcastPoll) {
		return
	}
	job, err := a.bridge.OpenJob(r.Context(), bridge.KindPodcastPoll, lib.ID, show.Path)
	if err != nil {
		a.writeCatalogError(w, err, "find open podcast poll failed", "Could not check show.")
		return
	}
	if job == nil {
		st, err := a.cat.PodcastState(r.Context(), lib.ID, show.Path)
		if err != nil {
			a.writeCatalogError(w, err, "load feed state failed", "Could not check show.")
			return
		}
		if st.LastChecked != nil {
			t, _ := time.Parse(time.RFC3339, *st.LastChecked)
			if time.Since(t) < time.Minute {
				writeErrorCode(w, 429, "checked_recently", "This feed was checked recently.")
				return
			}
		}
		job, _, err = a.bridge.Enqueue(r.Context(), bridge.NewJob{Kind: bridge.KindPodcastPoll, LibraryID: &lib.ID, Path: &show.Path, Params: st.PollParams(), RequestedBy: userFrom(r.Context()).ID})
		if !a.podcastQueueError(w, err) {
			return
		}
	}
	writeJSON(w, 202, map[string]any{"request": map[string]any{"id": job.ID, "state": job.State}})
}
func (a *API) handleBridgeMarker(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeErrorCode(w, 404, "library_not_found", "Library not found.")
		return
	}
	lib, err := a.cat.GetLibrary(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeErrorCode(w, 404, "library_not_found", "Library not found.")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "load marker library failed", "Could not read the library.")
		return
	}
	nonce, err := library.ReadLibraryMarker(lib.Root, id)
	if err != nil {
		writeErrorCode(w, 404, "no_marker", "No valid marker.")
		return
	}
	writeJSON(w, 200, map[string]string{"nonce": nonce})
}
func (a *API) servePodcastCover(w http.ResponseWriter, r *http.Request, lib *catalog.Library, p string) {
	showPath := strings.Split(p, "/")[0]
	show, err := a.cat.PodcastShow(r.Context(), lib.ID, showPath, userFrom(r.Context()).ID)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "load podcast artwork failed", "No cover.")
		return
	}
	if show.CoverPath == "" {
		writeError(w, 404, "no cover")
		return
	}
	if r.URL.Query().Has("size") {
		size, ok := thumbSizeParam(w, r.URL.Query().Get("size"))
		if !ok {
			return
		}
		// Every episode's cover is the show's artwork: one thumbnail per show,
		// cached under the show's path, and no colour reading (shows keep none).
		art := a.coverArt(r.Context(), lib, show.Path, catalog.CoverSource{CoverPath: show.CoverPath})
		if art == nil {
			writeError(w, 404, "no cover")
			return
		}
		key := thumbKey(lib.ID, show.Path, size, art.version)
		jpg, ok := a.thumbs.Get(key)
		if !ok {
			if jpg, err = a.makeThumbnail(r.Context(), lib, show.Path, art, size); err != nil {
				writeError(w, 404, "no cover")
				return
			}
			a.thumbs.Put(key, jpg)
		}
		if jpg == nil {
			writeError(w, 404, "no cover")
			return
		}
		conditional{etag: `"podcast-` + strconv.Itoa(size) + "-" + catalog.CoverVersion(art.version) + `"`, cacheControl: coverCache}.serve(w, r, "image/jpeg", jpg)
		return
	}
	abs, err := library.SafeJoin(lib.Root, show.CoverPath)
	if err != nil {
		writeError(w, 404, "no cover")
		return
	}
	serveSidecarCover(w, r, abs)
}
