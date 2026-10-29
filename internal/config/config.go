// Package config loads and validates AudioSilo server configuration.
//
// Configuration is read from a YAML file inside the data directory and can be
// overridden by environment variables (prefixed AUDIOSILO_). On first run the
// file does not exist and Load returns a Config populated with secure defaults;
// callers are expected to persist it via Save.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/store"
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

// Library is a named root directory containing audiobooks. The scanner
// auto-detects each folder's shape (single-file books, folder-per-book,
// multi-file parts); there is no layout setting to configure.
type Library struct {
	Name string `yaml:"name"`
	Root string `yaml:"root"`
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
	Mode TLSMode `yaml:"mode"`
	// HomeHTTP (selfsigned only) lets the listening port also answer plain HTTP,
	// to clients whose own address is on the home network (HomeNetworkIP or
	// loopback): the native apps can't trust a self-signed certificate. Other
	// plain HTTP clients are sent to https. On by default.
	HomeHTTP bool     `yaml:"home_http"`
	Hosts    []string `yaml:"hosts"`     // autocert: hostnames to obtain certs for
	CacheDir string   `yaml:"cache_dir"` // autocert: cert cache (default <data>/certs)
	CertFile string   `yaml:"cert_file"` // selfsigned/manual: optional persisted cert
	KeyFile  string   `yaml:"key_file"`  // selfsigned/manual: optional persisted key
}

// BackupConfig controls the database backups (internal/backup).
type BackupConfig struct {
	// Schedule is "" (off), "daily:HH:MM" or "weekly:DAY:HH:MM" in the server's
	// time zone (backup.ParseSchedule).
	Schedule string `yaml:"schedule"`
	// Keep is how many scheduled backups to keep (manual ones stay until deleted).
	Keep int `yaml:"keep"`
	// Dir is the backups folder; empty means <data>/backups. Only config.yaml or
	// AUDIOSILO_BACKUP_DIR set it, never the console.
	Dir string `yaml:"dir"`
}

// Backup defaults: a daily backup at 03:00, the newest week kept.
const (
	DefaultBackupSchedule = "daily:03:00"
	DefaultBackupKeep     = 7
	// MaxBackupKeep bounds backups.keep.
	MaxBackupKeep = 365
)

// ActivityConfig controls what the Activity pages keep.
type ActivityConfig struct {
	// SessionDays is how many days raw listening sessions (which device and app,
	// the time of day, the playback mode) are kept before they are summed into
	// per-day totals per listener and book. Settings > General changes it; the
	// daily retention job applies a change.
	SessionDays int `yaml:"session_days"`
}

// Session retention: just over a year by default, so the Activity page's longest
// range (a year, or the current calendar year) reads raw sessions throughout.
const (
	DefaultSessionDays = 400
	MinSessionDays     = 30
	MaxSessionDays     = 3650
)

// SessionRetention is SessionDays as a duration.
func (a ActivityConfig) SessionRetention() time.Duration {
	return time.Duration(a.SessionDays) * 24 * time.Hour
}

// RateLimitConfig sets the API's request rate limits (api/ratelimit.go), each a
// token bucket refilled at Rate requests a second up to Burst. Read once at
// start. Address is per client address: requests no credential has been proved
// for yet (a missing or refused token pays here, after its lookup), the sign-in,
// pairing and account routes (/api/v1/auth/*, whatever token they carry), the
// Bridge's routes (/api/v1/bridge/*) and the public routes. Token is per
// credential: every other authenticated API request, so several people behind
// one address (a household behind NAT, a reverse proxy) each have their own.
// Media is per credential too: covers and audio (a grid is one request per
// cover). The brute-force lockouts on sign-in and code redemption are separate
// and not configurable.
//
// It holds only what the admin set: a rate or burst left at 0 is its current
// default (Effective), so it is never written into config.yaml and a later
// release's defaults reach an existing install.
type RateLimitConfig struct {
	Address RateBucket `yaml:"address,omitempty"`
	Token   RateBucket `yaml:"token,omitempty"`
	Media   RateBucket `yaml:"media,omitempty"`
}

