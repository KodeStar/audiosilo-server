package config

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// patch builds a WithSettings argument from "section.name" -> JSON text.
func patch(kv map[string]string) map[string]map[string]json.RawMessage {
	out := map[string]map[string]json.RawMessage{}
	for id, v := range kv {
		section, name, _ := strings.Cut(id, ".")
		if out[section] == nil {
			out[section] = map[string]json.RawMessage{}
		}
		out[section][name] = json.RawMessage(v)
	}
	return out
}

func settingErr(t *testing.T, err error) *SettingError {
	t.Helper()
	var se *SettingError
	if !errors.As(err, &se) {
		t.Fatalf("want a *SettingError, got %v", err)
	}
	return se
}

func TestWithSettingsNormalizes(t *testing.T) {
	c := Default(t.TempDir())
	p := patch(map[string]string{
		"general.name":            `"  Hearthside  "`,
		"general.public_url":      `"https://books.example.com/"`,
		"network.tls_hosts":       `["Books.Example.com", "books.example.com", " "]`,
		"network.trusted_proxies": `["10.0.0.2", "fd00::1", "192.168.0.0/16"]`,
		"network.cors_origins":    `["HTTP://Localhost:8081/", "*"]`,
		"players.android_sha256":  `["` + strings.Repeat("ab:", 31) + `ab"]`,
		"players.android_package": `" com.example.app "`,
		"players.apple_app_ids":   `["ABCDE12345.com.example.app"]`,
		"demo.max_users":          `null`,
		"demo.idle_ttl":           `" 2h "`,
		"metadata.base_url":       `"https://meta.example.com/"`,
		"general.update_check":    `false`,
		"network.bind":            `":9000"`,
	})
	next, err := c.WithSettings(p, ok)
	if err != nil {
		t.Fatalf("WithSettings: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"name", next.Name, "Hearthside"},
		{"public_url", next.PublicURL, "https://books.example.com"},
		{"tls.hosts", next.TLS.Hosts, []string{"books.example.com"}},
		{"trusted_proxies", next.TrustedProxies, []string{"10.0.0.2/32", "fd00::1/128", "192.168.0.0/16"}},
		{"cors_origins", next.CORSOrigins, []string{"http://localhost:8081", "*"}},
		{"android_sha256", next.AppLinks.AndroidSHA256, []string{strings.Repeat("AB:", 31) + "AB"}},
		{"android_package", next.AppLinks.AndroidPackage, "com.example.app"},
		{"idle_ttl", next.Demo.IdleTTL, "2h"},
		{"base_url", next.Metadata.BaseURL, "https://meta.example.com"},
		{"update_check", next.UpdateCheck, false},
		{"bind", next.Bind, ":9000"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
	// The original is untouched (a save builds a new config).
	if c.Name != "" || len(c.TLS.Hosts) != 0 || !c.UpdateCheck || c.Bind != "0.0.0.0:8080" {
		t.Fatalf("WithSettings changed its receiver: %+v", c)
	}
}

func TestWithSettingsRefuses(t *testing.T) {
	cases := []struct {
		name, id, value, reason string
	}{
		{"long name", "general.name", `"` + strings.Repeat("x", MaxServerName+1) + `"`, ReasonInvalid},
		{"control char in name", "general.name", `"a\nb"`, ReasonInvalid},
		{"no scheme", "general.public_url", `"books.example.com"`, ReasonInvalid},
		{"query in url", "general.public_url", `"https://x.com/?a=1"`, ReasonInvalid},
		{"port out of range", "network.bind", `":70000"`, ReasonInvalid},
		{"no port", "network.bind", `"localhost"`, ReasonInvalid},
		{"bad tls mode", "network.tls_mode", `"sometimes"`, ReasonInvalid},
		{"host with scheme", "network.tls_hosts", `["https://x.com"]`, ReasonInvalid},
		{"bad cidr", "network.trusted_proxies", `["10.0.0.0/99"]`, ReasonInvalid},
		{"origin with path", "network.cors_origins", `["https://x.com/app"]`, ReasonInvalid},
		{"bad apple id", "players.apple_app_ids", `["com.example.app"]`, ReasonInvalid},
		{"bad package", "players.android_package", `"example"`, ReasonInvalid},
		{"bad fingerprint", "players.android_sha256", `["AB:CD"]`, ReasonInvalid},
		{"negative max users", "demo.max_users", `-1`, ReasonInvalid},
		{"bad ttl", "demo.idle_ttl", `"soon"`, ReasonInvalid},
		{"wrong type", "general.update_check", `"yes"`, ReasonInvalid},
		{"read only", "players.web_dir", `"/tmp"`, ReasonReadOnly},
		{"unknown", "general.colour", `"pink"`, ReasonUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Default(t.TempDir()).WithSettings(patch(map[string]string{tc.id: tc.value}), ok)
			se := settingErr(t, err)
			if se.Reason != tc.reason || se.Setting != tc.id {
				t.Fatalf("got %s/%s (%v), want %s/%s", se.Setting, se.Reason, se.Err, tc.id, tc.reason)
			}
		})
	}
}

// A setting that only fails together with another (autocert needs hosts) is
// reported on the setting Validate names.
func TestWithSettingsCrossFieldError(t *testing.T) {
	_, err := Default(t.TempDir()).WithSettings(patch(map[string]string{"network.tls_mode": `"autocert"`}), ok)
	if se := settingErr(t, err); se.Setting != "network.tls_hosts" || se.Reason != ReasonInvalid {
		t.Fatalf("got %s/%s, want network.tls_hosts/invalid", se.Setting, se.Reason)
	}
	next, err := Default(t.TempDir()).WithSettings(patch(map[string]string{
		"network.tls_mode": `"autocert"`, "network.tls_hosts": `["books.example.com"]`,
	}), ok)
	if err != nil || next.TLS.Mode != TLSAutocert {
		t.Fatalf("autocert with hosts: %v", err)
	}
}

// A value from an AUDIOSILO_* variable is locked in the console and never
// written into config.yaml, which keeps its own value.
func TestEnvSettingsLockedAndNotSaved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("public_url: https://file.example.com\nname: Den\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_PUBLIC_URL", "https://env.example.com")
	t.Setenv("AUDIOSILO_TLS_MODE", "off")
	t.Setenv("AUDIOSILO_UPDATE_CHECK", "not-a-bool") // ignored: not locked

	c, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://env.example.com" || c.TLS.Mode != TLSOff || !c.UpdateCheck {
		t.Fatalf("env not applied: %+v", c)
	}
	want := map[string]string{"general.public_url": "AUDIOSILO_PUBLIC_URL", "network.tls_mode": "AUDIOSILO_TLS_MODE"}
	if got := c.Locked(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Locked = %v, want %v", got, want)
	}
	_, err = c.WithSettings(patch(map[string]string{"general.public_url": `"https://x.com"`}), ok)
	if se := settingErr(t, err); se.Reason != ReasonLocked {
		t.Fatalf("an env setting must be locked, got %s", se.Reason)
	}

	next, err := c.WithSettings(patch(map[string]string{"general.name": `"Hearthside"`}), ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "name: Hearthside") || !strings.Contains(text, "public_url: https://file.example.com") ||
		strings.Contains(text, "env.example.com") || !strings.Contains(text, "mode: selfsigned") {
		t.Fatalf("config.yaml must keep its own values for env-set keys:\n%s", text)
	}
	if next.PublicURL != "https://env.example.com" {
		t.Fatal("saving must not change the running value")
	}
}

