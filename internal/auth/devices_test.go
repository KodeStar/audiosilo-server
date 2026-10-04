package auth

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestParseClient(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ClientInfo
		ok   bool
	}{
		{"AudioSilo/1.4.2 (ios)", ClientInfo{"AudioSilo", "1.4.2", "ios"}, true},
		{"AudioSilo Admin/0.1.0 (Web)", ClientInfo{"AudioSilo Admin", "0.1.0", "web"}, true},
		{"  AudioSilo/dev  ", ClientInfo{"AudioSilo", "dev", ""}, true},
		{"AudioSilo/1.0.0+build.7 (android)", ClientInfo{"AudioSilo", "1.0.0+build.7", "android"}, true},
		{"", ClientInfo{}, false},
		{"AudioSilo Admin (web)", ClientInfo{"AudioSilo Admin", "", "web"}, true},
		{"AudioSilo", ClientInfo{"AudioSilo", "", ""}, true},
		{"AudioSilo/", ClientInfo{}, false},
		{"AudioSilo/1.0 (ios", ClientInfo{}, false},                      // unbalanced
		{"<script>/1.0", ClientInfo{}, false},                            // markup
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X)", ClientInfo{}, false}, // a browser's UA is not a client name
		{"AudioSilo/1.0 (ios)\nX-Evil: 1", ClientInfo{}, false},
		{"A/" + string(make([]byte, 40)), ClientInfo{}, false},
	} {
		got, ok := ParseClient(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseClient(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolveRequestRecordsPresence(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "sam", "", RoleUser)
	secret, err := s.IssueToken(ctx, u.ID, KindSession, "Pixel 8", 0)
	if err != nil {
		t.Fatal(err)
	}
	ios := ClientInfo{App: "AudioSilo", Version: "1.4.2", Platform: "android"}
	_, cred, err := s.ResolveRequest(ctx, secret, Presence{IP: "192.0.2.7", Client: ios}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	if cred.ID == 0 || cred.Kind != KindSession || cred.DeviceName != "Pixel 8" || cred.Client != ios {
		t.Fatalf("credential = %+v", cred)
	}
	// A request without the header (an <audio> stream) keeps the stored app and
	// still reports it; a request with no address keeps the stored one.
	_, cred, err = s.ResolveRequest(ctx, secret, Presence{}, KindSession)
	if err != nil || cred.Client != ios {
		t.Fatalf("headerless request: %+v %v", cred, err)
	}
	devices, err := s.ListDevices(ctx, u.ID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices: %+v %v", devices, err)
	}
	d := devices[0]
	if d.Client == nil || *d.Client != ios || d.LastIP != "192.0.2.7" || d.LastSeen == nil || d.Name != "Pixel 8" || d.Username != "sam" {
		t.Fatalf("device = %+v", d)
	}
	// A newer build replaces the stored one.
	newer := ClientInfo{App: "AudioSilo", Version: "1.5.0", Platform: "android"}
	s.ResolveRequest(ctx, secret, Presence{IP: "192.0.2.8", Client: newer}, KindSession)
	devices, _ = s.ListDevices(ctx, u.ID)
	if *devices[0].Client != newer || devices[0].LastIP != "192.0.2.8" {
		t.Fatalf("device after update = %+v", devices[0])
	}
}

func TestListDevicesAndRevoke(t *testing.T) {
	s, ctx, now := newTestServiceWithClock(t)
	sam, _ := s.CreateUser(ctx, "sam", "", RoleUser)
	jo, _ := s.CreateUser(ctx, "jo", "", RoleUser)
	phone, _ := s.IssueToken(ctx, sam.ID, KindSession, "phone", 0)
	_, key, _ := s.IssueAPIToken(ctx, sam.ID, "dashboard")
	s.IssueToken(ctx, sam.ID, KindPairing, "", 0)                   // not a device
	s.IssueToken(ctx, sam.ID, KindSession, "expiring", time.Minute) // expires below
	gone, _ := s.IssueToken(ctx, sam.ID, KindSession, "gone", 0)
	s.RevokeToken(ctx, gone)
	s.IssueToken(ctx, jo.ID, KindSession, "jo's tablet", 0)
	*now = now.Add(time.Hour)

	devices, err := s.ListDevices(ctx, sam.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, d := range devices {
		names[d.Name] = d.Kind
	}
	if len(devices) != 2 || names["phone"] != KindSession || names["dashboard"] != KindAPI {
		t.Fatalf("sam's devices = %+v", devices)
	}
	all, _ := s.ListDevices(ctx, 0)
	if len(all) != 3 {
		t.Fatalf("everyone's devices = %d, want 3", len(all))
	}

	// Allowed: signing out one device leaves the others.
	if err := s.RevokeDevice(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveToken(ctx, phone, KindSession); err != nil {
		t.Fatalf("the phone should stay signed in: %v", err)
	}
	devices, _ = s.ListDevices(ctx, sam.ID)
	if len(devices) != 1 || devices[0].Name != "phone" {
		t.Fatalf("after sign-out: %+v", devices)
	}
	// Denied: already revoked, unknown, or a pairing token.
	var pairingID int64
	s.db.QueryRowContext(ctx, `SELECT id FROM tokens WHERE kind = 'pairing'`).Scan(&pairingID)
	for _, id := range []int64{key.ID, 9999, pairingID} {
		if err := s.RevokeDevice(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("RevokeDevice(%d) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestForgetRevokedAddresses(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "sam", "", RoleUser)
	live, _ := s.IssueToken(ctx, u.ID, KindSession, "live", 0)
	gone, _ := s.IssueToken(ctx, u.ID, KindSession, "gone", 0)
	expired, _ := s.IssueToken(ctx, u.ID, KindSession, "expired", time.Hour)
	for _, tok := range []string{live, gone, expired} {
		if _, _, err := s.ResolveRequest(ctx, tok, Presence{IP: "192.0.2.9"}, KindSession); err != nil {
			t.Fatal(err)
		}
	}
	s.RevokeToken(ctx, gone)
	later := time.Now().Add(2 * time.Hour) // past the expired token's hour
	s.now = func() time.Time { return later }
	if err := s.ForgetRevokedAddresses(ctx); err != nil {
		t.Fatal(err)
	}
	ips := map[string]string{}
	rows, _ := s.db.QueryContext(ctx, `SELECT device_name, last_ip FROM tokens`)
	defer rows.Close()
	for rows.Next() {
		var name, ip string
		rows.Scan(&name, &ip)
		ips[name] = ip
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if ips["live"] != "192.0.2.9" || ips["gone"] != "" || ips["expired"] != "" {
		t.Fatalf("addresses = %v: a signed-in device keeps its address, a signed-out or expired one loses it", ips)
	}
}

// TestResolveRequestHeaderlessDoesNotRevertApp: a request without the header (a
// cover or stream) racing one that names a new build must not write the old app
// back. The decision is made in SQL, so the race can't revert it.
func TestResolveRequestHeaderlessDoesNotRevertApp(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "sam", "", RoleUser)
	tok, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	for i := 0; i < 200; i++ {
		want := ClientInfo{App: "AudioSilo", Version: "1." + strconv.Itoa(i), Platform: "ios"}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.ResolveRequest(ctx, tok, Presence{}, KindSession) }()
		go func() { defer wg.Done(); s.ResolveRequest(ctx, tok, Presence{Client: want}, KindSession) }()
		wg.Wait()
		devices, err := s.ListDevices(ctx, u.ID)
		if err != nil || len(devices) != 1 || devices[0].Client == nil || *devices[0].Client != want {
			t.Fatalf("round %d: stored app = %+v, want %+v (%v)", i, devices[0].Client, want, err)
		}
	}
}
