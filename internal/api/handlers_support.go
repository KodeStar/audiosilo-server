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
	// Until is when a snooze POST just stored ends (RFC 3339); absent otherwise.
	Until string `json:"until,omitempty"`
}

// handleSupport says whether the support card shows now: GET /admin/support →
// {"show": bool}.
func (a *API) handleSupport(w http.ResponseWriter, r *http.Request) {
	show, err := a.cat.SupportCardDue(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "support: read", "could not read the support card")
		return
	}
	writeJSON(w, http.StatusOK, supportEnvelope{Show: show})
}

// handleSupportChoice records an admin's answer for the whole server: POST
// /admin/support {"action": "donated" | "snoozed"} → {"show": false, "until"?},
// with until only when a snooze was stored. After an answer the card is hidden
// either way, so nothing is recomputed. Taken on trust: nothing is checked and
// nothing leaves the server. 400 for any other action.
func (a *API) handleSupportChoice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	choice := catalog.SupportChoice(body.Action)
	res, err := a.cat.SetSupportChoice(r.Context(), choice)
	if err != nil {
		a.writeCatalogError(w, err, "support: save choice", "could not save the choice")
		return
	}
	out := supportEnvelope{}
	if res.Changed {
		details := map[string]any{"choice": string(choice)}
		if !res.Until.IsZero() {
			out.Until = res.Until.Format(time.RFC3339)
			details["returns_at"] = out.Until
		}
		a.audit(r, "settings.support", "", details)
	}
	writeJSON(w, http.StatusOK, out)
}
