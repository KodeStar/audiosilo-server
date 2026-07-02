package api

import "net/http"

// Well-known association files let a domain pointed at this server deep-link
// straight into the installed mobile app (iOS Universal Links / Android App
// Links). They are public and config-driven: when the relevant identifiers are
// unset the endpoints 404 and clients fall back to the embedded web player. Note
// that the app build must also claim the domain, so these only enable auto-launch
// for domains the shipped app knows about - see config.AppLinkConfig.

// appLinkPaths are the URL paths that should open the app: the QR/pairing handoff
// and the copy-invite connect page.
var appLinkPaths = []string{"/web/connect*", "/connect*"}

// handleAppleAppSiteAssociation serves /.well-known/apple-app-site-association.
func (a *API) handleAppleAppSiteAssociation(w http.ResponseWriter, r *http.Request) {
	ids := a.cfg.AppLinks.AppleAppIDs
	if len(ids) == 0 {
		http.NotFound(w, r)
		return
	}
	components := make([]map[string]string, 0, len(appLinkPaths))
	for _, p := range appLinkPaths {
		components = append(components, map[string]string{"/": p})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applinks": map[string]any{
			"apps": []string{},
			"details": []map[string]any{{
				"appIDs":     ids,
				"components": components,
			}},
		},
	})
}

// handleAssetLinks serves /.well-known/assetlinks.json for Android App Links.
func (a *API) handleAssetLinks(w http.ResponseWriter, r *http.Request) {
	al := a.cfg.AppLinks
	if al.AndroidPackage == "" || len(al.AndroidSHA256) == 0 {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, []map[string]any{{
		"relation": []string{"delegate_permission/common.handle_all_urls"},
		"target": map[string]any{
			"namespace":                "android_app",
			"package_name":             al.AndroidPackage,
			"sha256_cert_fingerprints": al.AndroidSHA256,
		},
	}})
}
