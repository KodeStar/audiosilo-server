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
// MemberSignIns (household_members) names the caller's household members signed
// in on that device ([] for none); only GET /me/devices carries it, a member's
// device list (handleMemberDevices) leaves it out.
type myDevice struct {
	ID            int64                `json:"id"`
	Kind          string               `json:"kind"` // session | api
	Name          string               `json:"name"`
	Client        *auth.ClientInfo     `json:"client"`
	CreatedAt     string               `json:"created_at"`
	LastSeen      *string              `json:"last_seen"`
	LastIP        string               `json:"last_ip"`
	Current       bool                 `json:"current"`
	MemberSignIns *[]auth.MemberSignIn `json:"member_sign_ins,omitempty"`
}

// toMyDevice is d as its owner sees it, current when it is the token current.
func toMyDevice(d auth.Device, current int64) myDevice {
	return myDevice{ID: d.ID, Kind: d.Kind, Name: d.Name, Client: d.Client, CreatedAt: d.CreatedAt,
		LastSeen: d.LastSeen, LastIP: d.LastIP, Current: d.ID == current}
}

// handleListMyDevices answers the caller's live sessions and API keys, most
// recently seen first, marking the one making this request, each with the
// caller's members signed in on it: {"devices": [...]}.
func (a *API) handleListMyDevices(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	devices, err := a.auth.ListDevices(r.Context(), u.ID)
	if err != nil {
		a.log.Warn("list own devices failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not load devices")
		return
	}
	signIns, err := a.auth.MemberSignIns(r.Context(), u.ID)
	if err != nil {
		a.log.Warn("list member sign-ins failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not load devices")
		return
	}
	current := credentialFrom(r.Context()).ID
	out := make([]myDevice, len(devices))
	for i, d := range devices {
		out[i] = toMyDevice(d, current)
		ins := signIns[d.ID]
		if ins == nil {
			ins = []auth.MemberSignIn{}
		}
		out[i].MemberSignIns = &ins
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleRevokeMyDevice signs one of the caller's own devices out (auth.
// RevokeOwnDeviceAndMembers): {"current": bool, "members_signed_out": int}. The
// device making the request may be the one: its token is dead from the next
// request, and the client signs out locally. Anyone else's device, like an
// unknown or signed-out one, is 404 and untouched. With ?also_members=1 (or
// true) the caller's household members signed in on that device are signed out
// there too, in the same transaction. An API-key caller may do it too (it only
// reduces access).
func (a *API) handleRevokeMyDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid device id")
		return
	}
	also := r.URL.Query().Get("also_members")
	n, err := a.auth.RevokeOwnDeviceAndMembers(r.Context(), userFrom(r.Context()).ID, id, also == "1" || also == "true")
	switch {
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "device not found")
	case err != nil:
		a.log.Warn("revoke own device failed", "err", err, "device", id)
		writeError(w, http.StatusInternalServerError, "could not sign the device out")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"current": id == credentialFrom(r.Context()).ID, "members_signed_out": n})
	}
}