func TestPinnedSettingsLocked(t *testing.T) {
	c := Default(t.TempDir())
	c.Pin("bind")
	if got := c.Locked()["network.bind"]; got != PinnedByLauncher {
		t.Fatalf("pinned bind locked by %q, want %q", got, PinnedByLauncher)
	}
	_, err := c.WithSettings(patch(map[string]string{"network.bind": `":9000"`}), ok)
	if se := settingErr(t, err); se.Reason != ReasonLocked {
		t.Fatalf("a pinned setting must be locked, got %s", se.Reason)
	}
}

func TestRestartPending(t *testing.T) {
	running := Default(t.TempDir())
	next, err := running.WithSettings(patch(map[string]string{
		"network.bind": `":9000"`, "general.name": `"Den"`, "network.tls_hosts": `[]`,
	}), ok)
	if err != nil {
		t.Fatal(err)
	}
	// The name applies at once and an emptied list equals an unset one.
	if got := next.RestartPending(running); !reflect.DeepEqual(got, []string{"network.bind"}) {
		t.Fatalf("RestartPending = %v, want [network.bind]", got)
	}
	if got := running.RestartPending(running); len(got) != 0 {
		t.Fatalf("nothing pending against itself, got %v", got)
	}
	for _, id := range []string{"network.bind", "network.tls_mode", "players.web_dir", "metadata.base_url", "demo.enabled"} {
		if !strings.Contains(strings.Join(RestartSettings(), ","), id) {
			t.Errorf("%s must be a restart setting", id)
		}
	}
}

