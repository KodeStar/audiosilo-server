package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Sessions, devices and listening stats for the admin console (redesign Phase
// 4a). Admin-only and transport-only: sessions are recorded from progress saves
// (catalog.RecordHeartbeat, see handlePutProgress), devices are tokens (auth).

// recordHeartbeat folds a progress save into the saving device's listening
// session. Best effort: a failure is logged and the save still succeeds.
func (a *API) recordHeartbeat(r *http.Request, userID int64, p catalog.Progress) {
	cred := credentialFrom(r.Context())
	now := time.Now()
	hb := catalog.Heartbeat{
		UserID: userID, Ref: p.Ref, TokenID: cred.ID, DeviceName: cred.DeviceName,
		Client:   catalog.Client(cred.Client),
		Position: p.Position, Duration: p.Duration, Speed: p.PlaybackSpeed, Finished: p.Finished,
		Transcoded: a.streams.Transcoding(cred.ID, p.LibraryID, p.Path, now),
		At:         now,
	}
	if err := a.cat.RecordHeartbeat(r.Context(), hb); err != nil {
		a.log.Warn("record listening session failed", "err", err, "library", p.LibraryID, "path", p.Path)
	}
}

// handleLiveSessions lists who is listening now (sessions with a save in the
// last 10 minutes, one per device).
func (a *API) handleLiveSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.cat.LiveSessions(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "live sessions failed", "could not load live sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleListSessions lists sessions newest first, optionally for one user and/or
// one book (?library_id=&path=), a page at a time (?before= is next_before).
func (a *API) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := catalog.SessionFilter{Limit: queryInt(r, "limit", 50)}
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"user_id", &f.UserID}, {"library_id", &f.LibraryID}, {"before", &f.Before}} {
		var ok bool
		if *p.dst, ok = parseOptionalID(q.Get(p.name)); !ok {
			writeError(w, http.StatusBadRequest, "invalid "+p.name)
			return
		}
	}
	if v := q.Get("path"); v != "" {
		if f.LibraryID == 0 {
			writeError(w, http.StatusBadRequest, "path needs library_id")
			return
		}
		f.Path = catalog.CleanRelPath(v)
	}
	sessions, next, err := a.cat.ListSessions(r.Context(), f)
	if err != nil {
		a.writeCatalogError(w, err, "list sessions failed", "could not load sessions")
		return
	}
	var nextBefore *int64
	if next > 0 {
		nextBefore = &next
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "next_before": nextBefore})
}

// handleListDevices lists signed-in devices (sessions and API keys) of one user
// (?user_id=) or of everyone, marking the one making this request.
func (a *API) handleListDevices(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseOptionalID(r.URL.Query().Get("user_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user_id")
		return
	}
	devices, err := a.auth.ListDevices(r.Context(), userID)
	if err != nil {
		a.log.Warn("list devices failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not load devices")
		return
	}
	current := credentialFrom(r.Context()).ID
	for i := range devices {
		devices[i].Current = devices[i].ID == current
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// handleRevokeDevice signs one device out. The device making the request is
// refused (sign out of the console instead), so an admin can't cut off their own
// session from a list by mistake.
func (a *API) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid device id")
		return
	}
	if id == credentialFrom(r.Context()).ID {
		writeErrorCode(w, http.StatusConflict, codeCurrentDevice, "this is the device you are using; sign out instead")
		return
	}
	owner, name, kind, _ := a.auth.DeviceLabel(r.Context(), id)
	err := a.auth.RevokeDevice(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		a.log.Warn("revoke device failed", "err", err, "device", id)
		writeError(w, http.StatusInternalServerError, "could not sign the device out")
		return
	}
	a.audit(r, "device.revoke", owner, map[string]any{"device": name, "kind": kind})
	w.WriteHeader(http.StatusNoContent)
}

// lookupUser loads a user by id, answering 404 for an unknown one (500 when the
// lookup fails); nil means the handler is done.
func (a *API) lookupUser(w http.ResponseWriter, r *http.Request, id int64) *auth.User {
	u, err := a.auth.GetUser(r.Context(), id)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
		return nil
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not load user")
		return nil
	}
	return u
}

// handleUserProgress lists one person's progress on every book, with start and
// finish dates.
func (a *API) handleUserProgress(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if a.lookupUser(w, r, id) == nil {
		return
	}
	items, err := a.cat.ListUserProgress(r.Context(), id)
	if err != nil {
		a.writeCatalogError(w, err, "list user progress failed", "could not load progress", "user", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"progress": items})
}

// optionalTime decodes a date field that may be absent (leave it), null (clear
// it), an RFC3339 time or a YYYY-MM-DD day (day is then set: the start of that
// day, server time; endOfDay moves a finish date to the day's end).
type optionalTime struct {
	catalog.OptionalTime
	day bool
}

// endOfDay moves a day-only finish date to the end of that day, or to now when
// that is sooner: a book started at 3 pm and "finished today" was finished after
// it started, not at midnight before it.
func (o *optionalTime) endOfDay(now time.Time) {
	if !o.day || o.Value == nil {
		return
	}
	end := o.Value.AddDate(0, 0, 1).Add(-time.Second)
	if end.After(now) {
		end = now
	}
	o.Value = &end
}

func (o *optionalTime) UnmarshalJSON(b []byte) error {
	o.Set = true
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		day, derr := time.ParseInLocation(time.DateOnly, *s, time.Local)
		if derr != nil {
			return err
		}
		t, o.day = day, true
	}
	o.Value = &t
	return nil
}

