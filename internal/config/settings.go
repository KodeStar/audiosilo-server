package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/backup"
)

// field is one config.yaml key that an AUDIOSILO_* variable or the admin
// console's Settings can set. The table below is the one place that says which
// variable overrides a key, what the console calls it, and whether a change
// waits for a restart.
type field struct {
	key string            // its key in config.yaml, dotted ("tls.hosts")
	env string            // the AUDIOSILO_* variable that overrides it, if any
	ptr func(*Config) any // a pointer to the field in c

	// setting is the admin console's id for it, "<section>.<name>", which is also
	// where it sits in GET /admin/settings. Empty: not a console setting.
	setting  string
	restart  bool // read once at start: a saved change waits for a restart
	readOnly bool // shown in the console, changed only in config.yaml or the environment
	// fix normalizes and checks a value the console sent; Validate then checks the
	// config as a whole.
	fix func(c *Config) error
}

var fields = []field{
	{key: "name", setting: "general.name", ptr: func(c *Config) any { return &c.Name }, fix: fixName},
	{key: "public_url", env: "AUDIOSILO_PUBLIC_URL", setting: "general.public_url",
		ptr: func(c *Config) any { return &c.PublicURL }, fix: fixPublicURL},
	{key: "lan_url", env: "AUDIOSILO_LAN_URL", setting: "general.lan_url",
		ptr: func(c *Config) any { return &c.LANURL }, fix: fixLANURL},
	{key: "update_check", env: "AUDIOSILO_UPDATE_CHECK", setting: "general.update_check",
		ptr: func(c *Config) any { return &c.UpdateCheck }},
	{key: "activity.session_days", env: "AUDIOSILO_SESSION_DAYS", setting: "general.session_days",
		ptr: func(c *Config) any { return &c.Activity.SessionDays }},

	{key: "bind", env: "AUDIOSILO_BIND", setting: "network.bind", restart: true,
		ptr: func(c *Config) any { return &c.Bind }, fix: fixBind},
	{key: "tls.mode", env: "AUDIOSILO_TLS_MODE", setting: "network.tls_mode", restart: true,
		ptr: func(c *Config) any { return &c.TLS.Mode }},
	{key: "tls.hosts", env: "AUDIOSILO_TLS_HOSTS", setting: "network.tls_hosts", restart: true,
		ptr: func(c *Config) any { return &c.TLS.Hosts }, fix: fixHosts},
	{key: "trusted_proxies", env: "AUDIOSILO_TRUSTED_PROXIES", setting: "network.trusted_proxies",
		ptr: func(c *Config) any { return &c.TrustedProxies }, fix: fixProxies},
	{key: "cors_origins", env: "AUDIOSILO_CORS_ORIGINS", setting: "network.cors_origins",
		ptr: func(c *Config) any { return &c.CORSOrigins }, fix: fixOrigins},

	{key: "web_dir", env: "AUDIOSILO_WEB_DIR", setting: "players.web_dir", restart: true, readOnly: true,
		ptr: func(c *Config) any { return &c.WebDir }},
	{key: "app_links.apple_app_ids", setting: "players.apple_app_ids",
		ptr: func(c *Config) any { return &c.AppLinks.AppleAppIDs }, fix: fixAppleIDs},
	{key: "app_links.android_package", setting: "players.android_package",
		ptr: func(c *Config) any { return &c.AppLinks.AndroidPackage }, fix: fixAndroidPackage},
	{key: "app_links.android_sha256", setting: "players.android_sha256",
		ptr: func(c *Config) any { return &c.AppLinks.AndroidSHA256 }, fix: fixFingerprints},

	{key: "libraries", ptr: func(c *Config) any { return &c.Libraries }}, // launcher-pinned only

	{key: "max_upload_bytes", env: "AUDIOSILO_MAX_UPLOAD_BYTES",
		ptr: func(c *Config) any { return &c.MaxUploadBytes }},

	{key: "metadata.enabled", env: "AUDIOSILO_METADATA_ENABLED", setting: "metadata.enabled",
		ptr: func(c *Config) any { return &c.Metadata.Enabled }},
	{key: "metadata.base_url", env: "AUDIOSILO_METADATA_BASE_URL", setting: "metadata.base_url", restart: true,
		ptr: func(c *Config) any { return &c.Metadata.BaseURL }, fix: fixBaseURL},
	{key: "metadata.region", env: "AUDIOSILO_METADATA_REGION", setting: "metadata.region",
		ptr: func(c *Config) any { return &c.Metadata.Region }, fix: fixRegion},
	// The backend is chosen when the server starts (pkg/launcher builds the mirror),
	// like base_url.
	{key: "metadata.mode", env: "AUDIOSILO_METADATA_MODE", setting: "metadata.mode", restart: true,
		ptr: func(c *Config) any { return &c.Metadata.Mode }, fix: fixMode},

	{key: "demo.enabled", env: "AUDIOSILO_DEMO_ENABLED", setting: "demo.enabled", restart: true,
		ptr: func(c *Config) any { return &c.Demo.Enabled }},
	{key: "demo.library", env: "AUDIOSILO_DEMO_LIBRARY", setting: "demo.library",
		ptr: func(c *Config) any { return &c.Demo.Library }, fix: fixDemoLibrary},
	{key: "demo.max_users", env: "AUDIOSILO_DEMO_MAX_USERS", setting: "demo.max_users",
		ptr: func(c *Config) any { return &c.Demo.MaxUsers }, fix: fixMaxUsers},
	{key: "demo.idle_ttl", env: "AUDIOSILO_DEMO_IDLE_TTL", setting: "demo.idle_ttl", restart: true,
		ptr: func(c *Config) any { return &c.Demo.IdleTTL }, fix: fixIdleTTL},

	{key: "backups.schedule", env: "AUDIOSILO_BACKUP_SCHEDULE", setting: "backups.schedule",
		ptr: func(c *Config) any { return &c.Backups.Schedule }, fix: fixBackupSchedule},
	{key: "backups.keep", env: "AUDIOSILO_BACKUP_KEEP", setting: "backups.keep",
		ptr: func(c *Config) any { return &c.Backups.Keep }},
	// The folder is never set from the console: a backup holds every account's
	// password hash, and the schedule's retention deletes files in it.
	{key: "backups.dir", env: "AUDIOSILO_BACKUP_DIR", setting: "backups.dir", restart: true, readOnly: true,
		ptr: func(c *Config) any { return &c.Backups.Dir }},
}