// update_check defaults on, also for a config.yaml written before it existed,
// and an explicit false survives a round trip.
func TestUpdateCheckDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("bind: 0.0.0.0:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := Load(dir)
	if err != nil || !c.UpdateCheck {
		t.Fatalf("an old config must default update_check on: %v %+v", err, c)
	}
	c.UpdateCheck = false
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if c2, _, err := Load(dir); err != nil || c2.UpdateCheck {
		t.Fatalf("update_check: false must persist: %v", err)
	}
}

func TestDisplayName(t *testing.T) {
	c := Default(t.TempDir())
	if c.DisplayName() != DefaultServerName {
		t.Fatalf("unnamed server = %q", c.DisplayName())
	}
	c.Name = "Den"
	if c.DisplayName() != "Den" {
		t.Fatalf("named server = %q", c.DisplayName())
	}
}

func TestSettingsListsNeverNull(t *testing.T) {
	s := Default(t.TempDir()).Settings()
	if v, ok := s["network"]["cors_origins"].([]string); !ok || v == nil {
		t.Fatalf("an unset list must read as [], got %#v", s["network"]["cors_origins"])
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"tls_hosts":null`) {
		t.Fatalf("null list on the wire: %s", b)
	}
}

// ok passes every check that needs the server (a metadata service, libraries).
var ok = Checks{MetadataAvailable: true}

// Checks that need the running server come back on their field too.
func TestWithSettingsChecks(t *testing.T) {
	c := Default(t.TempDir())
	c.Metadata.Enabled = false
	_, err := c.WithSettings(patch(map[string]string{"metadata.enabled": `true`}), Checks{})
	if se := settingErr(t, err); se.Setting != "metadata.enabled" || se.Reason != ReasonInvalid {
		t.Fatalf("enabling without a service: %s/%s", se.Setting, se.Reason)
	}
	exists := Checks{LibraryExists: func(name string) (bool, error) { return name == "Fiction", nil }}
	_, err = Default(t.TempDir()).WithSettings(patch(map[string]string{"demo.library": `"Nope"`}), exists)
	if se := settingErr(t, err); se.Setting != "demo.library" {
		t.Fatalf("unknown library: %s/%s", se.Setting, se.Reason)
	}
	if _, err := Default(t.TempDir()).WithSettings(patch(map[string]string{"demo.library": `"Fiction"`}), exists); err != nil {
		t.Fatalf("known library: %v", err)
	}
}

// The live config keeps restart settings as the server started.
func TestEffective(t *testing.T) {
	running := Default(t.TempDir())
	saved, err := running.WithSettings(patch(map[string]string{"network.bind": `":9000"`, "general.name": `"Den"`}), ok)
	if err != nil {
		t.Fatal(err)
	}
	eff := saved.Effective(running)
	if eff.Bind != running.Bind || eff.Name != "Den" {
		t.Fatalf("Effective = bind %q name %q", eff.Bind, eff.Name)
	}
}

// A launcher's pinned values reach config.yaml when the save creates it, and
// never again: a later console save keeps the file's own values.
func TestPinnedSaveKeepsFile(t *testing.T) {
	dir := t.TempDir()
	c, firstRun, err := Load(dir)
	if err != nil || !firstRun {
		t.Fatal(err)
	}
	c.Bind = "127.0.0.1:9000"
	c.Pin("bind")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := Load(dir); got.Bind != "127.0.0.1:9000" {
		t.Fatalf("the creating save records the pinned bind, got %q", got.Bind)
	}

	c2, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2.Bind = "127.0.0.1:9999" // the launcher passes a new one next start
	c2.PublicURL = "https://tunnel.example.com"
	c2.Pin("bind")
	c2.Pin("public_url")
	next, err := c2.WithSettings(patch(map[string]string{"general.name": `"Den"`}), ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Load(dir)
	if got.Name != "Den" || got.Bind != "127.0.0.1:9000" || got.PublicURL != "" {
		t.Fatalf("a console save must keep the file's pinned values: %+v", got)
	}
}
