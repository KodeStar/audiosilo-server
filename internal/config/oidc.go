package config

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AuthConfig holds the sign-in methods beyond auth codes and passwords.
type AuthConfig struct {
	OIDC OIDCConfig `yaml:"oidc"`
}

// OIDCConfig configures Sign in with OpenID Connect (server#235): one provider,
// an authorization code flow with PKCE run by the server (internal/oidc), the
// redirect URI <public_url>/api/v1/auth/oidc/callback. The provider fields are
// read once at start and shown read only in the console (Settings > Sign-in);
// AutoCreate, AllowedGroup and AdminGroup apply on the next sign-in and are the
// console's to change. A problem with the provider fields never stops the
// server: OIDC stays off and the console says why (Problem).
type OIDCConfig struct {
	Enabled bool `yaml:"enabled"`
	// Name is the provider as the sign-in button names it ("Sign in with
	// <name>"); empty is DefaultOIDCName.
	Name string `yaml:"name"`
	// Issuer is the provider's issuer exactly as its discovery document states
	// it (a trailing slash counts): <issuer>/.well-known/openid-configuration.
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// ClientSecret is the client's secret: config.yaml, AUDIOSILO_OIDC_CLIENT_SECRET
	// or the file ClientSecretFile names (one of them; none is a public client,
	// which authenticates with PKCE alone). Never in GET /admin/settings, never
	// logged; the console sees only whether one is set.
	ClientSecret     string `yaml:"client_secret,omitempty" json:"-"`
	ClientSecretFile string `yaml:"client_secret_file,omitempty"`
	// Scopes asked for; empty is DefaultOIDCScopes, with groups only while a
	// group policy is set (EffectiveScopes). openid is always asked.
	Scopes []string `yaml:"scopes,omitempty"`
	// UsernameClaim names the claim a new account's username comes from (empty:
	// preferred_username); GroupsClaim the claim listing the person's groups
	// (empty: groups).
	UsernameClaim string `yaml:"username_claim,omitempty"`
	GroupsClaim   string `yaml:"groups_claim,omitempty"`
	// AdminGroup, when set, makes the role follow the group at every OIDC
	// sign-in: in it an admin, out of it an ordinary user.
	AdminGroup string `yaml:"admin_group"`
	// AutoCreate makes an account for a sign-in no account is linked to or
	// matches (off: the sign-in waits for an admin); AllowedGroup, when set,
	// limits that to the group's members.
	AutoCreate   bool   `yaml:"auto_create"`
	AllowedGroup string `yaml:"allowed_group"`
	// CAFile is a PEM file of certificates trusted for the provider, beside the
	// system's (a provider on a private CA).
	CAFile string `yaml:"ca_file"`
}

// OIDC defaults (OIDCConfig).
const (
	DefaultOIDCName          = "single sign-on"
	DefaultOIDCUsernameClaim = "preferred_username"
	DefaultOIDCGroupsClaim   = "groups"
	// EnvOIDCClientSecret is the client secret's variable (a fields row, so it is
	// never saved to config.yaml from the environment).
	EnvOIDCClientSecret = "AUDIOSILO_OIDC_CLIENT_SECRET" //nolint:gosec // G101: the variable's name, not a credential
)

// DefaultOIDCScopes are the scopes asked for when none are configured, groups
// only while admin_group or allowed_group is set (EffectiveScopes): Keycloak
// refuses a scope it has no client scope for (invalid_scope), and Google and
// Entra have no groups scope at all.
var DefaultOIDCScopes = []string{"openid", "profile", "email", "groups"}

// oidcGroupsScope is the default scope asked for only with a group policy.
const oidcGroupsScope = "groups"

// maxOIDCClientSecret bounds the client secret (and its file), in bytes.
const maxOIDCClientSecret = 4096

// DisplayName is the provider's name for the button.
func (o OIDCConfig) DisplayName() string {
	if n := strings.TrimSpace(o.Name); n != "" {
		return n
	}
	return DefaultOIDCName
}

