// Package config loads and validates AudioSilo server configuration.
//
// Configuration is read from a YAML file inside the data directory and can be
// overridden by environment variables (prefixed AUDIOSILO_). On first run the
// file does not exist and Load returns a Config populated with secure defaults;
// callers are expected to persist it via Save.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TLSMode selects how the server terminates TLS.
type TLSMode string

const (
	// TLSOff serves plain HTTP. Intended for use behind a reverse proxy that
	// terminates TLS (see TrustedProxies).
	TLSOff TLSMode = "off"
	// TLSSelfSigned generates an in-memory self-signed certificate. Good for
	// LAN use; clients must accept the certificate.
	TLSSelfSigned TLSMode = "selfsigned"
	// TLSAutocert obtains certificates from Let's Encrypt via ACME. Requires a
	// publicly reachable hostname on port 443.
	TLSAutocert TLSMode = "autocert"
)

// Library is a named root directory containing audiobooks.
type Library struct {
	Name   string `yaml:"name"`
	Root   string `yaml:"root"`
	Layout string `yaml:"layout"` // flat | chapters_in_folder | books_in_folder
}

// AppLinkConfig holds the identifiers needed to serve the well-known association
// files (apple-app-site-association, assetlinks.json) so that a domain pointed at
// this server can deep-link straight into the installed mobile app (iOS Universal
// Links / Android App Links). It is optional: when AppleAppIDs and AndroidPackage
// are both empty the well-known endpoints return 404 and clients fall back to the
// embedded web player. Self-hosted note: the app build must also claim this domain,
// so this only enables auto-launch for domains the shipped app knows about.
type AppLinkConfig struct {
	AppleAppIDs    []string `yaml:"apple_app_ids"`   // "<TEAMID>.<bundleId>" entries, e.g. ABCDE12345.com.anonymous.audiosilo
	AndroidPackage string   `yaml:"android_package"` // e.g. com.anonymous.audiosilo
	AndroidSHA256  []string `yaml:"android_sha256"`  // signing-cert SHA-256 fingerprints (colon-separated uppercase hex)
}

// TLSConfig holds TLS-related settings.
type TLSConfig struct {
	Mode     TLSMode  `yaml:"mode"`
	Hosts    []string `yaml:"hosts"`     // autocert: hostnames to obtain certs for
	CacheDir string   `yaml:"cache_dir"` // autocert: cert cache (default <data>/certs)
	CertFile string   `yaml:"cert_file"` // selfsigned/manual: optional persisted cert
	KeyFile  string   `yaml:"key_file"`  // selfsigned/manual: optional persisted key
}

// DemoConfig configures public demo mode: when enabled, an unauthenticated
// visitor can mint a throwaway account via POST /api/v1/demo/session (granted the
// named library), and idle demo accounts are reaped in the background.
type DemoConfig struct {
	Enabled  bool   `yaml:"enabled"`   // activate demo mode
	Library  string `yaml:"library"`   // name of the library demo users are granted
	MaxUsers int    `yaml:"max_users"` // hard cap on live demo users (0 = unlimited)
	IdleTTL  string `yaml:"idle_ttl"`  // reap demo users idle longer than this, e.g. "24h" (default 24h)
}

// IdleTTLDuration parses IdleTTL, falling back to 24h when empty or invalid.
func (d DemoConfig) IdleTTLDuration() time.Duration {
	if d.IdleTTL != "" {
		if v, err := time.ParseDuration(d.IdleTTL); err == nil && v > 0 {
			return v
		}
	}
	return 24 * time.Hour
}

// Config is the full server configuration.
type Config struct {
	// DataDir is where the database, config and generated certs live. It is not
	// serialized; it is supplied on the command line / environment.
	DataDir string `yaml:"-"`

	Bind           string        `yaml:"bind"`       // host:port to listen on
	PublicURL      string        `yaml:"public_url"` // externally reachable base URL, used in QR payloads
	TLS            TLSConfig     `yaml:"tls"`
	TrustedProxies []string      `yaml:"trusted_proxies"` // CIDRs whose X-Forwarded-For is trusted
	CORSOrigins    []string      `yaml:"cors_origins"`    // allowed web origins ("*" to disable check)
	MaxUploadBytes int64         `yaml:"max_upload_bytes"`
	WebDir         string        `yaml:"web_dir"`   // directory of the prebuilt web player served at /web; empty disables it
	AppLinks       AppLinkConfig `yaml:"app_links"` // optional native deep-link association (well-known files)
	Libraries      []Library     `yaml:"libraries"`
	Demo           DemoConfig    `yaml:"demo"` // public demo mode (throwaway accounts)
}

// ConfigFileName is the config file stored inside the data directory.
const ConfigFileName = "config.yaml"

// Default returns a Config with secure defaults for the given data directory.
func Default(dataDir string) *Config {
	return &Config{
		DataDir:        dataDir,
		Bind:           "0.0.0.0:8080",
		PublicURL:      "",
		TLS:            TLSConfig{Mode: TLSSelfSigned, CacheDir: filepath.Join(dataDir, "certs")},
		TrustedProxies: nil,
		CORSOrigins:    nil,
		MaxUploadBytes: 2 << 30, // 2 GiB
		Libraries:      nil,
	}
}

