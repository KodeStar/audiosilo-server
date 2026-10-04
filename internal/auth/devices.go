package auth

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
)

// ClientHeader is the request header a player names itself in:
// "<app>/<version> (<platform>)", e.g. "AudioSilo/1.4.2 (ios)"; the version and
// platform are optional (the admin console, which ships inside the server, sends
// "AudioSilo Admin (web)"). The server keeps
// the newest one per token, so the admin console can say which app (and which
// build) each device runs. Clients released before the header send nothing and
// show as an unknown app.
const ClientHeader = "X-AudioSilo-Client"

// ClientInfo is the parsed ClientHeader. App is empty when the token never sent
// one.
type ClientInfo struct {
	App      string `json:"app"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

// clientPattern bounds what the header may hold: it comes from any signed-in
// client and is shown to admins, so it is a short name, a version and an optional
// platform made of plain characters, never free text.
var clientPattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9 ._-]{0,39}?)(?:/([A-Za-z0-9][A-Za-z0-9.+_-]{0,31}))?(?: \(([A-Za-z0-9][A-Za-z0-9 ._-]{0,23})\))?$`)

// ParseClient parses a ClientHeader value. ok is false for an empty or malformed
// value, which callers treat as "this request didn't say".
func ParseClient(v string) (ClientInfo, bool) {
	m := clientPattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return ClientInfo{}, false
	}
	app := strings.TrimSpace(m[1])
	return ClientInfo{App: app, Version: m[2], Platform: strings.ToLower(strings.TrimSpace(m[3]))}, true
}

// Presence is what one authenticated request says about the device behind its
// token: the client address (already resolved through the trusted proxies) and
// the app, when the request carried a valid ClientHeader (zero otherwise).
type Presence struct {
	IP     string
	Client ClientInfo
}

// Credential is the token that authenticated a request: its id (what the admin
// console calls a device), its kind, the device name given at sign-in, and the
// app it last reported.
type Credential struct {
	ID         int64
	Kind       string
	DeviceName string
	Client     ClientInfo
}

// Device is a signed-in token as the admin console lists it: a session (a paired
// phone, a browser) or a personal API key. Pairing tokens are not devices.
type Device struct {
	ID       int64  `json:"id"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Kind     string `json:"kind"` // session | api
	// Name is the device name the player sent at sign-in (an API key's label).
	Name string `json:"name"`
	// Client is null until the token makes a request naming its app.
	Client    *ClientInfo `json:"client"`
	CreatedAt string      `json:"created_at"`
	LastSeen  *string     `json:"last_seen"`
	// LastIP is the address of the token's newest request ("" before any).
	LastIP string `json:"last_ip"`
	// Current marks the token making this request (set by the transport).
	Current bool `json:"current"`
}

// ListDevices returns the live (not revoked, not expired) session and API-key
// tokens of one user, or of everyone when userID is 0, most recently seen first.
func (s *Service) ListDevices(ctx context.Context, userID int64) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.user_id, u.username, t.kind, t.device_name, t.client_app, t.client_version,
		        t.client_platform, t.created_at, t.last_seen, t.last_ip, t.expires_at
		   FROM tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.kind IN (?, ?) AND t.revoked = 0 AND (?3 = 0 OR t.user_id = ?3)
		  ORDER BY COALESCE(t.last_seen, t.created_at) DESC, t.id DESC`,
		KindSession, KindAPI, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var (
			d        Device
			c        ClientInfo
			lastSeen sql.NullString
			expires  sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.UserID, &d.Username, &d.Kind, &d.Name, &c.App, &c.Version,
			&c.Platform, &d.CreatedAt, &lastSeen, &d.LastIP, &expires); err != nil {
			return nil, err
		}
		if s.pastExpiry(expires) {
			continue
		}
		if c.App != "" {
			d.Client = &c
		}
		if lastSeen.Valid {
			d.LastSeen = &lastSeen.String
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice signs one device out: it revokes a live session or API-key token
// by id, whoever owns it (the admin console's "Sign out this device"). Other
// devices of the same person stay signed in. An id naming no live session or key
// (a pairing token included) returns ErrNotFound.
func (s *Service) RevokeDevice(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE id = ? AND kind IN (?, ?) AND revoked = 0`,
		id, KindSession, KindAPI)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetRevokedAddresses blanks the last address of every signed-out or expired
// token, so a device's address lives only as long as its sign-in (the token row
// itself stays, as revoked tokens always have). expires_at is RFC3339 UTC like
// s.ts(), so the comparison is chronological. Run by the daily retention job.
func (s *Service) ForgetRevokedAddresses(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_ip = ''
		  WHERE last_ip <> '' AND (revoked = 1 OR (expires_at IS NOT NULL AND expires_at <= ?))`, s.ts())
	return err
}

// DeviceLabel names a token for the audit log: its owner's username, the device
// name (an API key's label) and its kind. ErrNotFound when there is no such token.
func (s *Service) DeviceLabel(ctx context.Context, id int64) (username, name, kind string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT u.username, t.device_name, t.kind FROM tokens t JOIN users u ON u.id = t.user_id WHERE t.id = ?`, id).
		Scan(&username, &name, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return username, name, kind, err
}
