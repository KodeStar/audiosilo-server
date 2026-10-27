package config

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
		"metadata.region":         `" UK "`,
		"metadata.mode":           `" Mirror "`,
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
		{"region", next.Metadata.Region, "uk"},
		{"mode", next.Metadata.Mode, "mirror"},
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
		{"unknown marketplace", "metadata.region", `"gb"`, ReasonInvalid},
		{"unknown metadata mode", "metadata.mode", `"local"`, ReasonInvalid},
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
	// The listener reads tls.home_http once, at start.
	homeOff, err := running.WithSettings(patch(map[string]string{"network.tls_home_http": `false`}), ok)
	if err != nil {
		t.Fatal(err)
	}
	if got := homeOff.RestartPending(running); !reflect.DeepEqual(got, []string{"network.tls_home_http"}) {
		t.Fatalf("RestartPending = %v, want [network.tls_home_http]", got)
	}
	for _, id := range []string{"network.bind", "network.tls_mode", "network.tls_home_http", "players.web_dir", "metadata.base_url", "metadata.mode", "demo.enabled"} {
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

func TestChangedSettings(t *testing.T) {
	cur := Default(t.TempDir())
	next := cur.Clone()
	next.Name = "Den"
	next.Backups.Keep = 14
	next.CORSOrigins = []string{"http://localhost:8081"}
	got := ChangedSettings(cur, next)
	want := []SettingChange{
		{Setting: "backups.keep", From: 7, To: 14},
		{Setting: "general.name", From: "", To: "Den"},
		{Setting: "network.cors_origins", From: []string{}, To: []string{"http://localhost:8081"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %#v", got)
	}
	if got := ChangedSettings(cur, cur.Clone()); len(got) != 0 {
		t.Fatalf("no change listed %#v", got)
	}
}

// An emptied field arrives as null: refused for a setting that can't be unset,
// taken (the default) by one that can.
func TestNullOnlyForUnsettable(t *testing.T) {
	c := Default(t.TempDir())
	_, err := c.WithSettings(map[string]map[string]json.RawMessage{"backups": {"keep": json.RawMessage(`null`)}}, Checks{})
	var se *SettingError
	if !errors.As(err, &se) || se.Setting != "backups.keep" || se.Reason != ReasonInvalid {
		t.Fatalf("null keep: err = %v", err)
	}
	n := 5
	c.Demo.MaxUsers = &n
	next, err := c.WithSettings(map[string]map[string]json.RawMessage{"demo": {"max_users": json.RawMessage(`null`)}}, Checks{})
	if err != nil || next.Demo.MaxUsers != nil {
		t.Fatalf("null max_users: %v, %v", next, err)
	}
}

// A change must leave config.yaml valid on its own, since the file keeps its own
// values for what the environment sets: turning demo mode on while its library
// comes only from AUDIOSILO_DEMO_LIBRARY would save a file that stops the server
// starting once the variable is gone. A file that already leans on the
// environment doesn't block unrelated changes.
func TestWithSettingsKeepsTheFileValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("server_id: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_DEMO_LIBRARY", "Books")
	c, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.WithSettings(patch(map[string]string{"demo.enabled": `true`}), Checks{})
	se := settingErr(t, err)
	if se.Setting != "demo.enabled" || se.Reason != ReasonInvalid || !strings.Contains(se.Error(), "AUDIOSILO_DEMO_LIBRARY") {
		t.Fatalf("demo on with its library only in the environment = %+v (%v)", se, se)
	}
	if _, err := c.WithSettings(patch(map[string]string{"general.name": `"Den"`}), Checks{}); err != nil {
		t.Fatalf("an unrelated change: %v", err)
	}

	// The file already turns demo on and the variable supplies the library.
	if err := os.WriteFile(Path(dir), []byte("server_id: x\ndemo:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WithSettings(patch(map[string]string{"general.name": `"Den"`}), Checks{}); err != nil {
		t.Fatalf("a change to a file that already leans on the environment: %v", err)
	}
}

// How long raw listening sessions are kept: 400 days unless set, bounded, and set
// from the console or AUDIOSILO_SESSION_DAYS (which then locks it).
func TestSessionDaysSetting(t *testing.T) {
	c := Default(t.TempDir())
	if got := c.Settings()["general"]["session_days"]; got != DefaultSessionDays {
		t.Fatalf("default = %v", got)
	}
	for _, bad := range []string{`10`, `4000`, `"90"`} {
		if _, err := c.WithSettings(patch(map[string]string{"general.session_days": bad}), Checks{}); settingErr(t, err).Setting != "general.session_days" {
			t.Fatalf("%s accepted", bad)
		}
	}
	next, err := c.WithSettings(patch(map[string]string{"general.session_days": `90`}), Checks{})
	if err != nil || next.Activity.SessionRetention() != 90*24*time.Hour {
		t.Fatalf("90 days = %v, %v", next, err)
	}

	dir := t.TempDir()
	t.Setenv("AUDIOSILO_SESSION_DAYS", "180")
	c, _, err = Load(dir)
	if err != nil || c.Activity.SessionDays != 180 || c.Locked()["general.session_days"] != "AUDIOSILO_SESSION_DAYS" {
		t.Fatalf("from the environment = %+v %v %v", c.Activity, c.Locked(), err)
	}
}

// metadata.mode defaults to remote, also for a config.yaml written before it
// existed; the environment sets it (any case), and an unknown value is a config
// error naming the allowed ones.
func TestMetadataMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("bind: 0.0.0.0:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := Load(dir)
	if err != nil || c.Metadata.Mirror() || c.Metadata.ModeName() != MetadataRemote {
		t.Fatalf("an old config must read as remote: %v %+v", err, c.Metadata)
	}
	if (MetadataConfig{}).ModeName() != MetadataRemote {
		t.Fatal(`an empty mode must read as remote`)
	}

	t.Setenv("AUDIOSILO_METADATA_MODE", "Mirror")
	c, _, err = Load(dir)
	if err != nil || !c.Metadata.Mirror() {
		t.Fatalf("AUDIOSILO_METADATA_MODE=Mirror must select mirror mode: %v %+v", err, c.Metadata)
	}
	if got := c.Locked()["metadata.mode"]; got != "AUDIOSILO_METADATA_MODE" {
		t.Fatalf("metadata.mode locked by %q, want the variable", got)
	}

	t.Setenv("AUDIOSILO_METADATA_MODE", "local")
	_, _, err = Load(dir)
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Key != "metadata.mode" || !strings.Contains(err.Error(), "remote, mirror") {
		t.Fatalf("an unknown mode must be a metadata.mode error naming the values, got %v", err)
	}
}

// Household members: on with 6 per person unless set; max_members is 1 to 50,
// set from the console or AUDIOSILO_HOUSEHOLD_MAX_MEMBERS (which then locks it),
// and a config.yaml written before the section keeps the defaults.
func TestHouseholdSettings(t *testing.T) {
	c := Default(t.TempDir())
	if got := c.Settings()["household"]; got["members_enabled"] != true || got["max_members"] != DefaultMaxMembers {
		t.Fatalf("defaults = %v", got)
	}
	for _, bad := range []string{`0`, `51`, `"6"`} {
		if _, err := c.WithSettings(patch(map[string]string{"household.max_members": bad}), Checks{}); settingErr(t, err).Setting != "household.max_members" {
			t.Fatalf("%s accepted", bad)
		}
	}
	next, err := c.WithSettings(patch(map[string]string{"household.max_members": `2`, "household.members_enabled": `false`}), Checks{})
	if err != nil || next.Household.MaxMembers != 2 || next.Household.MembersEnabled {
		t.Fatalf("set = %+v, %v", next.Household, err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("bind: 0.0.0.0:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_HOUSEHOLD_MAX_MEMBERS", "3")
	c, _, err = Load(dir)
	if err != nil || c.Household.MaxMembers != 3 || !c.Household.MembersEnabled ||
		c.Locked()["household.max_members"] != "AUDIOSILO_HOUSEHOLD_MAX_MEMBERS" {
		t.Fatalf("from the environment = %+v %v %v", c.Household, c.Locked(), err)
	}
	t.Setenv("AUDIOSILO_HOUSEHOLD_MAX_MEMBERS", "99")
	if _, _, err := Load(dir); err == nil {
		t.Fatal("max_members 99 from the environment loaded")
	}
}

func TestModuleSettings(t *testing.T) {
	c := Default(t.TempDir())
	if got := c.Settings()["modules"]; got["contributions"] != false {
		t.Fatalf("modules default = %v, want contributions off", got)
	}
	if c.ModuleEnabled("contributions") || !c.HasModule("contributions") {
		t.Fatal("contributions must have a config row and default off")
	}
	if c.ModuleEnabled("nope") || c.HasModule("nope") || c.HasModule("") {
		t.Fatal("an unknown module has no row and is never on")
	}
	if slices.Contains(RestartSettings(), "modules.contributions") {
		t.Fatal("a module toggle must apply without a restart")
	}

	next, err := c.WithSettings(patch(map[string]string{"modules.contributions": `true`}), Checks{})
	if err != nil || !next.ModuleEnabled("contributions") || c.ModuleEnabled("contributions") {
		t.Fatalf("toggle on = %v, %v (the original must stay off)", next.Modules, err)
	}
	if _, err := c.WithSettings(patch(map[string]string{"modules.contributions": `"yes"`}), Checks{}); settingErr(t, err).Reason != ReasonInvalid {
		t.Fatal("a non-bool toggle was accepted")
	}
	if _, err := c.WithSettings(patch(map[string]string{"modules.nope": `true`}), Checks{}); settingErr(t, err).Reason != ReasonUnknown {
		t.Fatal("an unknown module setting was accepted")
	}
	if got := ChangedSettings(c, next); len(got) != 1 || got[0].Setting != "modules.contributions" || got[0].From != false || got[0].To != true {
		t.Fatalf("ChangedSettings = %+v", got)
	}
	if !next.Clone().ModuleEnabled("contributions") || !next.Effective(c).ModuleEnabled("contributions") {
		t.Fatal("Clone and Effective must carry the toggle (it is not a restart setting)")
	}

	// From config.yaml, and from the environment (locked, never saved).
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("modules:\n  contributions:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err = Load(dir)
	if err != nil || !c.ModuleEnabled("contributions") || c.Locked()["modules.contributions"] != "" {
		t.Fatalf("from config.yaml = %+v %v %v", c.Modules, c.Locked(), err)
	}
	t.Setenv("AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED", "false")
	c, _, err = Load(dir)
	if err != nil || c.ModuleEnabled("contributions") || c.Locked()["modules.contributions"] != "AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED" {
		t.Fatalf("from the environment = %+v %v %v", c.Modules, c.Locked(), err)
	}
	if _, err := c.WithSettings(patch(map[string]string{"modules.contributions": `true`}), Checks{}); settingErr(t, err).Reason != ReasonLocked {
		t.Fatal("an env-set module toggle must be locked")
	}
	next, err = c.WithSettings(patch(map[string]string{"general.name": `"Den"`}), Checks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED", "")
	if c, _, err = Load(dir); err != nil || !c.ModuleEnabled("contributions") || c.Name != "Den" {
		t.Fatalf("config.yaml must keep its own toggle under the variable: %+v %v", c.Modules, err)
	}
}

func TestPodcastsModuleSetting(t *testing.T) {
	c := Default(t.TempDir())
	if c.ModuleEnabled("podcasts") || !c.HasModule("podcasts") {
		t.Fatal("podcasts must ship off")
	}
	t.Setenv("AUDIOSILO_MODULES_PODCASTS_ENABLED", "true")
	loaded, _, err := Load(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ModuleEnabled("podcasts") || loaded.Locked()["modules.podcasts"] != "AUDIOSILO_MODULES_PODCASTS_ENABLED" {
		t.Fatal("podcasts environment override missing")
	}
}

// scan.concurrency: 0 (the default, one per CPU up to a cap) unless set, 0 to 64,
// from config.yaml or AUDIOSILO_SCAN_CONCURRENCY, read at start and no console
// setting.
func TestScanConcurrency(t *testing.T) {
	c := Default(t.TempDir())
	if c.Scan.Concurrency != 0 {
		t.Fatalf("default = %d", c.Scan.Concurrency)
	}
	if _, ok := c.Settings()["scan"]; ok {
		t.Fatal("scan.concurrency is a console setting")
	}
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("scan:\n  concurrency: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, err := Load(dir); err != nil || c.Scan.Concurrency != 3 {
		t.Fatalf("from config.yaml = %+v, %v", c.Scan, err)
	}
	t.Setenv("AUDIOSILO_SCAN_CONCURRENCY", "12")
	if c, _, err := Load(dir); err != nil || c.Scan.Concurrency != 12 {
		t.Fatalf("from the environment = %+v, %v", c.Scan, err)
	}
	for _, bad := range []string{"-1", "65"} {
		t.Setenv("AUDIOSILO_SCAN_CONCURRENCY", bad)
		var fe *FieldError
		if _, _, err := Load(dir); !errors.As(err, &fe) || fe.Key != "scan.concurrency" {
			t.Fatalf("concurrency %s loaded: %v", bad, err)
		}
	}
	t.Setenv("AUDIOSILO_SCAN_CONCURRENCY", "64")
	if c, _, err := Load(dir); err != nil || c.Scan.Concurrency != MaxScanConcurrency {
		t.Fatalf("the maximum = %+v, %v", c.Scan, err)
	}
}