// Path returns the on-disk path of the config file for a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, ConfigFileName) }

// Load reads config from <dataDir>/config.yaml, applies environment overrides
// and validates the result. If the file does not exist, defaults are used and
// the returned bool (firstRun) is true so the caller can bootstrap and Save.
func Load(dataDir string) (cfg *Config, firstRun bool, err error) {
	cfg = Default(dataDir)
	raw, readErr := os.ReadFile(Path(dataDir))
	switch {
	case readErr == nil:
		if err = yaml.Unmarshal(raw, cfg); err != nil {
			return nil, false, fmt.Errorf("parse config: %w", err)
		}
		cfg.DataDir = dataDir
	case errors.Is(readErr, os.ErrNotExist):
		firstRun = true
	default:
		return nil, false, fmt.Errorf("read config: %w", readErr)
	}

	applyEnv(cfg)
	if cfg.TLS.CacheDir == "" {
		cfg.TLS.CacheDir = filepath.Join(dataDir, "certs")
	}
	if err = cfg.Validate(); err != nil {
		return nil, firstRun, err
	}
	return cfg, firstRun, nil
}

// Save writes the config to disk with restrictive permissions.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(Path(c.DataDir), out, 0o600)
}

// applyEnv overrides selected fields from AUDIOSILO_* environment variables.
func applyEnv(c *Config) {
	if v := os.Getenv("AUDIOSILO_BIND"); v != "" {
		c.Bind = v
	}
	if v := os.Getenv("AUDIOSILO_PUBLIC_URL"); v != "" {
		c.PublicURL = v
	}
	if v := os.Getenv("AUDIOSILO_WEB_DIR"); v != "" {
		c.WebDir = v
	}
	if v := os.Getenv("AUDIOSILO_TLS_MODE"); v != "" {
		c.TLS.Mode = TLSMode(v)
	}
	if v := os.Getenv("AUDIOSILO_TLS_HOSTS"); v != "" {
		c.TLS.Hosts = splitList(v)
	}
	if v := os.Getenv("AUDIOSILO_TRUSTED_PROXIES"); v != "" {
		c.TrustedProxies = splitList(v)
	}
	if v := os.Getenv("AUDIOSILO_CORS_ORIGINS"); v != "" {
		c.CORSOrigins = splitList(v)
	}
	if v := os.Getenv("AUDIOSILO_MAX_UPLOAD_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.MaxUploadBytes = n
		}
	}
	if v := os.Getenv("AUDIOSILO_DEMO_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Demo.Enabled = b
		}
	}
	if v := os.Getenv("AUDIOSILO_DEMO_LIBRARY"); v != "" {
		c.Demo.Library = v
	}
	if v := os.Getenv("AUDIOSILO_DEMO_MAX_USERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Demo.MaxUsers = n
		}
	}
	if v := os.Getenv("AUDIOSILO_DEMO_IDLE_TTL"); v != "" {
		c.Demo.IdleTTL = v
	}
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks that the config is internally consistent.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("data dir is required")
	}
	if _, _, err := net.SplitHostPort(c.Bind); err != nil {
		return fmt.Errorf("invalid bind address %q: %w", c.Bind, err)
	}
	switch c.TLS.Mode {
	case TLSOff, TLSSelfSigned, TLSAutocert:
	default:
		return fmt.Errorf("invalid tls mode %q", c.TLS.Mode)
	}
	if c.TLS.Mode == TLSAutocert && len(c.TLS.Hosts) == 0 {
		return errors.New("tls mode autocert requires tls.hosts")
	}
	for _, p := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil {
			return fmt.Errorf("invalid trusted proxy CIDR %q: %w", p, err)
		}
	}
	seen := map[string]bool{}
	for i, lib := range c.Libraries {
		if lib.Name == "" {
			return fmt.Errorf("library %d: name is required", i)
		}
		if seen[lib.Name] {
			return fmt.Errorf("duplicate library name %q", lib.Name)
		}
		seen[lib.Name] = true
		if lib.Root == "" {
			return fmt.Errorf("library %q: root is required", lib.Name)
		}
		switch lib.Layout {
		case "", LayoutFlat, LayoutChaptersInFolder, LayoutBooksInFolder:
		default:
			return fmt.Errorf("library %q: invalid layout %q", lib.Name, lib.Layout)
		}
	}
	if c.Demo.Enabled {
		if c.Demo.Library == "" {
			return errors.New("demo mode requires demo.library")
		}
		if c.Demo.IdleTTL != "" {
			if _, err := time.ParseDuration(c.Demo.IdleTTL); err != nil {
				return fmt.Errorf("invalid demo.idle_ttl %q: %w", c.Demo.IdleTTL, err)
			}
		}
	}
	return nil
}

// Storage layout identifiers.
const (
	LayoutFlat             = "flat"
	LayoutChaptersInFolder = "chapters_in_folder"
	LayoutBooksInFolder    = "books_in_folder"
)
