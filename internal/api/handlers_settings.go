package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// Server settings, surfaced in the admin console's Server > Settings. Transport
// only: config.Config's settings table says what each setting is, how it is
// checked, which AUDIOSILO_* variable overrides it and whether it waits for a
// restart. A save builds a new config, writes config.yaml and swaps it in whole
// (a.live), so a setting that applies at once does so for the next request; a
// restart setting is saved and listed under restart_pending until the server
// starts again with it.

// metadataOn reports whether the community metadata lookup is live: the service
// was constructed (base_url was valid at start) AND it is switched on. The /meta
// handler and the `metadata` capability both gate on this.
func (a *API) metadataOn() bool { return a.meta != nil && a.config().Metadata.Enabled }

// settingsEnvelope is the GET/PATCH answer: each section's saved settings, plus
// what the console needs to show them honestly.
//
//	{"general": {...}, "network": {...}, "players": {...}, "metadata": {...}, "demo": {...},
//	 "locked": {"network.tls_mode": "AUDIOSILO_TLS_MODE"},
//	 "restart_settings": ["network.bind", ...], "restart_pending": ["network.bind"]}
//
// Read-only facts ride along in their sections: metadata.available (the service
// exists, so the switch can turn on), players.web_player (where /web is served
// from: "embedded", "dir" or ""), demo.max_users_default (the cap while max_users
// is null).
func (a *API) settingsEnvelope() map[string]any {
	saved := a.config().saved
	sections := saved.Settings()
	sections["metadata"]["available"] = a.meta != nil
	sections["players"]["web_player"] = a.playerSource
	sections["demo"]["max_users_default"] = config.DefaultDemoMaxUsers
	out := map[string]any{
		"locked":           saved.Locked(),
		"restart_settings": config.RestartSettings(),
		"restart_pending":  saved.RestartPending(a.boot),
	}
	for name, fields := range sections {
		out[name] = fields
	}
	return out
}

// handleGetSettings returns the settings (admin only).
func (a *API) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.settingsEnvelope())
}

// handleUpdateSettings changes settings and persists them (admin only). The body
// has the GET envelope's shape with only the settings to change:
// {"general": {"name": "Hearthside"}}. All or nothing: one refused setting (400
// invalid_setting / unknown_setting / setting_read_only, 409 setting_locked, each
// with "field") leaves everything as it was.
func (a *API) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]map[string]json.RawMessage
	if err := decodeJSON(r, &patch, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	// One save at a time: read, change, write config.yaml, swap.
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	cur := a.config().saved
	next, err := cur.WithSettings(patch, config.Checks{
		MetadataAvailable: a.meta != nil,
		LibraryExists: func(name string) (bool, error) {
			_, err := a.cat.GetLibraryByName(r.Context(), name)
			if errors.Is(err, catalog.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		},
	})
	if err != nil {
		a.writeCatalogError(w, err, "settings: apply", "could not save settings")
		return
	}
	if err := next.Save(); err != nil {
		a.log.Error("persist settings failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save settings")
		return
	}
	a.live.Store(newLiveConfig(next, a.boot))
	if a.rt.Updates != nil && next.UpdateCheck != cur.UpdateCheck {
		a.rt.Updates.SetEnabled(next.UpdateCheck)
	}
	if a.rt.Backups != nil && next.Backups != cur.Backups {
		a.rt.Backups.SetSettings(next.Backups.Schedule, next.Backups.Keep)
	}
	a.audit(r, "settings.update", "", map[string]any{"changes": config.ChangedSettings(cur, next)})
	writeJSON(w, http.StatusOK, a.settingsEnvelope())
}
