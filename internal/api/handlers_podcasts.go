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
	"github.com/kodestar/audiosilo-server/internal/podcast"
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
	writeErrorCode(w, http.StatusConflict, "bridge_unavailable", "No online Bridge can perform this podcast action.")
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
	// One batched status read per page, never one per episode.
	paths := make([]string, len(page.Episodes))
	for i := range page.Episodes {
		paths[i] = page.Episodes[i].Path
	}
	states, err := a.podcastFetchStatuses(r, lib.ID, paths)
	if err != nil {
		a.writeCatalogError(w, err, "load podcast fetch status failed", "Could not load episodes.")
		return
	}
	for i := range page.Episodes {
		if st, ok := states[page.Episodes[i].Path]; ok {
			page.Episodes[i].Fetch = &st
		}
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
	lib, err := a.grantedLibrary(r, id)
	switch {
	case err != nil:
		a.writeCatalogError(w, err, "podcast access failed", "Could not check access.")
	case lib == nil:
		writeErrorCode(w, 403, "no_library_access", "No access to this library.")
	case lib.Kind != catalog.KindPodcasts:
		writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
	default:
		return lib, true
	}
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
	if req.FeedURL != nil {
		a.followByURL(w, r, lib.ID, *req.FeedURL, req.Private != nil && *req.Private)
		return
	}
	p := *req.Path
	s, err := a.cat.CallerScope(r.Context(), callerOf(r.Context()), lib.ID)
	if err != nil {
		a.writeCatalogError(w, err, "podcast scope failed", "Could not check access.")
		return
	}
	if !feed.ShowPath(p) || !s.VisibleInBrowse(p) {
		writeShowNotFound(w)
		return
	}
	show, err := a.cat.PodcastShow(r.Context(), lib.ID, p, user)
	if errors.Is(err, catalog.ErrNotFound) {
		writeShowNotFound(w)
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "find podcast failed", "Could not load show.")
		return
	}
	// A visible show is followed by its path even while its secret is missing
	// (the path, unlike a URL, cannot repair it).
	if err := a.cat.FollowPodcast(r.Context(), user, lib.ID, show.Path, show.ShowID); err != nil {
		a.writeCatalogError(w, err, "follow podcast failed", "Could not follow show.")
		return
	}
	if a.bridge != nil {
		a.bridge.WakePodcasts()
	}
	show.Following = true
	writeJSON(w, 200, map[string]any{"state": "following", "show": show})
}

// followByURL is POST /podcasts/follow with a feed_url: podcast.Follower, the
// implementation an OPML import's rows share. Both follow states answer 200
// "following" (the shipped envelope); a queued or adopted subscription 202.
func (a *API) followByURL(w http.ResponseWriter, r *http.Request, lib int64, raw string, private bool) {
	out, err := a.follower().Follow(r.Context(), podcast.FollowURL{Caller: callerOf(r.Context()), LibraryID: lib, FeedURL: raw, Private: private})
	switch {
	case err == nil:
	case errors.Is(err, podcast.ErrNoWholeGrant):
		writeErrorCode(w, 403, "no_library_access", "Following a URL needs access to the whole library.")
		return
	case errors.Is(err, podcast.ErrBridgeUnavailable):
		writeBridgeUnavailable(w)
		return
	case errors.Is(err, podcast.ErrModuleOff):
		writeModuleOff(w)
		return
	default:
		a.podcastQueueError(w, err)
		return
	}
	if out.State == podcast.StateSubscribing {
		writeJSON(w, 202, map[string]any{"state": "subscribing", "request": podcastRequestOf(*out.Job)})
		return
	}
	writeJSON(w, 200, map[string]any{"state": "following", "show": out.Show})
}
func (a *API) podcastQueueError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case writeLibraryKindError(w, err):
	case errors.Is(err, feed.ErrInvalidFeedURL):
		writeErrorCode(w, 400, "invalid_feed_url", "Invalid feed URL.")
	case errors.Is(err, bridge.ErrPrivacyConflict):
		e, _ := podcast.FollowError(err)
		writeErrorCode(w, 409, e.Code, e.Message)
	case errors.Is(err, bridge.ErrNotFound):
		writeErrorCode(w, 404, "episode_not_found", "Episode not found.")
	case errors.Is(err, bridge.ErrTooManyRequests):
		// The open follows settle as the Bridge works through them: a guess at
		// when one has, so every 429 says when to try again.
		w.Header().Set("Retry-After", "30")
		writeErrorCode(w, 429, "too_many_requests", "At most five follow requests may be open.")
	case errors.Is(err, bridge.ErrFetchLimit):
		writeErrorCode(w, 409, "fetch_limit", bridge.FetchErrorFor("fetch_limit").Message)
	case errors.Is(err, bridge.ErrLibraryNotWritable):
		writeErrorCode(w, 409, "library_not_writable", "The Bridge cannot write to this library.")
	case errors.Is(err, bridge.ErrKindUnavailable):
		writeBridgeUnavailable(w)
	case errors.Is(err, bridge.ErrFetchModuleOff):
		writeModuleOff(w)
	default:
		a.writeCatalogError(w, err, "podcast queue failed", "Could not queue the request.")
	}
	return false
}

// writeLibraryKindError answers the queue's library-kind and podcast-identity
// refusals (409 not_podcast_library, not_book_library, identity_changed), the
// ones every podcast and Bridge error mapper shares, and reports whether err
// was one of them.
func writeLibraryKindError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, bridge.ErrNotPodcastLibrary):
		writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
	case errors.Is(err, bridge.ErrNotBookLibrary):
		writeErrorCode(w, 409, "not_book_library", "This library holds podcasts.")
	case errors.Is(err, bridge.ErrIdentityChanged):
		writeErrorCode(w, 409, "identity_changed", "The podcast identity changed.")
	default:
		return false
	}
	return true
}

// podcastBridgeReady answers 409 bridge_unavailable unless a Bridge is online
// and runs kind, and returns its heartbeat when it is.
func (a *API) podcastBridgeReady(w http.ResponseWriter, r *http.Request, kind string) (*bridge.Heartbeat, bool) {
	if a.bridge == nil {
		writeBridgeUnavailable(w)
		return nil, false
	}
	h, err := a.bridge.Online(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "podcast Bridge status failed", "Could not check the Bridge.")
		return nil, false
	}
	if h == nil || !h.Offers(kind) {
		writeBridgeUnavailable(w)
		return nil, false
	}
	return h, true
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
	if a.bridge != nil {
		a.bridge.WakePodcasts()
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
	if _, ok := a.podcastBridgeReady(w, r, bridge.KindPodcastPoll); !ok {
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
			if since := time.Since(t); since < time.Minute {
				setRetryAfter(w, time.Minute-since)
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
