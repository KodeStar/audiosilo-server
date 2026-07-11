package api

import "net/http"

// Runtime-toggleable server settings, surfaced in the admin console. Transport
// only: the flag lives on the API (metaEnabled, an atomic.Bool) and the durable
// value is persisted to config.yaml via config.Save(). The envelope is an object
// keyed by feature so future settings can join it without reshaping the wire
// contract.

// metadataOn reports whether the community metadata lookup is live: the service
// was constructed (base_url is valid) AND the runtime flag is on. The /meta
// handler and the `metadata` capability both gate on this.
func (a *API) metadataOn() bool { return a.meta != nil && a.metaEnabled.Load() }

// settingsEnvelope builds the GET/PATCH response body. `available` reports
// whether the service is constructed at all (base_url valid); when false the
// feature cannot be enabled.
func (a *API) settingsEnvelope() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"enabled":   a.metaEnabled.Load(),
			"base_url":  a.cfg.Metadata.BaseURL,
			"available": a.meta != nil,
		},
	}
}

// handleGetSettings returns the current runtime settings (admin only).
func (a *API) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.settingsEnvelope())
}

// handleUpdateSettings flips runtime settings and persists them (admin only).
// Fields are pointers so an absent field is left unchanged. Enabling the metadata
// lookup when no service is configured (empty/invalid base_url) is a 400.
func (a *API) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Metadata *struct {
			Enabled *bool `json:"enabled"`
		} `json:"metadata"`
	}
	if err := decodeJSON(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if req.Metadata != nil && req.Metadata.Enabled != nil {
		want := *req.Metadata.Enabled
		if want && a.meta == nil {
			writeError(w, http.StatusBadRequest,
				"metadata lookup is unavailable: set metadata.base_url to an absolute http(s) URL in the server config first")
			return
		}
		// Serialize the mutate+persist so concurrent admin PATCHes don't race on the
		// config struct or the config.yaml write.
		a.settingsMu.Lock()
		prev := a.cfg.Metadata.Enabled
		a.cfg.Metadata.Enabled = want
		if err := a.cfg.Save(); err != nil {
			a.cfg.Metadata.Enabled = prev // roll back the in-memory change on a failed persist
			a.settingsMu.Unlock()
			a.log.Error("persist settings failed", "err", err)
			writeError(w, http.StatusInternalServerError, "could not save settings")
			return
		}
		a.metaEnabled.Store(want)
		a.settingsMu.Unlock()
	}

	writeJSON(w, http.StatusOK, a.settingsEnvelope())
}