// RateBucket is one rate limit: Rate requests a second on average and at most
// Burst at once; 0 (unset) is the default's.
type RateBucket struct {
	Rate  float64 `yaml:"rate,omitempty"`
	Burst int     `yaml:"burst,omitempty"`
}

// Effective is the limits in force: each rate or burst not set is its default.
func (r RateLimitConfig) Effective() RateLimitConfig {
	return RateLimitConfig{
		Address: r.Address.or(DefaultAddressRateLimit),
		Token:   r.Token.or(DefaultTokenRateLimit),
		Media:   r.Media.or(DefaultMediaRateLimit),
	}
}

func (b RateBucket) or(def RateBucket) RateBucket {
	return RateBucket{Rate: cmp.Or(b.Rate, def.Rate), Burst: cmp.Or(b.Burst, def.Burst)}
}

// The default rate limits (RateLimitConfig).
var (
	DefaultAddressRateLimit = RateBucket{Rate: 50, Burst: 200}
	DefaultTokenRateLimit   = RateBucket{Rate: 50000, Burst: 100000}
	DefaultMediaRateLimit   = RateBucket{Rate: 200, Burst: 2000}
)

// HouseholdConfig configures household members (player redesign Phase 8b-1):
// whether people may add members from the player, and how many each. Both apply
// on the next request (read through the API's live config).
type HouseholdConfig struct {
	MembersEnabled bool `yaml:"members_enabled"` // anyone but a demo account may add members (default true)
	MaxMembers     int  `yaml:"max_members"`     // members per person, MinMaxMembers to MaxMaxMembers (default 6)
}

// Household member limits.
const (
	DefaultMaxMembers = 6
	MinMaxMembers     = 1
	MaxMaxMembers     = 50
)

// ScanConfig tunes library scans.
type ScanConfig struct {
	// Concurrency is how many audio files a scan reads at once (tags and ffprobe):
	// 0, the default, means one per CPU up to 8 (library.DefaultScanReaders).
	// Set below 8, it also caps how many folder listings and file lookups
	// discovery makes at once (8 otherwise). Lower it for a library on a slow disk
	// or a busy share, raise it for a fast one on a machine with cores to spare.
	// Read once at start (scan.concurrency, AUDIOSILO_SCAN_CONCURRENCY).
	Concurrency int `yaml:"concurrency"`
}

// MaxScanConcurrency bounds scan.concurrency.
const MaxScanConcurrency = 64

// DatabaseConfig tunes the SQLite database.
type DatabaseConfig struct {
	// Mmap is whether the database's readers map the file (store.WithReaderMmap),
	// in any case: "auto" (or empty, the default) only when the database file (in
	// the data folder; a symlink is followed) is on Linux on shfs (an Unraid
	// /mnt/user share), by its mount's type, where it is measured
	// several times faster; never on another FUSE filesystem (mergerfs, sshfs, rclone,
	// s3fs, ntfs-3g) or a network one, where an I/O error under a mapping
	// crashes the server and a read would fail one request; "on" always; "off"
	// never. Read once at start (database.mmap, AUDIOSILO_DB_MMAP).
	Mmap string `yaml:"mmap,omitempty"`
}

// ModulesConfig holds each managed module's settings (internal/modules), all
// off by default. A module's toggle is modules.<id>.enabled (the console's
// setting modules.<id>, AUDIOSILO_MODULES_<ID>_ENABLED); a module's own
// settings would be modules.<id>.<name> (setting <id>.<name>). Every key
// applies on the next request: a module starts and stops without a restart.
type ModulesConfig struct {
	Contributions ModuleConfig `yaml:"contributions"`
	Podcasts      ModuleConfig `yaml:"podcasts"`
}

// ModuleConfig is one module's toggle.
type ModuleConfig struct {
	Enabled bool `yaml:"enabled"`
}

// moduleToggle is module id's toggle in c, or nil when it has no config row.
func (c *Config) moduleToggle(id string) *bool {
	f := fieldByKey("modules." + id + ".enabled")
	if f == nil {
		return nil
	}
	on, _ := f.ptr(c).(*bool)
	return on
}

// HasModule reports whether module id has its toggle's config row (the
// registry is checked against it at start).
func (c *Config) HasModule(id string) bool { return c.moduleToggle(id) != nil }

