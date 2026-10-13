package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Listening in the house (capability household): a person's own opt-in and what
// the other people who share are listening to. The rules (reciprocal, other
// enabled non-demo people only, the caller's scopes, no device fields) live in
// catalog.HouseListening.

// householdSettings is GET/PUT /me/household's answer: the person's own choice.
type householdSettings struct {
	ShareListening bool `json:"share_listening"`
}

// houseListening is GET /household/listening's answer: whether the caller
// shares, and (only then) what the other people who share are listening to,
// newest first.
type houseListening struct {
	Sharing   bool                        `json:"sharing"`
	Listening []catalog.HouseListeningRow `json:"listening"`
}

// handleGetHousehold answers GET /me/household: {share_listening}.
func (a *API) handleGetHousehold(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	on, err := a.cat.HouseholdSharing(r.Context(), u.ID)
	if err != nil {
		a.writeCatalogError(w, err, "load household setting failed", "could not load your household setting", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, householdSettings{ShareListening: on})
}

// handlePutHousehold sets the caller's choice from {share_listening: bool} (the
// field is required) and answers as GET. Refused for demo accounts, and for kids
// before it (notKid: a kid never shares, and has no row to read).
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
	err := a.cat.SetHouseholdSharing(r.Context(), u.ID, *body.ShareListening)
	if writeKidRefusal(w, err) {
		return
	}
	if err != nil {
		a.writeCatalogError(w, err, "save household setting failed", "could not save your household setting", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, householdSettings{ShareListening: *body.ShareListening})
}

// handleHouseListening answers GET /household/listening: {sharing, listening}.
// A caller who doesn't share gets {false, []} before any scope is loaded (the
// player polls it every minute).
func (a *API) handleHouseListening(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	sharing, err := a.cat.HouseholdSharing(r.Context(), u.ID)
	if err != nil {
		a.writeCatalogError(w, err, "house listening failed", "could not load the house", "user", u.ID)
		return
	}
	if !sharing {
		writeJSON(w, http.StatusOK, houseListening{Listening: []catalog.HouseListeningRow{}})
		return
	}
	scopes, ok := a.callerScopes(w, r, "could not load the house")
	if !ok {
		return
	}
	rows, err := a.cat.HouseListening(r.Context(), u.ID, scopes)
	if err != nil {
		a.writeCatalogError(w, err, "house listening failed", "could not load the house", "user", u.ID)
		return
	}
	writeJSON(w, http.StatusOK, houseListening{Sharing: true, Listening: rows})
}