func fieldByKey(key string) *field {
	for i := range fields {
		if fields[i].key == key {
			return &fields[i]
		}
	}
	return nil
}

// value is the field's current value in c; a nil list reads as an empty one, so
// the wire never says null for a list and an unset list equals an emptied one.
func (f *field) value(c *Config) any {
	v := reflect.ValueOf(f.ptr(c)).Elem().Interface()
	if l, ok := v.([]string); ok && l == nil {
		return []string{}
	}
	return v
}

// copyField sets dst's field key to src's.
func copyField(dst, src *Config, key string) {
	if f := fieldByKey(key); f != nil {
		reflect.ValueOf(f.ptr(dst)).Elem().Set(reflect.ValueOf(f.ptr(src)).Elem())
	}
}

// Clone returns a deep copy of c that can be changed without touching c.
func (c *Config) Clone() *Config {
	out := *c
	out.TLS.Hosts = slices.Clone(c.TLS.Hosts)
	out.TrustedProxies = slices.Clone(c.TrustedProxies)
	out.CORSOrigins = slices.Clone(c.CORSOrigins)
	out.AppLinks.AppleAppIDs = slices.Clone(c.AppLinks.AppleAppIDs)
	out.AppLinks.AndroidSHA256 = slices.Clone(c.AppLinks.AndroidSHA256)
	out.Libraries = slices.Clone(c.Libraries)
	if c.Demo.MaxUsers != nil {
		n := *c.Demo.MaxUsers
		out.Demo.MaxUsers = &n
	}
	return &out
}

