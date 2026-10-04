package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/notify"
)

// Notifications (Settings > Notifications) and the event feed (the console's bell).
// A destination's address and secret are credentials: they go in, never out. The
// console sees a redacted address and whether a secret is set.

// notifyTarget is a destination as the console sees it.
type notifyTarget struct {
	catalog.NotifyTarget
	Address   string `json:"address"`
	HasSecret bool   `json:"has_secret"`
}

func targetView(t catalog.NotifyTarget) notifyTarget {
	return notifyTarget{NotifyTarget: t, Address: notify.Redact(t.URL), HasSecret: t.Secret != ""}
}

// handleListNotifyTargets lists the destinations (admin only): GET
// /admin/notifications, with the event kinds and destination kinds the server knows.
func (a *API) handleListNotifyTargets(w http.ResponseWriter, r *http.Request) {
	list, err := a.cat.ListNotifyTargets(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "notify: list", "could not load the notification settings")
		return
	}
	out := make([]notifyTarget, len(list))
	for i, t := range list {
		out[i] = targetView(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targets": out, "events": notify.Kinds, "kinds": notify.TargetKinds,
	})
}

// targetBody is a destination's fields in a create or change. On a change an absent
// field keeps its value; secret "" clears it.
type targetBody struct {
	Kind    *string   `json:"kind"`
	Name    *string   `json:"name"`
	URL     *string   `json:"url"`
	Secret  *string   `json:"secret"`
	Enabled *bool     `json:"enabled"`
	Events  *[]string `json:"events"`
}

// apply lays the body over t and checks the result. A destination's kind is set
// when it is created and can't change after.
func (b targetBody) apply(t *catalog.NotifyTarget, creating bool) error {
	if b.Kind != nil {
		if !creating && *b.Kind != t.Kind {
			return notify.Refuse("kind", notify.ReasonKindFixed, "a destination's kind can't change; add a new one")
		}
		t.Kind = *b.Kind
	}
	if b.Name != nil {
		t.Name = *b.Name
	}
	if b.URL != nil {
		// A saved secret (an ntfy access token, a webhook's signing key) belongs to the
		// address it was given for: one moving to another server must come again (or
		// be cleared), or it would be sent where it was never meant to go.
		if !creating && b.Secret == nil && t.Secret != "" && !notify.SameOrigin(t.URL, *b.URL) {
			return notify.Refuse("secret", notify.ReasonSecretAgain, "enter the secret again for the new address, or clear it")
		}
		t.URL = *b.URL
	}
	if b.Secret != nil {
		t.Secret = *b.Secret
	}
	if b.Enabled != nil {
		t.Enabled = *b.Enabled
	}
	if b.Events != nil {
		t.Events = *b.Events
	}
	if t.Events == nil {
		t.Events = []string{}
	}
	return notify.Clean(&t.Kind, &t.Name, &t.URL, &t.Secret, &t.Events)
}

// targetDetails is a destination's change for the audit log: which fields, never
// the address or secret themselves.
func targetDetails(b targetBody, t catalog.NotifyTarget) map[string]any {
	d := map[string]any{"kind": t.Kind}
	if b.URL != nil {
		d["address_changed"] = true
	}
	if b.Secret != nil {
		d["secret_changed"] = true
	}
	if b.Enabled != nil {
		d["enabled"] = t.Enabled
	}
	if b.Events != nil {
		d["events"] = t.Events
	}
	return d
}

// handleCreateNotifyTarget adds a destination (admin only): POST /admin/notifications
// {kind, name, url, secret?, enabled? (default on), events}. 400 invalid_target +
// field; 409 too_many_targets.
func (a *API) handleCreateNotifyTarget(w http.ResponseWriter, r *http.Request) {
	var body targetBody
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	t := catalog.NotifyTarget{Enabled: true}
	if err := body.apply(&t, true); err != nil {
		a.writeCatalogError(w, err, "notify: create", "could not save the destination")
		return
	}
	created, err := a.cat.CreateNotifyTarget(r.Context(), t)
	if err != nil {
		a.writeCatalogError(w, err, "notify: create", "could not save the destination")
		return
	}
	a.audit(r, "notify.create", created.Name, map[string]any{"kind": created.Kind, "events": created.Events})
	writeJSON(w, http.StatusCreated, targetView(*created))
}