// ModuleEnabled reports whether module id is turned on; false for an unknown id.
func (c *Config) ModuleEnabled(id string) bool {
	on := c.moduleToggle(id)
	return on != nil && *on
}

// PodcastsConfig holds the podcast settings that are not the module's toggle
// (modules.podcasts.enabled).
type PodcastsConfig struct {
	Discovery PodcastDiscoveryConfig `yaml:"discovery"`
}

// PodcastDiscoveryConfig configures podcast directory search (Bridge Phase
// 2c-3). Enabled is read once at start (podcasts.discovery.enabled,
// AUDIOSILO_PODCAST_DISCOVERY_ENABLED; on by default) and has no console
// setting; the podcasts module stays the live switch. The Podcast Index key pair
// comes only from the environment (AUDIOSILO_PODCAST_INDEX_KEY and
// AUDIOSILO_PODCAST_INDEX_SECRET, read at start): never written to config.yaml,
// never a console setting, never serialized (yaml:"-" json:"-").
type PodcastDiscoveryConfig struct {
	Enabled     bool   `yaml:"enabled"`
	IndexKey    string `yaml:"-" json:"-"`
	IndexSecret string `yaml:"-" json:"-"`
}

// The Podcast Index credential variables (environment only).
const (
	EnvPodcastIndexKey    = "AUDIOSILO_PODCAST_INDEX_KEY"
	EnvPodcastIndexSecret = "AUDIOSILO_PODCAST_INDEX_SECRET" //nolint:gosec // G101: the variable's name, not a credential
)

// maxPodcastIndexCredential bounds each Podcast Index credential, in bytes.
const maxPodcastIndexCredential = 256

// PodcastIndex returns the Podcast Index key pair when both are set and each is
// 1 to 256 printable, non-space ASCII bytes (kept byte for byte). ok is false
// otherwise; problem then says which variable is wrong and why, never its value,
// and is "" when neither is set (Podcast Index simply isn't configured).
func (d PodcastDiscoveryConfig) PodcastIndex() (key, secret string, ok bool, problem string) {
	if d.IndexKey == "" && d.IndexSecret == "" {
		return "", "", false, ""
	}
	for _, v := range []struct{ name, value string }{{EnvPodcastIndexKey, d.IndexKey}, {EnvPodcastIndexSecret, d.IndexSecret}} {
		if v.value == "" {
			return "", "", false, v.name + " is not set"
		}
		if !credentialBytes(v.value) {
			return "", "", false, v.name + " must be 1 to 256 printable ASCII characters without spaces"
		}
	}
	return d.IndexKey, d.IndexSecret, true, ""
}

