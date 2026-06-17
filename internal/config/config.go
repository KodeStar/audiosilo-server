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

// TLSConfig holds TLS-related settings.
type TLSConfig struct {
	Mode     TLSMode  `yaml:"mode"`
	Hosts    []string `yaml:"hosts"`     // autocert: hostnames to obtain certs for
	CacheDir string   `yaml:"cache_dir"` // autocert: cert cache (default <data>/certs)
	CertFile string   `yaml:"cert_file"` // selfsigned/manual: optional persisted cert
	KeyFile  string   `yaml:"key_file"`  // selfsigned/manual: optional persisted key
}

// Config is the full server configuration.
type Config struct {
	// DataDir is where the database, config and generated certs live. It is not
	// serialized; it is supplied on the command line / environment.
	DataDir string `yaml:"-"`

	Bind           string    `yaml:"bind"`       // host:port to listen on
	PublicURL      string    `yaml:"public_url"` // externally reachable base URL, used in QR payloads
	TLS            TLSConfig `yaml:"tls"`
	TrustedProxies []string  `yaml:"trusted_proxies"` // CIDRs whose X-Forwarded-For is trusted
	CORSOrigins    []string  `yaml:"cors_origins"`    // allowed web origins ("*" to disable check)
	MaxUploadBytes int64     `yaml:"max_upload_bytes"`
	Libraries      []Library `yaml:"libraries"`
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
	return nil
}

// Storage layout identifiers.
const (
	LayoutFlat             = "flat"
	LayoutChaptersInFolder = "chapters_in_folder"
	LayoutBooksInFolder    = "books_in_folder"
)