// handleUpdateNotifyTarget changes a destination (admin only): PATCH
// /admin/notifications/{id} with the fields to change (not kind).
func (a *API) handleUpdateNotifyTarget(w http.ResponseWriter, r *http.Request) {
	var body targetBody
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	t := a.loadTarget(w, r)
	if t == nil {
		return
	}
	if err := body.apply(t, false); err != nil {
		a.writeCatalogError(w, err, "notify: update", "could not save the destination")
		return
	}
	saved, err := a.cat.SaveNotifyTarget(r.Context(), *t)
	if err != nil {
		a.writeTargetError(w, err, "notify: update")
		return
	}
	a.audit(r, "notify.update", saved.Name, targetDetails(body, *saved))
	writeJSON(w, http.StatusOK, targetView(*saved))
}

// handleDeleteNotifyTarget removes a destination (admin only): DELETE
// /admin/notifications/{id}.
func (a *API) handleDeleteNotifyTarget(w http.ResponseWriter, r *http.Request) {
	t := a.loadTarget(w, r)
	if t == nil {
		return
	}
	if err := a.cat.DeleteNotifyTarget(r.Context(), t.ID); err != nil {
		a.writeTargetError(w, err, "notify: delete")
		return
	}
	a.audit(r, "notify.delete", t.Name, map[string]any{"kind": t.Kind})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestNotifyTarget sends a test message now (admin only): POST
// /admin/notifications/{id}/test answers {ok, error} ("timeout", "unreachable",
// "http_<status>", "failed") and the destination with the outcome recorded.
func (a *API) handleTestNotifyTarget(w http.ResponseWriter, r *http.Request) {
	t := a.loadTarget(w, r)
	if t == nil {
		return
	}
	reason := a.rt.Notify.Test(r.Context(), *t)
	t, err := a.cat.GetNotifyTarget(r.Context(), t.ID) // with the outcome Test recorded
	if err != nil {
		a.writeTargetError(w, err, "notify: get")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": reason == "", "error": reason, "target": targetView(*t)})
}

// loadTarget reads the destination the path's {id} names, answering 400/404 itself
// (nil means the handler is done).
func (a *API) loadTarget(w http.ResponseWriter, r *http.Request) *catalog.NotifyTarget {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return nil
	}
	t, err := a.cat.GetNotifyTarget(r.Context(), id)
	if err != nil {
		a.writeTargetError(w, err, "notify: get")
		return nil
	}
	return t
}

func (a *API) writeTargetError(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such destination")
		return
	}
	a.writeCatalogError(w, err, op, "could not load the destination")
}

// handleServerEvents lists the event feed, newest first (admin only): GET
// /admin/events?before=&limit= (<= 100)&kind= (one of notify.Kinds, else 400).
func (a *API) handleServerEvents(w http.ResponseWriter, r *http.Request) {
	before, ok := parseOptionalID(r.URL.Query().Get("before"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid before")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && !slices.Contains(notify.Kinds, kind) {
		writeError(w, http.StatusBadRequest, "invalid kind")
		return
	}
	events, next, err := a.cat.ListServerEvents(r.Context(), before, queryInt(r, "limit", 20), kind)
	if err != nil {
		a.writeCatalogError(w, err, "events: list", "could not load the notifications")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_before": next})
}

// NotifyIdentity is how notifications name the server and where their links point:
// the live display name and public address.
func (a *API) NotifyIdentity() notify.Server {
	c := a.config()
	return notify.Server{Name: c.DisplayName(), PublicURL: c.PublicURL}
}
