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

// A browser is known once one of the person's sessions came from it: a sign-out
// keeps it known, an admin's "sign out this device" forgets it, and the key is
// per person (another account signing in from the same browser is new to it).
func TestIssueSessionKnowsTheBrowser(t *testing.T) {
	s, ctx := newTestService(t)
	ann, _ := s.CreateUser(ctx, "ann", "a-long-password", RoleAdmin)
	bob, _ := s.CreateUser(ctx, "bob", "a-long-password", RoleUser)
	const key = "8c4a2f1e-0b9d-4c57-9e1a-3f6d2b7c8e90"

	known := func(user *User, key string) bool {
		t.Helper()
		secret, known, err := s.IssueSession(ctx, user.ID, "admin-web", key)
		if err != nil || secret == "" {
			t.Fatalf("IssueSession: %q %v", secret, err)
		}
		return known
	}
	if known(ann, key) {
		t.Fatal("a first sign-in from a browser is known")
	}
	if !known(ann, key) {
		t.Fatal("a second sign-in from the same browser is new")
	}
	if known(bob, key) {
		t.Fatal("another person's browser key counts for this one")
	}
	// Twice each: a sign-in without a usable key never makes the next one known.
	for _, unusable := range []string{"", "", "short", "short", "not a key; has spaces!", "not a key; has spaces!"} {
		if known(ann, unusable) {
			t.Fatalf("a sign-in with key %q is known", unusable)
		}
	}

	// Signing out keeps the browser known.
	secret, _, _ := s.IssueSession(ctx, ann.ID, "admin-web", key)
	if err := s.RevokeToken(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if !known(ann, key) {
		t.Fatal("a sign-out forgot the browser")
	}

	// An admin signing one of its sessions out forgets it on every session.
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM tokens WHERE user_id = ? AND sign_in_key <> ''`, ann.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(ctx, id); err != nil {
		t.Fatal(err)
	}
	if known(ann, key) {
		t.Fatal("a browser an admin signed out is still known")
	}
	if !known(bob, key) {
		t.Fatal("signing out ann's browser forgot bob's")
	}
	// A new password, or disabling the account, forgets every browser of that person.
	if !known(ann, key) {
		t.Fatal("setup: the browser should be known again")
	}
	if err := s.SetPassword(ctx, ann.ID, "another-long-password"); err != nil {
		t.Fatal(err)
	}
	if known(ann, key) {
		t.Fatal("a browser is still known after a new password")
	}
	if err := s.SetDisabled(ctx, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, bob.ID, false); err != nil {
		t.Fatal(err)
	}
	if known(bob, key) {
		t.Fatal("a browser is still known after the account was disabled")
	}

	// The key is never stored as sent.
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE sign_in_key = ?`, key).Scan(&n)
	if n != 0 {
		t.Fatal("the browser key is stored in the clear")
	}
}

// TestResolveRequestSkipsUnchangedTouch: requests within a minute of the last
// write, reporting nothing new, don't write the token row; a new app, a new
// address or a minute passing does. Validity is still checked on every request.
func TestResolveRequestSkipsUnchangedTouch(t *testing.T) {
	s, ctx, now := newTestServiceWithClock(t)
	*now = now.Truncate(time.Second) // last_seen is stored to the second
	u, _ := s.CreateUser(ctx, "sam", "", RoleUser)
	tok, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	app := ClientInfo{App: "AudioSilo", Version: "1.4.2", Platform: "ios"}
	row := func() (lastSeen string, client ClientInfo, ip string) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(last_seen, ''), client_app, client_version, client_platform, last_ip FROM tokens WHERE kind = 'session'`).
			Scan(&lastSeen, &client.App, &client.Version, &client.Platform, &ip); err != nil {
			t.Fatal(err)
		}
		return
	}
	resolve := func(p Presence) {
		t.Helper()
		if _, _, err := s.ResolveRequest(ctx, tok, p, KindSession); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func() string { return now.UTC().Format(time.RFC3339) }

	resolve(Presence{IP: "192.0.2.1", Client: app}) // the first request writes
	first := stamp()
	if seen, _, _ := row(); seen != first {
		t.Fatalf("first request: last_seen %q, want %q", seen, first)
	}
	// Within the minute, the same app and address (or none): no write.
	*now = now.Add(30 * time.Second)
	resolve(Presence{IP: "192.0.2.1", Client: app})
	resolve(Presence{})
	if seen, _, _ := row(); seen != first {
		t.Fatalf("an unchanged request within a minute wrote last_seen %q", seen)
	}
	// A new build is written at once.
	newer := ClientInfo{App: "AudioSilo", Version: "1.5.0", Platform: "ios"}
	resolve(Presence{IP: "192.0.2.1", Client: newer})
	if seen, client, _ := row(); seen != stamp() || client != newer {
		t.Fatalf("a changed client header: last_seen %q, client %+v", seen, client)
	}
	// So is a new address.
	*now = now.Add(10 * time.Second)
	resolve(Presence{IP: "192.0.2.2"})
	if seen, client, ip := row(); seen != stamp() || ip != "192.0.2.2" || client != newer {
		t.Fatalf("a changed address: last_seen %q, ip %q, client %+v", seen, ip, client)
	}
	// A minute after the last write, an unchanged request writes again.
	written := stamp()
	*now = now.Add(59 * time.Second)
	resolve(Presence{IP: "192.0.2.2"})
	if seen, _, _ := row(); seen != written {
		t.Fatalf("59 s later: last_seen %q, want %q", seen, written)
	}
	*now = now.Add(time.Second)
	resolve(Presence{IP: "192.0.2.2"})
	if seen, _, _ := row(); seen != stamp() {
		t.Fatalf("a minute later: last_seen %q, want %q", seen, stamp())
	}
	// Denied: skipping the write never skips the check. A token revoked a moment
	// after it was seen is refused at once.
	if err := s.RevokeToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, tok, Presence{IP: "192.0.2.2"}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token within the minute = %v, want ErrInvalidToken", err)
	}
}