func credentialBytes(v string) bool {
	if len(v) == 0 || len(v) > maxPodcastIndexCredential {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// DemoConfig configures public demo mode: when enabled, an unauthenticated
// visitor can mint a throwaway account via POST /api/v1/demo/session (granted the
// named library), and idle demo accounts are reaped in the background.
type DemoConfig struct {
	Enabled  bool   `yaml:"enabled"`             // activate demo mode
	Library  string `yaml:"library"`             // name of the library demo users are granted
	MaxUsers *int   `yaml:"max_users,omitempty"` // cap on live demo users; unset = safe default, explicit 0 = unlimited
	IdleTTL  string `yaml:"idle_ttl"`            // reap demo users idle longer than this, e.g. "24h" (default 24h)
}

// DefaultDemoMaxUsers caps concurrent live demo accounts when demo mode is on
// but demo.max_users is left unset. An exposed demo instance is otherwise
// unbounded: the per-IP creation limit is bypassable by rotating source IPs
// (trivial on an IPv6 /64), so without a global cap an attacker could grow the
// database far faster than the idle reaper trims it. Set an explicit
// demo.max_users to override (use 0 for unlimited, opting into that risk).
const DefaultDemoMaxUsers = 200

// EffectiveMaxUsers returns the cap on concurrent live demo accounts: the
// configured value when set (an explicit 0 means unlimited), or
// DefaultDemoMaxUsers when unset.
func (d DemoConfig) EffectiveMaxUsers() int {
	if d.MaxUsers != nil {
		return *d.MaxUsers
	}
	return DefaultDemoMaxUsers
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

// MetadataConfig configures the community metadata lookup (Phase 1.5): when
// enabled, the server resolves a book's asin/isbn against the metadata API
// (metaserve, meta.audiosilo.app) and exposes a composed enrichment envelope at
// GET /libraries/{id}/meta. Disabling it stops all outbound calls with one key,
// and the `metadata` capability flag then reports false so clients hide the UI.
type MetadataConfig struct {
	Enabled bool   `yaml:"enabled"`  // activate the meta lookup (default true)
	BaseURL string `yaml:"base_url"` // metaserve base URL; site at / and API at /api/v1
	// Region is the Audible marketplace whose ASIN a match takes when a recording
	// sells in several (one of Regions; "" = no preference, the US store's first).
	Region string `yaml:"region"`
	// Mode is where the lookups are answered (read only at start): MetadataRemote
	// asks the service at BaseURL for each book; MetadataMirror keeps a local copy
	// of the service's database (internal/metamirror), downloaded from GitHub once
	// a day, and asks BaseURL only while that copy can't answer.
	Mode string `yaml:"mode"`
}

// The metadata.mode values.
const (
	MetadataRemote = "remote"
	MetadataMirror = "mirror"
)

// MetadataModes are the values metadata.mode may take.
var MetadataModes = []string{MetadataRemote, MetadataMirror}

// ModeName is Mode as the server reads it: lower case, "" meaning
// MetadataRemote (a config.yaml written before the key existed).
func (m MetadataConfig) ModeName() string {
	if mode := strings.ToLower(strings.TrimSpace(m.Mode)); mode != "" {
		return mode
	}
	return MetadataRemote
}

// Mirror reports whether the server keeps a local copy of the metadata service.
func (m MetadataConfig) Mirror() bool { return m.ModeName() == MetadataMirror }

// Regions are the Audible marketplaces metadata.region may name: the community
// metadata's own region vocabulary (audiosilo-meta schema/common.schema.json).
var Regions = []string{"us", "uk", "ca", "au", "de", "fr", "es", "it", "jp", "in", "br"}

// PreferredRegion is Region in the form the community metadata uses (lower case).
func (m MetadataConfig) PreferredRegion() string {
	return strings.ToLower(strings.TrimSpace(m.Region))
}

// ValidBaseURL reports whether BaseURL is a usable absolute http(s) URL. The
// metadata service is constructed whenever this holds (regardless of Enabled),
// so the admin runtime toggle can flip the feature on without a restart; an
// empty or non-http(s) base_url means the feature is unavailable.
func (m MetadataConfig) ValidBaseURL() bool {
	if m.BaseURL == "" {
		return false
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		return false
	}
	return u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// Config is the full server configuration.
type Config struct {
	// DataDir is where the database, config and generated certs live. It is not
	// serialized; it is supplied on the command line / environment.
	DataDir string `yaml:"-"`

	// ServerID is a stable, per-install identity minted once (see launcher
	// bootstrap) and persisted here in config.yaml so it survives a database
	// rebuild. Clients key their per-server state (downloads, progress, cache)
	// on it, so it must never change for the life of the install.
	ServerID string `yaml:"server_id"`

	// Name is the server's display name: GET /server's name, invites and the admin
	// console. Empty means "AudioSilo" (DisplayName).
	Name string `yaml:"name"`
	// UpdateCheck lets the server ask GitHub Releases once a day whether a newer
	// version exists (internal/updates). On by default; the admin console's
	// Settings > General switch turns it off.
	UpdateCheck bool `yaml:"update_check"`

	Bind           string          `yaml:"bind"`       // host:port to listen on
	PublicURL      string          `yaml:"public_url"` // externally reachable base URL, used in QR payloads
	LANURL         string          `yaml:"lan_url"`    // the server's home-network address, the apps' home address ("" = derived per request; see Addresses)
	TLS            TLSConfig       `yaml:"tls"`
	TrustedProxies []string        `yaml:"trusted_proxies"`  // CIDRs whose X-Forwarded-For is trusted
	CORSOrigins    []string        `yaml:"cors_origins"`     // allowed web origins ("*" to disable check)
	MaxUploadBytes int64           `yaml:"max_upload_bytes"` // reserved for Phase B uploads; not yet enforced (JSON bodies use a fixed 1 MiB cap)
	WebDir         string          `yaml:"web_dir"`          // directory of the prebuilt web player served at /web; empty disables it
	AppLinks       AppLinkConfig   `yaml:"app_links"`        // optional native deep-link association (well-known files)
	Libraries      []Library       `yaml:"libraries"`
	Demo           DemoConfig      `yaml:"demo"`      // public demo mode (throwaway accounts)
	Metadata       MetadataConfig  `yaml:"metadata"`  // community metadata lookup (Phase 1.5)
	Backups        BackupConfig    `yaml:"backups"`   // database backups (internal/backup)
	Activity       ActivityConfig  `yaml:"activity"`  // listening history retention
	Household      HouseholdConfig `yaml:"household"` // household members
	Modules        ModulesConfig   `yaml:"modules"`   // managed modules (internal/modules), each off by default
	Podcasts       PodcastsConfig  `yaml:"podcasts"`  // podcast discovery (boot only; the Podcast Index pair from the environment only)
	Scan           ScanConfig      `yaml:"scan"`      // library scans (boot only)

	// RateLimit is the API's request rate limits (boot only).
	RateLimit RateLimitConfig `yaml:"rate_limit,omitempty"`
	// Database tunes the SQLite database (boot only).
	Database DatabaseConfig `yaml:"database,omitempty"`

	// fromEnv maps each field key (see fields) an AUDIOSILO_* variable set at load
	// to that variable's name; file is the config as config.yaml had it, before the
	// environment. Save writes the file's values for those keys, so a value the
	// environment supplies never ends up in config.yaml. pinned holds the keys an
	// embedding launcher overrides (Pin). All three are set while loading and only
	// read afterwards, so copies share them.
	fromEnv map[string]string
	file    *Config
	pinned  map[string]bool
	// onDisk: config.yaml exists. The first Save (creating it) records pinned
	// values in it; later saves keep the file's own values for them, as for env.
	onDisk bool
}

// DefaultServerName is the display name of a server whose name isn't set.
const DefaultServerName = "AudioSilo"

// DisplayName is the server's name, or DefaultServerName when none is set.
func (c *Config) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return DefaultServerName
}

// DefaultMetadataBaseURL is the community metadata API used when metadata lookup
// is enabled and no base_url is configured.
const DefaultMetadataBaseURL = "https://meta.audiosilo.app"

// ConfigFileName is the config file stored inside the data directory.
const ConfigFileName = "config.yaml"

// Default returns a Config with secure defaults for the given data directory.
func Default(dataDir string) *Config {
	return &Config{
		DataDir:        dataDir,
		Bind:           "0.0.0.0:8080",
		PublicURL:      "",
		TLS:            TLSConfig{Mode: TLSSelfSigned, HomeHTTP: true, CacheDir: filepath.Join(dataDir, "certs")},
		TrustedProxies: nil,
		CORSOrigins:    nil,
		MaxUploadBytes: 2 << 30, // 2 GiB
		Libraries:      nil,
		Metadata:       MetadataConfig{Enabled: true, BaseURL: DefaultMetadataBaseURL, Mode: MetadataRemote},
		UpdateCheck:    true,
		Backups:        BackupConfig{Schedule: DefaultBackupSchedule, Keep: DefaultBackupKeep},
		Activity:       ActivityConfig{SessionDays: DefaultSessionDays},
		Household:      HouseholdConfig{MembersEnabled: true, MaxMembers: DefaultMaxMembers},
		Podcasts:       PodcastsConfig{Discovery: PodcastDiscoveryConfig{Enabled: true}},
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
		cfg.onDisk = true
		if err = yaml.Unmarshal(raw, cfg); err != nil {
			return nil, false, fmt.Errorf("parse config: %w", err)
		}
		cfg.DataDir = dataDir
	case errors.Is(readErr, os.ErrNotExist):
		firstRun = true
	default:
		return nil, false, fmt.Errorf("read config: %w", readErr)
	}

	cfg.file = cfg.Clone()
	applyEnv(cfg)
	// The Podcast Index pair is environment only: no fields row, so it is never a
	// console setting, never locked or listed, and never saved.
	cfg.Podcasts.Discovery.IndexKey = os.Getenv(EnvPodcastIndexKey)
	cfg.Podcasts.Discovery.IndexSecret = os.Getenv(EnvPodcastIndexSecret)
	if cfg.TLS.CacheDir == "" {
		cfg.TLS.CacheDir = filepath.Join(dataDir, "certs")
	}
	if err = cfg.Validate(); err != nil {
		return nil, firstRun, err
	}
	return cfg, firstRun, nil
}

// Save writes the config to disk with restrictive permissions. A field an
// AUDIOSILO_* variable set keeps config.yaml's own value: the environment wins at
// every load anyway, and writing it into the file would make it outlive the
// variable. A field an embedding launcher pins likewise keeps the file's value,
// except on the save that creates config.yaml, which records it.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	out, err := yaml.Marshal(c.asSaved())
	if err != nil {
		return err
	}
	if err := os.WriteFile(Path(c.DataDir), out, 0o600); err != nil {
		return err
	}
	if !c.onDisk && c.file != nil {
		// The file now holds the pinned values: later saves keep them.
		for key := range c.pinned {
			copyField(c.file, c, key)
		}
		c.onDisk = true
	}
	return nil
}

// asSaved is c as Save writes it to config.yaml: the file's own values for the
// keys the environment sets (and, once the file exists, the launcher pins).
func (c *Config) asSaved() *Config {
	if c.file == nil {
		return c
	}
	on := c.Clone()
	for key := range c.fromEnv {
		copyField(on, c.file, key)
	}
	if c.onDisk {
		for key := range c.pinned {
			copyField(on, c.file, key)
		}
	}
	return on
}

// applyEnv overrides fields from AUDIOSILO_* environment variables (see fields)
// and records which, so Save leaves them out of config.yaml and the admin console
// can show them as set by the environment. A value that doesn't parse is ignored.
func applyEnv(c *Config) {
	for _, f := range fields {
		if f.env == "" {
			continue
		}
		v := os.Getenv(f.env)
		if v == "" || !setFromEnv(f.ptr(c), v) {
			continue
		}
		if c.fromEnv == nil {
			c.fromEnv = map[string]string{}
		}
		c.fromEnv[f.key] = f.env
	}
}

// setFromEnv parses v into the field ptr points at, reporting whether it did.
func setFromEnv(ptr any, v string) bool {
	switch p := ptr.(type) {
	case *string:
		*p = v
	case *TLSMode:
		*p = TLSMode(v)
	case *[]string:
		*p = splitList(v)
	case *bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false
		}
		*p = b
	case *int:
		n, err := strconv.Atoi(v)
		if err != nil {
			return false
		}
		*p = n
	case *float64:
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return false
		}
		*p = n
	case *int64:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return false
		}
		*p = n
	case **int:
		n, err := strconv.Atoi(v)
		if err != nil {
			return false
		}
		*p = &n
	default:
		return false
	}
	return true
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

