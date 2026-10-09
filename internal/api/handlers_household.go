package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Listening in the house (capability household): a person's own opt-in and what
// the other people who share are listening to. The rules (reciprocal, other
// enabled non-demo people only, the caller's scopes, no device fields) live in
// catalog.HouseListening.

// handleGetHousehold answers GET /me/household: {share_listening}.
func (a *API) handleGetHousehold(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	on, err := a.cat.HouseholdSharing(r.Context(), u.ID)
	if err != nil {
		a.writeCatalogError(w, err, "load household setting failed", "could not load your household setting", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, catalog.HouseholdSettings{ShareListening: on})
}

// handlePutHousehold sets the caller's choice from {share_listening: bool} (the
// field is required) and answers as GET. Refused for demo accounts.
func (a *API) handlePutHousehold(w http.ResponseWriter, r *http.Request) {
	u := a.nonDemoAccount(w, r)
	if u == nil {
		return
	}
	var body struct {
		ShareListening *bool `json:"share_listening"`
	}
	if err := decodeJSON(r, &body, 0); err != nil || body.ShareListening == nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.cat.SetHouseholdSharing(r.Context(), u.ID, *body.ShareListening); err != nil {
		a.writeCatalogError(w, err, "save household setting failed", "could not save your household setting", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, catalog.HouseholdSettings{ShareListening: *body.ShareListening})
}

// handleHouseListening answers GET /household/listening: {sharing, listening}.
func (a *API) handleHouseListening(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	scopes, ok := a.callerScopes(w, r, "could not load the house")
	if !ok {
		return
	}
	house, err := a.cat.HouseListening(r.Context(), u.ID, scopes)
	if err != nil {
		a.writeCatalogError(w, err, "house listening failed", "could not load the house", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, house)
}
