package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// myDevicesOf decodes GET /me/devices for tok, failing on anything but 200.
func myDevicesOf(t *testing.T, e *testEnv, tok string) ([]myDevice, string) {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/me/devices", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me/devices = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Devices []myDevice `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Devices, body
}

func TestMyDevices(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	sam, err := e.auth.CreateUser(ctx, "sam", "sam-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(user int64, kind, name string) string {
		t.Helper()
		tok, err := e.auth.IssueToken(ctx, user, kind, name, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	phone, laptop, key := issue(sam.ID, auth.KindSession, "Phone"), issue(sam.ID, auth.KindSession, "Laptop"), issue(sam.ID, auth.KindAPI, "Script")
	adminTok := issue(e.adminID, auth.KindSession, "Admin phone")
	// The phone names its app.
	if resp, _ := e.doHeaders(t, "GET", "/api/v1/me", phone, "", map[string]string{auth.ClientHeader: "AudioSilo/1.4.2 (ios)"}); resp.StatusCode != http.StatusOK {
		t.Fatal("phone request")
	}

	devices, body := myDevicesOf(t, e, phone)
	names := map[string]myDevice{}
	for _, d := range devices {
		names[d.Name] = d
	}
	if len(devices) != 3 || names["Phone"].Kind != auth.KindSession || !names["Phone"].Current || names["Phone"].Client == nil ||
		names["Phone"].Client.Version != "1.4.2" || names["Laptop"].Current || names["Laptop"].Client != nil ||
		names["Script"].Kind != auth.KindAPI || names["Script"].Current {
		t.Fatalf("sam's devices = %s", body)
	}
	// The owner's shape: no user id or username, never another user's device.
	if strings.Contains(body, `"user_id"`) || strings.Contains(body, `"username"`) || strings.Contains(body, "Admin phone") {
		t.Fatalf("devices leak an account or someone else's device: %s", body)
	}
	var raw struct {
		Devices []map[string]json.RawMessage `json:"devices"`
	}
	_ = json.Unmarshal([]byte(body), &raw)
	keys := make([]string, 0, len(raw.Devices[0]))
	for k := range raw.Devices[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if strings.Join(keys, ",") != "client,created_at,current,id,kind,last_ip,last_seen,name" {
		t.Fatalf("MyDevice keys = %v", keys)
	}
	admins, _ := myDevicesOf(t, e, adminTok)
	if len(admins) != 1 || admins[0].Name != "Admin phone" {
		t.Fatalf("the admin's own devices = %+v", admins)
	}

	del := func(tok string, id int64) (int, string) {
		t.Helper()
		resp, body := e.do(t, "DELETE", "/api/v1/me/devices/"+strconv.FormatInt(id, 10), tok, "")
		return resp.StatusCode, body
	}

	// Denied: another user's device is 404 and stays signed in (even for an admin
	// caller: this is the owner's route, not the console's).
	adminDevice := admins[0].ID
	if status, body := del(phone, adminDevice); status != http.StatusNotFound || !strings.Contains(body, "device not found") {
		t.Fatalf("revoke another user's device = %d %s, want 404", status, body)
	}
	if status, _ := del(adminTok, names["Laptop"].ID); status != http.StatusNotFound {
		t.Fatalf("an admin revoking sam's device here = %d, want 404", status)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatal("a refused revoke signed the admin out")
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", laptop, ""); resp.StatusCode != http.StatusOK {
		t.Fatal("a refused revoke signed sam's laptop out")
	}
	if status, _ := del(phone, 999999); status != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", status)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/me/devices/abc", phone, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me/devices", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", resp.StatusCode)
	}

	// Allowed: an own other device (here from an API key, which may only reduce access).
	if status, body := del(key, names["Laptop"].ID); status != http.StatusOK || strings.TrimSpace(body) != `{"current":false}` {
		t.Fatalf("revoke own laptop = %d %s", status, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", laptop, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked laptop = %d, want 401", resp.StatusCode)
	}
	if status, _ := del(phone, names["Laptop"].ID); status != http.StatusNotFound {
		t.Fatalf("already revoked = %d, want 404", status)
	}

	// Allowed: the current device; its token is dead for the next request.
	if status, body := del(phone, names["Phone"].ID); status != http.StatusOK || strings.TrimSpace(body) != `{"current":true}` {
		t.Fatalf("revoke the current device = %d %s", status, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me/devices", phone, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the revoked current device's next request = %d, want 401", resp.StatusCode)
	}
	if left, _ := myDevicesOf(t, e, key); len(left) != 1 || left[0].Name != "Script" || !left[0].Current {
		t.Fatalf("devices left = %+v", left)
	}
}
