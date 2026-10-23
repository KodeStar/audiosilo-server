package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// The admin console's "Set up your server" flow (FIRST-RUN-PLAN.md Phase F1).
// Admin only and console only: no capability flag, the player never asks. Each
// step writes through the ordinary admin APIs (settings, libraries); these
// routes hold only the flow's progress and the folder preview.

// setupEnvelope is GET /admin/setup's answer.
type setupEnvelope struct {
	State catalog.SetupState `json:"state"`
	// Request is how this request reached the server, for the reach step to
	// compare with the address the browser is using.
	Request setupRequest `json:"request"`
	// ConfigLibraries are the names of the libraries config.yaml or the
	// environment declares (added at every start, so the flow shows them as
	// already added).
	ConfigLibraries []string `json:"config_libraries"`
	// MountHints are folders mounted into the server that may hold books
	// (library.MountHints; [] off Linux), read only while the folder step may
	// still be shown: the flow is not finished, or its folder step was skipped
	// (reopened from the Overview card). [] otherwise: only that step offers them.
	MountHints []string `json:"mount_hints"`
}

// setupRequest is the request as the server saw it. The server never trusts
// X-Forwarded-Host or -Proto, so host and scheme are the direct connection's;
// proxied is any forwarding header (proxied), and peer, sent only then, is the
// address of whatever connected (the proxy), for trusted_proxies.
type setupRequest struct {
	Host        string `json:"host"`
	Scheme      string `json:"scheme"`
	Proxied     bool   `json:"proxied"`
	Peer        string `json:"peer,omitempty"`
	PeerTrusted bool   `json:"peer_trusted"`
}

// handleGetSetup is GET /admin/setup: the flow's progress and what its steps
// need to know about this server.
func (a *API) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	st, err := a.cat.SetupState(r.Context())
	if err != nil {
		a.writeCatalogError(w, err, "setup: read", "could not read the setup state")
		return
	}
	cfg := a.config()
	peer := peerIP(r.RemoteAddr)
	req := setupRequest{Host: r.Host, Scheme: requestScheme(r), Proxied: proxied(r), PeerTrusted: isTrusted(peer, cfg.proxies)}
	if req.Proxied {
		req.Peer = peer
	}
	names := make([]string, 0, len(cfg.Libraries))
	for _, l := range cfg.Libraries {
		names = append(names, l.Name)
	}
	hints := []string{}
	if !st.Finished || st.Steps["folder"] == catalog.SetupStepSkipped {
		hints = a.mountHints(cfg.DataDir)
	}
	writeJSON(w, http.StatusOK, setupEnvelope{
		State: st, Request: req, ConfigLibraries: names, MountHints: hints,
	})
}

// handleUpdateSetup is PUT /admin/setup {finished?, dismissed?, steps?: {id:
// "done"|"skipped"}}: merges the fields given and returns the state. 400
// invalid_setup_step for a step id or outcome it doesn't know.
func (a *API) handleUpdateSetup(w http.ResponseWriter, r *http.Request) {
	var body catalog.SetupPatch
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	st, err := a.cat.UpdateSetupState(r.Context(), body)
	if err != nil {
		a.writeCatalogError(w, err, "setup: save", "could not save the setup state")
		return
	}
	details := map[string]any{}
	if body.Finished != nil {
		details["finished"] = *body.Finished
	}
	if body.Dismissed != nil {
		details["dismissed"] = *body.Dismissed
	}
	if len(body.Steps) > 0 {
		details["steps"] = body.Steps
	}
	a.audit(r, "settings.setup", "", details)
	writeJSON(w, http.StatusOK, st)
}

// setupPreview is POST /admin/setup/preview's answer: the folder's preview
// plus same_root, the name of a library already at exactly this root (a
// warning; create allows it), null for none.
type setupPreview struct {
	*library.Preview
	SameRoot *string `json:"same_root"`
}

// handleSetupPreview is POST /admin/setup/preview {root, ignore_patterns?}: what
// a books library at root would hold and a sample of its books named from the
// tags and from the folders (library.Preview). It checks the folder as the
// folder picker does (library.CheckRoot: 400 path_not_absolute, 404
// folder_unreadable; library create checks neither), the root against the other
// libraries as library create does (catalog.CheckLibraryRoot: 409
// root_overlaps) and the ignore rules as a library edit does (400
// invalid_pattern). Reads only; never a file's contents.
func (a *API) handleSetupPreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root           string   `json:"root"`
		IgnorePatterns []string `json:"ignore_patterns"`
	}
	if err := decodeJSON(r, &body, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if body.Root == "" {
		writeError(w, http.StatusBadRequest, "root is required")
		return
	}
	root, err := library.CheckRoot(body.Root)
	if err != nil {
		writeFolderError(w, err)
		return
	}
	same, err := a.cat.CheckLibraryRoot(r.Context(), root)
	if err != nil {
		a.writeCatalogError(w, err, "setup: check root", "could not check the folder")
		return
	}
	p, err := a.scanner.Preview(r.Context(), root, body.IgnorePatterns, library.PreviewLimits{})
	if err != nil {
		a.writeCatalogError(w, err, "setup: preview", "could not read the folder")
		return
	}
	out := setupPreview{Preview: p}
	if same != "" {
		out.SameRoot = &same
	}
	writeJSON(w, http.StatusOK, out)
}
