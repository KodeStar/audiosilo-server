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
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/podcast"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
)

type podcastRequest struct {
	ID        int64  `json:"id"`
	LibraryID *int64 `json:"library_id"`
	State     string `json:"state"`
	Feed      string `json:"feed"`
	// Title is the show's name as the follower's list or directory gave it
	// (bridge.SubscribeParams.Title), null when none was given (a follow by
	// address, a private feed, a request from before v2.7.0).
	Title     *string               `json:"title"`
	Private   bool                  `json:"private"`
	ShowPath  *string               `json:"show_path"`
	Error     *catalog.PodcastError `json:"error"`
	CreatedAt string                `json:"created_at"`
	UpdatedAt string                `json:"updated_at"`
}

// showSpelled is the index's spelling of show, a show path a client sent,
// among the shows allow admits (nil: every one): catalog.PodcastShowSpelling,
// so an older client or a saved link naming a show folder in the Unicode
// spelling it had before a rename by hand still finds it. A failed lookup is
// answered here (false).
func (a *API) showSpelled(w http.ResponseWriter, r *http.Request, lib int64, show string, allow func(string) bool) (string, bool) {
	spelled, err := a.cat.PodcastShowSpelling(r.Context(), lib, show, allow)
	if err != nil {
		a.writeCatalogError(w, err, "resolve podcast show spelling failed", "Could not load show.")
		return "", false
	}
	return spelled, true
}