// FieldError is a validation error about one field, named by its key in
// config.yaml ("tls.hosts"). Its message is the wrapped error's.
type FieldError struct {
	Key string
	Err error
}

func (e *FieldError) Error() string { return e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func fieldErr(key string, err error) error { return &FieldError{Key: key, Err: err} }

// Validate checks that the config is internally consistent.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("data dir is required")
	}
	if _, _, err := net.SplitHostPort(c.Bind); err != nil {
		return fieldErr("bind", fmt.Errorf("invalid bind address %q: %w", c.Bind, err))
	}
	switch c.TLS.Mode {
	case TLSOff, TLSSelfSigned, TLSAutocert:
	default:
		return fieldErr("tls.mode", fmt.Errorf("invalid tls mode %q", c.TLS.Mode))
	}
	if c.TLS.Mode == TLSAutocert && len(c.TLS.Hosts) == 0 {
		return fieldErr("tls.hosts", errors.New("tls mode autocert requires tls.hosts"))
	}
	for _, p := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil {
			return fieldErr("trusted_proxies", fmt.Errorf("invalid trusted proxy CIDR %q: %w", p, err))
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
	}
	if c.Demo.Enabled {
		if c.Demo.Library == "" {
			return fieldErr("demo.library", errors.New("demo mode requires demo.library"))
		}
		if c.Demo.IdleTTL != "" {
			d, err := time.ParseDuration(c.Demo.IdleTTL)
			if err != nil {
				return fieldErr("demo.idle_ttl", fmt.Errorf("invalid demo.idle_ttl %q: %w", c.Demo.IdleTTL, err))
			}
			// A non-positive TTL is silently replaced by the 24h fallback at
			// runtime (IdleTTLDuration), so reject it here instead of letting
			// the operator's value be discarded without a signal.
			if d <= 0 {
				return fieldErr("demo.idle_ttl", fmt.Errorf("demo.idle_ttl must be positive, got %q", c.Demo.IdleTTL))
			}
		}
	}
	if _, err := backup.ParseSchedule(c.Backups.Schedule); err != nil {
		return fieldErr("backups.schedule", fmt.Errorf("invalid backups.schedule %q: %w", c.Backups.Schedule, err))
	}
	if c.Backups.Keep < 1 || c.Backups.Keep > MaxBackupKeep {
		return fieldErr("backups.keep", fmt.Errorf("backups.keep must be from 1 to %d", MaxBackupKeep))
	}
	if d := c.Activity.SessionDays; d < MinSessionDays || d > MaxSessionDays {
		return fieldErr("activity.session_days", fmt.Errorf("keep listening sessions from %d to %d days", MinSessionDays, MaxSessionDays))
	}
	if n := c.Household.MaxMembers; n < MinMaxMembers || n > MaxMaxMembers {
		return fieldErr("household.max_members", fmt.Errorf("household.max_members must be from %d to %d", MinMaxMembers, MaxMaxMembers))
	}
	for _, b := range []struct {
		key string
		b   RateBucket
	}{{"rate_limit.address", c.RateLimit.Address}, {"rate_limit.token", c.RateLimit.Token}, {"rate_limit.media", c.RateLimit.Media}} {
		// 0 is unset (the default); anything else must let requests through.
		if b.b.Rate < 0 || math.IsNaN(b.b.Rate) || math.IsInf(b.b.Rate, 0) {
			return fieldErr(b.key+".rate", fmt.Errorf("%s.rate must be a number of requests a second above 0, or 0 (or left out) for the default", b.key))
		}
		if b.b.Burst < 0 {
			return fieldErr(b.key+".burst", fmt.Errorf("%s.burst must be at least 1, or 0 (or left out) for the default", b.key))
		}
	}
	if n := c.Scan.Concurrency; n < 0 || n > MaxScanConcurrency {
		return fieldErr("scan.concurrency", fmt.Errorf("scan.concurrency must be from 0 (one per CPU) to %d", MaxScanConcurrency))
	}
	if m := c.Database.Mmap; m != "" && !slices.Contains(store.MmapModes, strings.ToLower(m)) {
		return fieldErr("database.mmap", fmt.Errorf("database.mmap must be one of %s, got %q",
			strings.Join(store.MmapModes, ", "), m))
	}
	if d := c.Backups.Dir; d != "" && !filepath.IsAbs(d) {
		return fieldErr("backups.dir", fmt.Errorf("backups.dir must be an absolute path, got %q", d))
	}
	if r := c.Metadata.PreferredRegion(); r != "" && !slices.Contains(Regions, r) {
		return fieldErr("metadata.region", fmt.Errorf("metadata.region must be one of %s, got %q",
			strings.Join(Regions, ", "), c.Metadata.Region))
	}
	if mode := c.Metadata.ModeName(); !slices.Contains(MetadataModes, mode) {
		return fieldErr("metadata.mode", fmt.Errorf("metadata.mode must be one of %s, got %q",
			strings.Join(MetadataModes, ", "), c.Metadata.Mode))
	}
	if c.Metadata.Enabled {
		if c.Metadata.BaseURL == "" {
			return fieldErr("metadata.base_url", errors.New("metadata lookup requires metadata.base_url"))
		}
		if !c.Metadata.ValidBaseURL() {
			return fieldErr("metadata.base_url", fmt.Errorf("metadata.base_url must be an absolute http(s) URL, got %q", c.Metadata.BaseURL))
		}
	}
	return nil
}