// Pin marks a key as set by an embedding launcher (pkg/launcher's Options, which
// the desktop manager uses), so the admin console shows it as managed there
// instead of offering to change it. Call it while starting, before any Clone.
func (c *Config) Pin(key string) {
	if c.pinned == nil {
		c.pinned = map[string]bool{}
	}
	c.pinned[key] = true
}

// PinnedByLauncher is the lock reason Locked reports for a pinned key.
const PinnedByLauncher = "launcher"

// lockedBy is why the console can't change f: the AUDIOSILO_* variable that set
// it, PinnedByLauncher, or "" when it can.
func (c *Config) lockedBy(f *field) string {
	if env := c.fromEnv[f.key]; env != "" {
		return env
	}
	if c.pinned[f.key] {
		return PinnedByLauncher
	}
	return ""
}

// Settings is the console's view of the settings: section -> name -> value.
func (c *Config) Settings() map[string]map[string]any {
	out := map[string]map[string]any{}
	for i := range fields {
		f := &fields[i]
		if f.setting == "" {
			continue
		}
		section, name, _ := strings.Cut(f.setting, ".")
		if out[section] == nil {
			out[section] = map[string]any{}
		}
		out[section][name] = f.value(c)
	}
	return out
}

// Locked maps each setting the environment or the launcher sets to which: the
// AUDIOSILO_* variable, or PinnedByLauncher. A read-only setting is listed too
// when a variable sets it (the console shows what sets it).
func (c *Config) Locked() map[string]string {
	out := map[string]string{}
	for i := range fields {
		f := &fields[i]
		if f.setting == "" {
			continue
		}
		if by := c.lockedBy(f); by != "" {
			out[f.setting] = by
		}
	}
	return out
}

// RestartSettings lists the settings read only when the server starts.
func RestartSettings() []string {
	var out []string
	for _, f := range fields {
		if f.setting != "" && f.restart {
			out = append(out, f.setting)
		}
	}
	return out
}

// RestartPending lists the restart settings whose saved value (c) differs from
// the one the server started with (running).
func (c *Config) RestartPending(running *Config) []string {
	out := []string{}
	for i := range fields {
		f := &fields[i]
		if f.setting != "" && f.restart && !reflect.DeepEqual(f.value(c), f.value(running)) {
			out = append(out, f.setting)
		}
	}
	return out
}

// SettingChange is one setting a save changed, from what to what (the audit log).
type SettingChange struct {
	Setting string `json:"setting"`
	From    any    `json:"from"`
	To      any    `json:"to"`
}

// ChangedSettings lists the console settings whose value differs between cur and
// next, in id order. No setting holds a secret, so the values can be recorded.
func ChangedSettings(cur, next *Config) []SettingChange {
	out := []SettingChange{}
	for i := range fields {
		f := &fields[i]
		if f.setting == "" {
			continue
		}
		if from, to := f.value(cur), f.value(next); !reflect.DeepEqual(from, to) {
			out = append(out, SettingChange{Setting: f.setting, From: from, To: to})
		}
	}
	slices.SortFunc(out, func(a, b SettingChange) int { return strings.Compare(a.Setting, b.Setting) })
	return out
}

// Effective returns c with each restart setting as running has it: the config
// the server actually works with until it restarts with c.
func (c *Config) Effective(running *Config) *Config {
	out := c.Clone()
	for _, f := range fields {
		if f.restart {
			copyField(out, running, f.key)
		}
	}
	return out
}

// Reasons a settings change is refused (SettingError.Reason).
const (
	ReasonUnknown  = "unknown"   // no such setting
	ReasonReadOnly = "read_only" // shown, not changeable in the console
	ReasonLocked   = "locked"    // set by the environment or the launcher
	ReasonInvalid  = "invalid"   // the value doesn't validate
)

