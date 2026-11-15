package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOIDCDefaults(t *testing.T) {
	t.Parallel()
	var o OIDCConfig
	if o.DisplayName() != DefaultOIDCName || o.EffectiveUsernameClaim() != "preferred_username" || o.EffectiveGroupsClaim() != "groups" {
		t.Fatalf("defaults: %q %q %q", o.DisplayName(), o.EffectiveUsernameClaim(), o.EffectiveGroupsClaim())
	}
	if got := o.EffectiveScopes(); !reflect.DeepEqual(got, DefaultOIDCScopes) {
		t.Fatalf("default scopes %v", got)
	}
	o.Scopes = []string{"email", " openid ", "email", ""}
	if got := o.EffectiveScopes(); !reflect.DeepEqual(got, []string{"openid", "email"}) {
		t.Fatalf("scopes %v: openid first, once", got)
	}
}

func TestOIDCProblem(t *testing.T) {
	t.Parallel()
	good := OIDCConfig{Enabled: true, Issuer: "https://idp.example/realms/home", ClientID: "audiosilo"}
	if p := good.Problem("https://audiosilo.example"); p != "" {
		t.Fatalf("a good config: %q", p)
	}
	if p := (OIDCConfig{}).Problem(""); p != "" {
		t.Fatalf("off: %q", p)
	}
	for name, c := range map[string]struct {
		o      OIDCConfig
		public string
	}{
		"no public_url":   {good, ""},
		"no issuer":       {OIDCConfig{Enabled: true, ClientID: "x"}, "https://a"},
		"not http(s)":     {OIDCConfig{Enabled: true, Issuer: "ftp://idp", ClientID: "x"}, "https://a"},
		"a query":         {OIDCConfig{Enabled: true, Issuer: "https://idp?x=1", ClientID: "x"}, "https://a"},
		"no client id":    {OIDCConfig{Enabled: true, Issuer: "https://idp"}, "https://a"},
		"bad claim":       {OIDCConfig{Enabled: true, Issuer: "https://idp", ClientID: "x", UsernameClaim: "a b"}, "https://a"},
		"bad group claim": {OIDCConfig{Enabled: true, Issuer: "https://idp", ClientID: "x", GroupsClaim: "a\nb"}, "https://a"},
	} {
		if p := c.o.Problem(c.public); p == "" {
			t.Errorf("%s: no problem", name)
		}
	}
}

func TestOIDCSecret(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := (OIDCConfig{ClientSecret: "inline"}).Secret(); err != nil || s != "inline" {
		t.Fatalf("inline: %q %v", s, err)
	}
	if s, err := (OIDCConfig{ClientSecretFile: file}).Secret(); err != nil || s != "from-file" {
		t.Fatalf("file: %q %v", s, err)
	}
	if s, err := (OIDCConfig{}).Secret(); err != nil || s != "" {
		t.Fatalf("public client: %q %v", s, err)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	for name, o := range map[string]OIDCConfig{
		"both":    {ClientSecret: "inline", ClientSecretFile: file},
		"missing": {ClientSecretFile: filepath.Join(dir, "nope")},
		"empty":   {ClientSecretFile: empty},
	} {
		s, err := o.Secret()
		if err == nil {
			t.Errorf("%s: accepted %q", name, s)
		} else if strings.Contains(err.Error(), "from-file") || strings.Contains(err.Error(), "inline") {
			t.Errorf("%s: the error shows the secret: %v", name, err)
		}
	}
	if !(OIDCConfig{ClientSecretFile: file}).SecretSet() || (OIDCConfig{}).SecretSet() {
		t.Fatal("SecretSet")
	}
}

// TestOIDCSettingsRows: the provider is read only, who may sign in is live and
// editable, the environment locks, and the secret is never a setting, never
// saved from the environment.
func TestOIDCSettingsRows(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("auth:\n  oidc:\n    enabled: true\n    issuer: https://file.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_OIDC_CLIENT_SECRET", "env-secret")
	t.Setenv("AUDIOSILO_OIDC_ADMIN_GROUP", "env-admins")
	t.Setenv("AUDIOSILO_OIDC_SCOPES", "openid, email")
	c, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Auth.OIDC
	if !o.Enabled || o.Issuer != "https://file.example" || o.ClientSecret != "env-secret" || o.AdminGroup != "env-admins" ||
		!reflect.DeepEqual(o.Scopes, []string{"openid", "email"}) {
		t.Fatalf("loaded: %+v", o)
	}
	sec := c.Settings()["signin"]
	if _, ok := sec["client_secret"]; ok {
		t.Fatal("the client secret is a setting")
	}
	if sec["issuer"] != "https://file.example" || sec["admin_group"] != "env-admins" {
		t.Fatalf("signin section: %v", sec)
	}
	if c.Locked()["signin.admin_group"] != "AUDIOSILO_OIDC_ADMIN_GROUP" {
		t.Fatalf("locked: %v", c.Locked())
	}
	for _, id := range []string{"signin.issuer", "signin.enabled", "signin.client_id", "signin.scopes"} {
		section, name, _ := strings.Cut(id, ".")
		_, err := c.WithSettings(patch(map[string]string{id: `"x"`}), ok)
		if se := settingErr(t, err); se.Reason != ReasonReadOnly {
			t.Fatalf("%s.%s: %s", section, name, se.Reason)
		}
	}
	if _, err := c.WithSettings(patch(map[string]string{"signin.admin_group": `"x"`}), ok); settingErr(t, err).Reason != ReasonLocked {
		t.Fatal("an env-set admin_group is not locked")
	}
	next, err := c.WithSettings(patch(map[string]string{"signin.auto_create": `true`, "signin.allowed_group": `" family "`}), ok)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Auth.OIDC.AutoCreate || next.Auth.OIDC.AllowedGroup != "family" {
		t.Fatalf("saved: %+v", next.Auth.OIDC)
	}
	if _, err := c.WithSettings(patch(map[string]string{"signin.allowed_group": `"` + strings.Repeat("g", MaxOIDCGroup+1) + `"`}), ok); settingErr(t, err).Reason != ReasonInvalid {
		t.Fatal("a too long group")
	}
	if err := next.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Path(dir))
	if strings.Contains(string(raw), "env-secret") || strings.Contains(string(raw), "env-admins") || !strings.Contains(string(raw), "allowed_group: family") {
		t.Fatalf("config.yaml:\n%s", raw)
	}
	for _, ch := range ChangedSettings(c, next) {
		if strings.Contains(ch.Setting, "secret") {
			t.Fatalf("a secret in the audit's changes: %+v", ch)
		}
	}
}

func TestOIDCCloneScopes(t *testing.T) {
	t.Parallel()
	c := Default(t.TempDir())
	c.Auth.OIDC.Scopes = []string{"openid"}
	d := c.Clone()
	d.Auth.OIDC.Scopes[0] = "changed"
	if c.Auth.OIDC.Scopes[0] != "openid" {
		t.Fatal("Clone shares the scopes")
	}
}