// progressEditBody is the body of a progress edit, the admin's and the
// listener's own (handleEditMyProgress): absent fields stay as they are.
type progressEditBody struct {
	Finished   *bool        `json:"finished"`
	Position   *float64     `json:"position"`
	StartedAt  optionalTime `json:"started_at"`
	FinishedAt optionalTime `json:"finished_at"`
}

// msgBadProgressEdit is the 400 for a progress edit that doesn't fit its book
// (catalog.ErrInvalidProgressEdit, or an exact date in the future).
const msgBadProgressEdit = "those dates or that position don't fit this book"

// editDateSkew is how far ahead of the server's clock an exact (RFC3339) date in
// a progress edit may be: a client clock running a little fast, not a future
// date.
const editDateSkew = 5 * time.Minute

// decodeProgressEdit reads a progress edit's body, a day-only finish moved to the
// end of that day (or now), answering 400 for a malformed one (nil: done). An
// exact date in the future is refused here, where it can still be told from a
// day-only one: the catalog's day of slack is for a day-only start (the client's
// today can be the server's tomorrow), and a future finish would be left out of
// the year's finished books until it came.
func decodeProgressEdit(w http.ResponseWriter, r *http.Request) *progressEditBody {
	var body progressEditBody
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return nil
	}
	now := time.Now()
	for _, d := range []optionalTime{body.StartedAt, body.FinishedAt} {
		if d.Value != nil && !d.day && d.Value.After(now.Add(editDateSkew)) {
			writeError(w, http.StatusBadRequest, msgBadProgressEdit)
			return nil
		}
	}
	body.FinishedAt.endOfDay(now)
	return &body
}

// edit is the body as the catalog's ProgressEdit.
func (b *progressEditBody) edit() catalog.ProgressEdit {
	return catalog.ProgressEdit{
		Finished: b.Finished, Position: b.Position,
		StartedAt: b.StartedAt.OptionalTime, FinishedAt: b.FinishedAt.OptionalTime,
	}
}

// handleEditProgress is an admin's edit of a user's progress on a book
// (?path=&user_id=): mark finished or not, move the position, set or clear the
// start and finish dates.
func (a *API) handleEditProgress(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	userID, ok := parseOptionalID(r.URL.Query().Get("user_id"))
	if !ok || userID == 0 {
		writeError(w, http.StatusBadRequest, "invalid user_id")
		return
	}
	user := a.lookupUser(w, r, userID)
	if user == nil {
		return
	}
	body := decodeProgressEdit(w, r)
	if body == nil {
		return
	}
	// The user's own scope, not the admin's: an edit may start progress only on a
	// book the user can see (EditProgress applies it to new rows only).
	scope, err := a.cat.UserScope(r.Context(), user.ID, lib.ID, user.Role == auth.RoleAdmin)
	if err != nil {
		a.log.Warn("user scope failed", "err", err, "user", userID)
		writeError(w, http.StatusInternalServerError, "access check failed")
		return
	}
	saved, err := a.cat.EditProgress(r.Context(), userID, catalog.Ref{LibraryID: lib.ID, Path: p}, body.edit(), scope)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, codeBookNotFound, "no progress or book at this path")
		return
	case errors.Is(err, catalog.ErrNoAccess):
		// 409, not 403: the admin may make the call, the user's access is the conflict
		// (and a 403 from /admin tells the console its session lost the admin role).
		writeErrorCode(w, http.StatusConflict, codeNoAccess, "this person can't see this book; give them access first")
		return
	case err != nil:
		a.writeCatalogError(w, err, "edit progress failed", "could not save progress", "library", lib.ID, "path", p)
		return
	}
	details := map[string]any{"book": lib.Name + ": " + p}
	if body.Finished != nil {
		details["finished"] = *body.Finished
	}
	if body.Position != nil {
		details["position"] = *body.Position
	}
	if body.StartedAt.Set || body.FinishedAt.Set {
		details["dates"] = true
	}
	a.audit(r, "progress.edit", user.Username, details)
	writeJSON(w, http.StatusOK, map[string]any{"progress": saved})
}

// handleActivityStats answers GET /admin/stats?range=: {"activity": ...}, the
// Activity page for the period (7d, 30d, 90d, 1y or a calendar year) in server
// time, without the Overview's figures (the console reads those from the plain
// /admin/stats).
func (a *API) handleActivityStats(w http.ResponseWriter, r *http.Request) {
	label, from, to, err := catalog.ParseActivityRange(r.URL.Query().Get("range"), time.Now(), time.Local)
	if err == nil {
		var activity *catalog.Activity
		if activity, err = a.cat.ActivityFor(r.Context(), label, from, to, time.Local); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"activity": activity})
			return
		}
	}
	a.writeCatalogError(w, err, "activity stats failed", "could not load activity")
}

// handleListeningDays answers GET /admin/listening?range=&user_id=: listening per
// day over the period, of everyone or one person (the year calendar, a person's
// listening year), without computing the rest of the Activity page.
func (a *API) handleListeningDays(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseOptionalID(r.URL.Query().Get("user_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid user_id")
		return
	}
	if userID != 0 && a.lookupUser(w, r, userID) == nil {
		return
	}
	label, from, to, err := catalog.ParseActivityRange(r.URL.Query().Get("range"), time.Now(), time.Local)
	if err == nil {
		var days *catalog.ListeningDays
		if days, err = a.cat.ListeningDaysFor(r.Context(), label, from, to, time.Local, userID); err == nil {
			writeJSON(w, http.StatusOK, days)
			return
		}
	}
	a.writeCatalogError(w, err, "listening days failed", "could not load listening")
}
