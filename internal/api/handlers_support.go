package api

import (
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The admin console's support card (catalog/support.go decides when it shows).
// Admin only, and only the console asks: the player never sees it.

// supportEnvelope is GET /admin/support's answer, and POST's.
type supportEnvelope struct {
	Show bool `json:"show"`
}

// handleSupport says whether the support card shows now: GET /admin/support →
// {"show": bool}.
func (a *API) handleSupport(w http.ResponseWriter, r *http.Request) {
	a.writeSupport(w, r)
}

// supportActions maps POST /admin/support's action to the choice it stores.
var supportActions = map[string]catalog.SupportChoice{
	"donated": catalog.SupportDonated,
	"snooze":  catalog.SupportSnoozed,
}

// handleSupportChoice records an admin's answer for the whole server: POST
// /admin/support {"action": "donated" | "snooze"} → {"show": false}. Taken on
// trust: nothing is checked and nothing leaves the server. 400 for any other action.
func (a *API) handleSupportChoice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	choice, ok := supportActions[body.Action]
	if !ok {
		writeError(w, http.StatusBadRequest, `action must be "donated" or "snooze"`)
		return
	}
	res, err := a.cat.SetSupportChoice(r.Context(), choice)
	if err != nil {
		a.log.Error("support: save choice failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save the choice")
		return
	}
	if res.Changed {
		details := map[string]any{"choice": string(choice)}
		if !res.Until.IsZero() {
			details["returns_at"] = res.Until.Format(time.RFC3339)
		}
		a.audit(r, "settings.support", "", details)
	}
	a.writeSupport(w, r)
}

func (a *API) writeSupport(w http.ResponseWriter, r *http.Request) {
	show, err := a.cat.SupportCardDue(r.Context())
	if err != nil {
		a.log.Error("support: read failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read the support card")
		return
	}
	writeJSON(w, http.StatusOK, supportEnvelope{Show: show})
}
