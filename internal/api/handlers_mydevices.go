package api

import (
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// My devices (player redesign Phase 1b, capability "my_devices"): the caller's
// own signed-in sessions and API keys, and signing one out. The admin's device
// list (handleListDevices) covers everyone; this one only ever the caller.

// myDevice is an auth.Device as its owner sees it: no user id or username.
type myDevice struct {
	ID        int64            `json:"id"`
	Kind      string           `json:"kind"` // session | api
	Name      string           `json:"name"`
	Client    *auth.ClientInfo `json:"client"`
	CreatedAt string           `json:"created_at"`
	LastSeen  *string          `json:"last_seen"`
	LastIP    string           `json:"last_ip"`
	Current   bool             `json:"current"`
}

// handleListMyDevices answers the caller's live sessions and API keys, most
// recently seen first, marking the one making this request: {"devices": [...]}.
func (a *API) handleListMyDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := a.auth.ListDevices(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		a.log.Warn("list own devices failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not load devices")
		return
	}
	current := credentialFrom(r.Context()).ID
	out := make([]myDevice, len(devices))
	for i, d := range devices {
		out[i] = myDevice{ID: d.ID, Kind: d.Kind, Name: d.Name, Client: d.Client, CreatedAt: d.CreatedAt,
			LastSeen: d.LastSeen, LastIP: d.LastIP, Current: d.ID == current}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleRevokeMyDevice signs one of the caller's own devices out (auth.
// RevokeOwnDevice): {"current": bool}. The device making the request may be the
// one: its token is dead from the next request, and the client signs out locally.
// Anyone else's device, like an unknown or signed-out one, is 404 and untouched.
// An API-key caller may do it too (it only reduces access).
func (a *API) handleRevokeMyDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid device id")
		return
	}
	err := a.auth.RevokeOwnDevice(r.Context(), userFrom(r.Context()).ID, id)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "device not found")
	case err != nil:
		a.log.Warn("revoke own device failed", "err", err, "device", id)
		writeError(w, http.StatusInternalServerError, "could not sign the device out")
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"current": id == credentialFrom(r.Context()).ID})
	}
}