// SettingError is a refused settings change: which setting and why.
type SettingError struct {
	Setting string
	Reason  string
	Err     error
}

// Error says what is wrong with the value, without naming the setting (the
// caller has it in Setting), so a form can show it under the field as is.
func (e *SettingError) Error() string {
	switch {
	case e.Err != nil:
		return e.Err.Error()
	case e.Reason == ReasonLocked:
		return "set by an environment variable or the launcher; change it there"
	case e.Reason == ReasonReadOnly:
		return "can only be changed in config.yaml"
	}
	return "no such setting"
}

func (e *SettingError) Unwrap() error { return e.Err }

// Checks are what WithSettings can't tell from the config alone.
type Checks struct {
	// MetadataAvailable: the metadata service exists (base_url was valid when the
	// server started), so the lookup can be turned on.
	MetadataAvailable bool
	// LibraryExists reports whether a library has this name (demo.library).
	LibraryExists func(name string) (bool, error)
}

func fieldBySetting(id string) *field {
	for i := range fields {
		if fields[i].setting == id {
			return &fields[i]
		}
	}
	return nil
}

// WithSettings returns a copy of c with the console's changes applied
// (section -> name -> JSON value), normalized and validated, or a *SettingError
// naming the first refused setting (in id order). c itself is never changed.
func (c *Config) WithSettings(patch map[string]map[string]json.RawMessage, checks Checks) (*Config, error) {
	var ids []string
	for section, fs := range patch {
		for name := range fs {
			ids = append(ids, section+"."+name)
		}
	}
	slices.Sort(ids)

	next := c.Clone()
	changed := make([]*field, 0, len(ids))
	for _, id := range ids {
		f := fieldBySetting(id)
		switch {
		case f == nil:
			return nil, &SettingError{Setting: id, Reason: ReasonUnknown}
		case f.readOnly:
			return nil, &SettingError{Setting: id, Reason: ReasonReadOnly}
		case c.lockedBy(f) != "":
			return nil, &SettingError{Setting: id, Reason: ReasonLocked}
		}
		section, name, _ := strings.Cut(id, ".")
		raw := patch[section][name]
		// JSON null leaves a value as it is, which would save "nothing" as a success:
		// only a setting that can be unset (a pointer, like demo.max_users) takes it.
		if string(bytes.TrimSpace(raw)) == "null" && reflect.ValueOf(f.ptr(next)).Elem().Kind() != reflect.Pointer {
			return nil, &SettingError{Setting: id, Reason: ReasonInvalid, Err: errors.New("enter a value")}
		}
		if err := json.Unmarshal(raw, f.ptr(next)); err != nil {
			return nil, &SettingError{Setting: id, Reason: ReasonInvalid, Err: errors.New("wrong type of value")}
		}
		changed = append(changed, f)
	}
	for _, f := range changed {
		if f.fix == nil {
			continue
		}
		if err := f.fix(next); err != nil {
			return nil, &SettingError{Setting: f.setting, Reason: ReasonInvalid, Err: err}
		}
	}
	if err := next.Validate(); err != nil {
		id := ""
		var fe *FieldError
		if errors.As(err, &fe) {
			if f := fieldByKey(fe.Key); f != nil {
				id = f.setting
			}
		}
		if id == "" && len(changed) > 0 {
			id = changed[0].setting
		}
		return nil, &SettingError{Setting: id, Reason: ReasonInvalid, Err: err}
	}
	// config.yaml keeps its own values for what the environment sets (Save), so the
	// change must also leave the file valid on its own: else it saves a file that
	// stops the server from starting once the variable is removed (demo mode on in
	// the file, its library only in AUDIOSILO_DEMO_LIBRARY). A file that already
	// leans on the environment doesn't block unrelated changes.
	if err := next.asSaved().Validate(); err != nil && len(changed) > 0 && c.asSaved().Validate() == nil {
		id := changed[0].setting
		var fe *FieldError
		if errors.As(err, &fe) && c.fromEnv[fe.Key] != "" {
			err = fmt.Errorf("%w in config.yaml, where %s doesn't apply: set it there too, or set this the same way",
				err, c.fromEnv[fe.Key])
		}
		return nil, &SettingError{Setting: id, Reason: ReasonInvalid, Err: err}
	}
	if next.Metadata.Enabled && !c.Metadata.Enabled && !checks.MetadataAvailable {
		return nil, &SettingError{Setting: "metadata.enabled", Reason: ReasonInvalid,
			Err: errors.New("no metadata service is configured: set its address, then restart the server")}
	}
	if lib := next.Demo.Library; lib != "" && lib != c.Demo.Library && checks.LibraryExists != nil {
		ok, err := checks.LibraryExists(lib)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &SettingError{Setting: "demo.library", Reason: ReasonInvalid, Err: errors.New("no library has that name")}
		}
	}
	return next, nil
}

