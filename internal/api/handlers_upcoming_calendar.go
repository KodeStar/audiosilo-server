package api

import (
	"net/http"
	"net/url"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/upcoming"
)

// Upcoming releases, Track U3: the calendar feed (capability
// upcoming_calendar) and the Upcoming covers (upcoming_covers). Transport only:
// the feed token is auth's (feed.go), the calendar's text upcoming's (ics.go).
//
//	GET    /me/upcoming/calendar      {"active", "created_at"?}: is there a feed link
//	POST   /me/upcoming/calendar      201 {"path", "created_at"}: a new link, the old one ended
//	DELETE /me/upcoming/calendar      204: no link (idempotent)
//	GET    /me/upcoming/releases.ics  the calendar, for a feed token only
//	GET    /me/upcoming/cover?work=&size=  a cover the caller's own list hands out

// upcomingFeedPath is the calendar feed's route; a mint answers it with the
// new secret as its ?token=.
const upcomingFeedPath = "/api/v1/me/upcoming/releases.ics"

// feedKinds is the one token kind the calendar feed accepts. No other route
// lists auth.KindFeed, and the feed lists no other kind (requireFeedAuth).
var feedKinds = []string{auth.KindFeed}

// requireFeedAuth authenticates a calendar feed token, as ?token= (a calendar
// app can't set a header) or the Authorization header, and nothing else: a
// session, an API key, a cast, bridge or pairing token is a 401 here. It is
// mediaAuth's sequence (the media rate limit per credential, the address
// charged for a failure), so it is used ONLY on GET /me/upcoming/releases.ics.
func (a *API) requireFeedAuth(next http.Handler) http.Handler {
	return a.mediaAuth(next, feedKinds)
}

// upcomingCalendarCache is the feed's Cache-Control: a calendar app polls it,
// and the answer is one person's.
const upcomingCalendarCache = "private, max-age=3600"

// handleUpcomingCalendarStatus answers GET /me/upcoming/calendar (requireAuth,
// notKid): whether the caller has a feed link, never the link itself.
func (a *API) handleUpcomingCalendarStatus(w http.ResponseWriter, r *http.Request) {
	if a.upcomingOff(w) {
		return
	}
	st, err := a.auth.FeedTokenStatus(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		a.log.Warn("read calendar feed status failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read the calendar link")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// upcomingCalendarLink is POST /me/upcoming/calendar's answer: the feed's
// path with its secret (shown once; the player prefixes the address it reaches
// the server at, since the server knows no reliable public one) and when it was
// made.
type upcomingCalendarLink struct {
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

// handleMintUpcomingCalendar answers POST /me/upcoming/calendar (requireAuth,
// notKid): a new feed token, the caller's earlier one ended in the same
// transaction. Refused for an API key (a key never mints a credential:
// denyAPIKey) and a demo account (403 demo_account), then 404 metadata_off /
// upcoming_off. The answer holds a secret: no-store.
func (a *API) handleMintUpcomingCalendar(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	u := userFrom(r.Context())
	if u.IsDemo {
		writeErrorCode(w, http.StatusForbidden, codeDemoAccount, "not available for demo accounts")
		return
	}
	if a.upcomingOff(w) {
		return
	}
	secret, created, err := a.auth.IssueFeedToken(r.Context(), u.ID)
	if writeKidRefusal(w, err) {
		return
	}
	if err != nil {
		a.log.Warn("mint calendar feed token failed", "user", u.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not make a calendar link")
		return
	}
	noStore(w)
	writeJSON(w, http.StatusCreated, upcomingCalendarLink{
		Path:      upcomingFeedPath + "?" + url.Values{"token": {secret}}.Encode(),
		CreatedAt: created,
	})
}

// handleDeleteUpcomingCalendar answers DELETE /me/upcoming/calendar
// (requireAuth, notKid): the caller's feed link ends; 204 with or without one.
// Session only (denyAPIKey), as the mint. It is not gated on the feature's
// switches: ending a link only takes access away, so it works while upcoming
// releases are off too.
func (a *API) handleDeleteUpcomingCalendar(w http.ResponseWriter, r *http.Request) {
	if denyAPIKey(w, r) {
		return
	}
	if err := a.auth.RevokeFeedToken(r.Context(), userFrom(r.Context()).ID); err != nil {
		a.log.Warn("revoke calendar feed token failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not turn the calendar link off")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpcomingCalendarFeed answers GET /me/upcoming/releases.ics
// (requireFeedAuth, notKid): the token's owner's /me/upcoming list, read with
// their scopes at this request, as an iCalendar document (upcoming.ICS).
func (a *API) handleUpcomingCalendarFeed(w http.ResponseWriter, r *http.Request) {
	if a.upcomingOff(w) {
		return
	}
	ans, ok := a.myUpcoming(w, r)
	if !ok {
		return
	}
	cfg := a.config()
	body := upcoming.ICS(ans, upcoming.Calendar{ServerID: cfg.ServerID, ServerName: cfg.Name, Now: time.Now()})
	h := w.Header()
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	h.Set("Content-Disposition", `inline; filename="releases.ics"`)
	h.Set("Cache-Control", upcomingCalendarCache)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleUpcomingCover answers GET /me/upcoming/cover?work=&size=
// (requireMediaAuth, notKid; capability upcoming_covers): a JPEG thumbnail of
// the cover the caller's own /me/upcoming list hands out for that work right
// now, so the player shows it from its own server. It is not an image proxy:
// a work the list doesn't carry (another person's series, one the caller holds,
// none at all) or carries without a cover is 404 not_found. 400 without work or
// with a size outside thumbSizes (unset is defaultThumbSize); the rest is
// /meta/cover's (serveCommunityCover: the shared fetch, cache and ETag).
func (a *API) handleUpcomingCover(w http.ResponseWriter, r *http.Request) {
	if a.upcomingOff(w) {
		return
	}
	q := r.URL.Query()
	work := q.Get("work")
	if work == "" {
		writeError(w, http.StatusBadRequest, "work is required")
		return
	}
	size, ok := optThumbSize(w, q)
	if !ok {
		return
	}
	ans, ok := a.myUpcoming(w, r)
	if !ok {
		return
	}
	rawURL := ""
	for _, it := range ans.Items {
		if it.Work.ID == work {
			rawURL = it.Work.CoverURL
			break
		}
	}
	if rawURL == "" {
		writeErrorCode(w, http.StatusNotFound, codeNotFound, "no such cover")
		return
	}
	a.serveCommunityCover(w, r, rawURL, size)
}
