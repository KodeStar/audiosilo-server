package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/upcoming"
)

// Upcoming releases (Track U1, capability `upcoming`): GET /me/upcoming is the
// caller's preorders, recent releases and missing volumes in the series they
// hold, read from what the background job (internal/upcoming) stored and
// filtered by their access there. Transport only.

// upcomingOn reports whether upcoming releases are live: metadata on AND the
// live upcoming.enabled. The job, the route and the capability gate on it.
func (a *API) upcomingOn() bool { return a.metadataOn() && a.config().Upcoming.Enabled }

// handleMyUpcoming answers GET /me/upcoming (requireAuth, notKid): 404
// metadata_off while metadata is off, 404 upcoming_off while the job is
// switched off.
func (a *API) handleMyUpcoming(w http.ResponseWriter, r *http.Request) {
	if a.upcomingOff(w) {
		return
	}
	ans, ok := a.myUpcoming(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ans)
}

// upcomingOff answers 404 metadata_off while metadata is off, else 404
// upcoming_off while the job is switched off, and reports whether it did: the
// gate of every upcoming route after its auth, kid and credential checks.
func (a *API) upcomingOff(w http.ResponseWriter) bool {
	if a.metadataOff(w) {
		return true
	}
	if !a.upcomingOn() {
		writeErrorCode(w, http.StatusNotFound, codeUpcomingOff, "upcoming releases are turned off")
		return true
	}
	return false
}

// myUpcoming is the request's caller's GET /me/upcoming answer (their scopes,
// read at this request). On a failure it has written the 500 and ok is false.
// The JSON route, the calendar feed and the cover route read the list through
// it, so the three share one access rule.
func (a *API) myUpcoming(w http.ResponseWriter, r *http.Request) (upcoming.Answer, bool) {
	scopes, err := a.cat.CallerScopes(r.Context(), callerOf(r.Context()))
	if err != nil {
		a.writeCatalogError(w, err, "upcoming: scopes", "could not list upcoming releases")
		return upcoming.Answer{}, false
	}
	ans, err := upcoming.List(r.Context(), a.cat, scopes, a.meta.WorkURL, a.config().Upcoming.Window(), time.Now())
	if err != nil {
		a.writeCatalogError(w, err, "upcoming: list", "could not list upcoming releases")
		return upcoming.Answer{}, false
	}
	return ans, true
}

// announceReleases is the job's book_released: one event naming the first few
// releases as "Title (Series #position)".
func (a *API) announceReleases(ctx context.Context, releases []upcoming.Release) {
	if a.rt.Notify == nil || len(releases) == 0 {
		return
	}
	titles := make([]string, 0, maxReleaseTitles)
	ids := make([]string, 0, len(releases))
	for i, rel := range releases {
		ids = append(ids, rel.WorkID)
		if i >= maxReleaseTitles {
			continue
		}
		t := rel.Title
		if t == "" {
			t = rel.WorkID
		}
		switch {
		case rel.Series != "" && rel.Position != "":
			t = fmt.Sprintf("%s (%s #%s)", t, rel.Series, rel.Position)
		case rel.Series != "":
			t = fmt.Sprintf("%s (%s)", t, rel.Series)
		}
		titles = append(titles, t)
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	a.rt.Notify.BooksReleased(ctx, len(releases), titles, "released:"+hex.EncodeToString(sum[:]))
}

// maxReleaseTitles is how many releases a book_released event names (the rest
// are counted). Its dedupe key is a digest of the run's work ids.
const maxReleaseTitles = 5

// upcomingStatus is the job's state for Settings > Metadata, nil without one.
func (a *API) upcomingStatus(ctx context.Context) *upcoming.Status {
	if a.upcoming == nil {
		return nil
	}
	st, err := a.upcoming.Status(ctx)
	if err != nil {
		return nil
	}
	return &st
}