// MaxServerName is the longest server name, in characters.
const MaxServerName = 64

func fixName(c *Config) error {
	c.Name = strings.TrimSpace(c.Name)
	if utf8.RuneCountInString(c.Name) > MaxServerName {
		return fmt.Errorf("a name can be at most %d characters", MaxServerName)
	}
	if strings.IndexFunc(c.Name, unicode.IsControl) >= 0 {
		return errors.New("a name can't contain line breaks or control characters")
	}
	return nil
}

// httpURL normalizes an absolute http(s) URL with no query, fragment or
// credentials, without a trailing slash; "" stays "".
func httpURL(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("must be an address starting with https:// or http://")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("must be a plain address, with no ?, # or user name")
	}
	return s, nil
}

func fixPublicURL(c *Config) (err error) {
	c.PublicURL, err = httpURL(c.PublicURL)
	return err
}

func fixLANURL(c *Config) (err error) {
	c.LANURL, err = httpURL(c.LANURL)
	return err
}

func fixBaseURL(c *Config) (err error) {
	c.Metadata.BaseURL, err = httpURL(c.Metadata.BaseURL)
	return err
}

// fixRegion stores the marketplace as the community metadata names it ("UK" ->
// "uk"); Validate checks it is one.
func fixRegion(c *Config) error {
	c.Metadata.Region = c.Metadata.PreferredRegion()
	return nil
}

// fixMode stores the mode as the server reads it ("Mirror " -> "mirror", "" ->
// "remote"); Validate checks it is one.
func fixMode(c *Config) error {
	c.Metadata.Mode = c.Metadata.ModeName()
	return nil
}

func fixBind(c *Config) error {
	c.Bind = strings.TrimSpace(c.Bind)
	_, port, err := net.SplitHostPort(c.Bind)
	if err != nil {
		return errors.New("must be host:port, like 0.0.0.0:8080 or :8080")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("the port must be a number from 1 to 65535")
	}
	return nil
}

// maxListEntries bounds every list setting.
const maxListEntries = 50