// EffectiveScopes are the scopes asked for, openid first and once: the
// configured ones as listed, else DefaultOIDCScopes, without groups while
// neither admin_group nor allowed_group is set (GroupPolicy). Both groups are
// live, so this is read at each sign-in.
func (o OIDCConfig) EffectiveScopes() []string {
	in := o.Scopes
	defaults := len(in) == 0
	if defaults {
		in = DefaultOIDCScopes
	}
	out := []string{"openid"}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || slices.Contains(out, s) || defaults && s == oidcGroupsScope && !o.GroupPolicy() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// GroupPolicy reports whether a sign-in's groups decide anything: admin_group
// or allowed_group is set.
func (o OIDCConfig) GroupPolicy() bool {
	return strings.TrimSpace(o.AdminGroup) != "" || strings.TrimSpace(o.AllowedGroup) != ""
}

// EffectiveUsernameClaim is UsernameClaim or its default.
func (o OIDCConfig) EffectiveUsernameClaim() string {
	return cmp.Or(strings.TrimSpace(o.UsernameClaim), DefaultOIDCUsernameClaim)
}

// EffectiveGroupsClaim is GroupsClaim or its default.
func (o OIDCConfig) EffectiveGroupsClaim() string {
	return cmp.Or(strings.TrimSpace(o.GroupsClaim), DefaultOIDCGroupsClaim)
}

// SecretSet reports whether a client secret is configured (inline, from the
// environment or as a file), without reading the file.
func (o OIDCConfig) SecretSet() bool {
	return o.ClientSecret != "" || strings.TrimSpace(o.ClientSecretFile) != ""
}

// Secret is the client secret: ClientSecret, else the file's content without
// its trailing line break, else "" (a public client). Both set is an error, as
// is a file that can't be read or is empty or over 4 KiB. The error never
// carries the secret.
func (o OIDCConfig) Secret() (string, error) {
	file := strings.TrimSpace(o.ClientSecretFile)
	if o.ClientSecret != "" && file != "" {
		return "", errors.New("set client_secret or client_secret_file, not both")
	}
	if file == "" {
		if len(o.ClientSecret) > maxOIDCClientSecret {
			return "", errors.New("client_secret is too long")
		}
		return o.ClientSecret, nil
	}
	st, err := os.Stat(file)
	if err != nil {
		return "", fmt.Errorf("client_secret_file can't be read: %w", err)
	}
	if st.Size() > maxOIDCClientSecret {
		return "", errors.New("client_secret_file is too large")
	}
	raw, err := os.ReadFile(file) //nolint:gosec // G304: an admin-configured path
	if err != nil {
		return "", fmt.Errorf("client_secret_file can't be read: %w", err)
	}
	secret := strings.TrimRight(string(raw), "\r\n")
	if secret == "" {
		return "", errors.New("client_secret_file is empty")
	}
	return secret, nil
}

// Problem says why OIDC can't run with this config ("" when it can, or when it
// is off). publicURL is the server's public_url: every redirect is built from
// it, so OIDC needs one. It reads no file (Secret does, at start).
func (o OIDCConfig) Problem(publicURL string) string {
	if !o.Enabled {
		return ""
	}
	if strings.TrimSpace(publicURL) == "" {
		return "set public_url: the provider sends people back to <public_url>/api/v1/auth/oidc/callback"
	}
	if o.Issuer == "" {
		return "set auth.oidc.issuer"
	}
	// httpURL's rule, with no space around it: the issuer is used exactly as
	// configured (a trailing slash counts).
	if _, err := httpURL(o.Issuer); err != nil || strings.TrimSpace(o.Issuer) != o.Issuer {
		return "auth.oidc.issuer must be an http(s) address with no ?, # or user name"
	}
	if strings.TrimSpace(o.ClientID) == "" {
		return "set auth.oidc.client_id"
	}
	for _, c := range []string{o.EffectiveUsernameClaim(), o.EffectiveGroupsClaim()} {
		if !claimName(c) {
			return "a claim name holds only letters, digits, '.', '_', '-', ':' or '/'"
		}
	}
	return ""
}

// claimName reports whether s can name a claim: a plain name, or a namespaced
// one as some providers require of a custom claim (https://example.org/groups).
func claimName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-:/", r))) {
			return false
		}
	}
	return true
}

// MaxOIDCGroup bounds a group name setting, in bytes: the bound on a group a
// sign-in carries (internal/oidc's maxGroupClaim), so a setting can always
// name a group a sign-in can match.
const MaxOIDCGroup = 256

func fixOIDCAdminGroup(c *Config) error {
	return fixGroup(&c.Auth.OIDC.AdminGroup)
}

func fixOIDCAllowedGroup(c *Config) error {
	return fixGroup(&c.Auth.OIDC.AllowedGroup)
}

// fixGroup is fixText's rule for a group name, its length counted in bytes.
func fixGroup(p *string) error {
	*p = strings.TrimSpace(*p)
	if len(*p) > MaxOIDCGroup {
		return fmt.Errorf("a group name can be at most %d bytes", MaxOIDCGroup)
	}
	return fixText(p, MaxOIDCGroup, "a group name")
}