// visibleShow is show, a show path a client sent, in the index's spelling
// among the shows s lets the caller see, or false once answered: 404
// show_not_found for a path no show folder could have or one s hides, a
// failed lookup's error (showSpelled) otherwise.
func (a *API) visibleShow(w http.ResponseWriter, r *http.Request, lib int64, s catalog.Scope, show string) (string, bool) {
	if !feed.ShowPath(show) {
		writeShowNotFound(w)
		return "", false
	}
	show, ok := a.showSpelled(w, r, lib, show, s.VisibleInBrowse)
	if !ok {
		return "", false
	}
	if !s.VisibleInBrowse(show) {
		writeShowNotFound(w)
		return "", false
	}
	return show, true
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
	if p.Title != "" {
		r.Title = &p.Title
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
	show, ok := a.visibleShow(w, r, lib.ID, s, r.URL.Query().Get("show"))
	if !ok {
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

// handlePodcastRequestDismiss is DELETE /podcasts/requests/{id}: the caller's
// own follow request, once it failed or was cancelled, leaves their list
// (bridge.DismissSubscribe), 204. Anyone else's, or no such request, is 404
// request_not_found (never saying which); one still open, ended done, or
// ended a moment ago and not folded yet is 409 request_open. The old-client
// gate applies (403 no_library_access), as for every podcast route.
func (a *API) handlePodcastRequestDismiss(w http.ResponseWriter, r *http.Request) {
	if !podcastGate(w, r) {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 || a.bridge == nil {
		writeErrorCode(w, 404, "request_not_found", "Follow request not found.")
		return
	}
	switch err := a.bridge.DismissSubscribe(r.Context(), userFrom(r.Context()).ID, id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, bridge.ErrNotFound):
		writeErrorCode(w, 404, "request_not_found", "Follow request not found.")
	case errors.Is(err, bridge.ErrRequestNotDismissable):
		writeErrorCode(w, 409, "request_open", "Only a follow request that failed or was cancelled can be dismissed.")
	default:
		a.writeCatalogError(w, err, "dismiss podcast request failed", "Could not dismiss the request.")
	}
}

// podcastActionLibrary checks the gate and grant before the private-show scope:
// knowing a private URL is the proof needed to follow that particular show.
// It returns the library and the caller's scope in it, private shows applied,
// from one read of their access (catalog.CallerLibraryScope).
func (a *API) podcastActionLibrary(w http.ResponseWriter, r *http.Request, id int64) (*catalog.Library, catalog.Scope, bool) {
	lib, scope, granted, err := a.cat.CallerLibraryScope(r.Context(), callerOf(r.Context()), id)
	switch {
	case err != nil:
		a.writeCatalogError(w, err, "podcast access failed", "Could not check access.")
	case !granted: // never with no such library
		writeErrorCode(w, 403, "no_library_access", "No access to this library.")
	case lib.Kind != catalog.KindPodcasts:
		writeErrorCode(w, 409, "not_podcast_library", "This library holds books.")
	default:
		return lib, scope, true
	}
	return nil, scope, false
}
func (a *API) handlePodcastFollow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID int64   `json:"library_id"`
		Path      *string `json:"path"`
		FeedURL   *string `json:"feed_url"`
		Private   *bool   `json:"private"`
		// Title labels a follow by URL's request (v2.7.0, optional): the
		// directory's or list's name for the show, cleaned, never kept for a
		// private feed.
		Title *string `json:"title"`
	}
	if decodeJSON(r, &req, 8<<10) != nil || req.LibraryID <= 0 || (req.Path == nil) == (req.FeedURL == nil) || req.Path != nil && (req.Private != nil || req.Title != nil) {
		writeErrorCode(w, 400, "invalid_request", "Send a path or a feed URL.")
		return
	}
	lib, s, ok := a.podcastActionLibrary(w, r, req.LibraryID)
	if !ok {
		return
	}
	user := userFrom(r.Context()).ID
	if req.FeedURL != nil {
		title := ""
		if req.Title != nil {
			title = *req.Title
		}
		a.followByURL(w, r, lib.ID, *req.FeedURL, req.Private != nil && *req.Private, title)
		return
	}
	p := *req.Path
	if p, ok = a.visibleShow(w, r, lib.ID, s, p); !ok {
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
func (a *API) followByURL(w http.ResponseWriter, r *http.Request, lib int64, raw string, private bool, title string) {
	out, err := a.follower().Follow(r.Context(), podcast.FollowURL{Caller: callerOf(r.Context()), LibraryID: lib, FeedURL: raw, Private: private, Title: title})
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
	case errors.Is(err, bridge.ErrInvalid):
		// A request the queue's params check refuses (as an import row's
		// invalid_feed_url, FollowError's code and copy), never a 500. A
		// backstop: feed.NormalizeFeedURL refuses what that check does.
		e, _ := podcast.FollowError(err)
		writeErrorCode(w, 400, e.Code, e.Message)
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
		setRetryAfter(w, 30*time.Second)
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
	_, s, ok := a.podcastActionLibrary(w, r, id)
	if !ok {
		return
	}
	// The follow is stored at the show's spelling, spelled only among the
	// shows the caller can see; catalog.UnfollowPodcast decides which of
	// their own rows go (one in another spelling only from their own rows).
	spelled, ok := a.showSpelled(w, r, id, p, s.VisibleInBrowse)
	if !ok {
		return
	}
	if err := a.cat.UnfollowPodcast(r.Context(), userFrom(r.Context()).ID, id, p, s.VisibleInBrowse, spelled); err != nil {
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
	lib, scope, ok := a.podcastActionLibrary(w, r, req.LibraryID)
	if !ok {
		return
	}
	if req.Path, ok = a.showSpelled(w, r, lib.ID, req.Path, scope.VisibleInBrowse); !ok {
		return
	}
	// The show's identity may be replaced (a re-subscribe) between the reads
	// here and the queue's check of it: the check is then made once more, of the
	// show as it is now (the caller's follow of it included), and is 409
	// identity_changed only when that identity was replaced too.
	var job *bridge.Job
	for retried := false; ; retried = true {
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
		if job, err = a.bridge.OpenJob(r.Context(), bridge.KindPodcastPoll, lib.ID, show.Path); err != nil {
			a.writeCatalogError(w, err, "find open podcast poll failed", "Could not check show.")
			return
		}
		// An open poll joins the ask, unless it was asked to stop: it ends
		// without checking, so the queue ends it and starts a new one.
		if job != nil && !job.CancelRequested {
			break
		}
		st, err := a.podcastState(r.Context(), lib.ID, show.Path)
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
		job, _, err = a.bridge.Enqueue(r.Context(), bridge.NewJob{Kind: bridge.KindPodcastPoll, LibraryID: &lib.ID, Path: &show.Path, Params: st.PollParams(false), RequestedBy: userFrom(r.Context()).ID})
		if errors.Is(err, bridge.ErrIdentityChanged) && !retried {
			continue
		}
		if !a.podcastQueueError(w, err) {
			return
		}
		break
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

// servePodcastCover is a show's cover for p, a show's or an episode's path,
// already in the index's spelling (spelledPath spells a show's own path from
// podcast_shows).
func (a *API) servePodcastCover(w http.ResponseWriter, r *http.Request, lib *catalog.Library, p string) {
	showPath, _, _ := strings.Cut(p, "/")
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
				if metadata.GaveUp(err) {
					// Art on a share that stopped answering: try again, not "none".
					writeShareNotAnswering(w)
					return
				}
				writeError(w, 404, "no cover")
				return
			}
			a.thumbs.Put(key, jpg)
		}
		if jpg == nil {
			writeError(w, 404, "no cover")
			return
		}
		conditional{etag: thumbETag("podcast", size, catalog.CoverVersion(art.version)), cacheControl: coverCache}.serve(w, r, "image/jpeg", jpg)
		return
	}
	a.serveArt(w, r, lib.Root, catalog.CoverSource{CoverPath: show.CoverPath})
}