// cleanList trims each entry, normalizes it, and drops empty ones and repeats;
// more than maxListEntries is an error naming what the list holds.
func cleanList(l []string, norm func(string) string, noun string) ([]string, error) {
	out := []string{}
	for _, s := range l {
		s = strings.TrimSpace(s)
		if norm != nil {
			s = norm(s)
		}
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) > maxListEntries {
		return nil, fmt.Errorf("at most %d %s", maxListEntries, noun)
	}
	return out, nil
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

func fixHosts(c *Config) error {
	hosts, err := cleanList(c.TLS.Hosts, strings.ToLower, "names")
	if err != nil {
		return err
	}
	c.TLS.Hosts = hosts
	for _, h := range c.TLS.Hosts {
		if len(h) > 253 || !hostnameRE.MatchString(h) {
			return fmt.Errorf("%q isn't a host name (no https://, port or path)", h)
		}
	}
	return nil
}

// fixProxies accepts a bare IP as the one-address range ("10.0.0.2" ->
// "10.0.0.2/32"); Validate checks the ranges.
func fixProxies(c *Config) error {
	proxies, err := cleanList(c.TrustedProxies, func(s string) string {
		if ip := net.ParseIP(s); ip != nil {
			if ip.To4() != nil {
				return s + "/32"
			}
			return s + "/128"
		}
		return s
	}, "ranges")
	c.TrustedProxies = proxies
	return err
}

func fixOrigins(c *Config) error {
	var bad string
	origins, err := cleanList(c.CORSOrigins, func(s string) string {
		if s == "*" {
			return s
		}
		u, err := url.Parse(strings.TrimRight(s, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			if bad == "" {
				bad = s
			}
			return s
		}
		return strings.ToLower(u.Scheme + "://" + u.Host)
	}, "origins")
	if bad != "" {
		return fmt.Errorf("%q isn't a web origin (like https://example.com or http://localhost:8081)", bad)
	}
	c.CORSOrigins = origins
	return err
}

var (
	appleIDRE     = regexp.MustCompile(`^[A-Z0-9]{10}\.[A-Za-z0-9][A-Za-z0-9.-]*$`)
	androidPkgRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	fingerprintRE = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)
)

func fixAppleIDs(c *Config) error {
	ids, err := cleanList(c.AppLinks.AppleAppIDs, nil, "app IDs")
	if err != nil {
		return err
	}
	c.AppLinks.AppleAppIDs = ids
	for _, id := range c.AppLinks.AppleAppIDs {
		if !appleIDRE.MatchString(id) {
			return fmt.Errorf("%q isn't TEAMID.bundle.id (a 10-character team ID, a dot, the bundle ID)", id)
		}
	}
	return nil
}

func fixAndroidPackage(c *Config) error {
	c.AppLinks.AndroidPackage = strings.TrimSpace(c.AppLinks.AndroidPackage)
	if p := c.AppLinks.AndroidPackage; p != "" && (len(p) > 255 || !androidPkgRE.MatchString(p)) {
		return fmt.Errorf("%q isn't an Android package name (like com.example.app)", p)
	}
	return nil
}

func fixFingerprints(c *Config) error {
	fps, err := cleanList(c.AppLinks.AndroidSHA256, strings.ToUpper, "fingerprints")
	if err != nil {
		return err
	}
	c.AppLinks.AndroidSHA256 = fps
	for _, fp := range c.AppLinks.AndroidSHA256 {
		if !fingerprintRE.MatchString(fp) {
			return fmt.Errorf("%q isn't a SHA-256 fingerprint (32 pairs of hex digits joined by colons)", fp)
		}
	}
	return nil
}

func fixDemoLibrary(c *Config) error {
	c.Demo.Library = strings.TrimSpace(c.Demo.Library)
	return nil
}

// MaxDemoUsers bounds demo.max_users when set from the console.
const MaxDemoUsers = 100000

func fixMaxUsers(c *Config) error {
	if n := c.Demo.MaxUsers; n != nil && (*n < 0 || *n > MaxDemoUsers) {
		return fmt.Errorf("must be from 0 (no limit) to %d", MaxDemoUsers)
	}
	return nil
}

// fixBackupSchedule stores the schedule in its canonical form ("daily:03:00").
func fixBackupSchedule(c *Config) error {
	sch, err := backup.ParseSchedule(c.Backups.Schedule)
	if err != nil {
		return err
	}
	c.Backups.Schedule = sch.String()
	return nil
}

func fixIdleTTL(c *Config) error {
	c.Demo.IdleTTL = strings.TrimSpace(c.Demo.IdleTTL)
	if c.Demo.IdleTTL == "" {
		return nil
	}
	if d, err := time.ParseDuration(c.Demo.IdleTTL); err != nil || d <= 0 {
		return errors.New("must be a length of time like 30m, 2h or 24h")
	}
	return nil
}
