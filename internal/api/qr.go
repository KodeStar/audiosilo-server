package api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// PairingPayload is returned by the redeem/pair endpoints so a client can render
// a QR code (PNGDataURI) or deep-link directly. The QR encodes WebURL: an HTTPS
// link that opens the native app when it claims the domain (iOS Universal / Android
// App Link) and otherwise opens the embedded web player's connect route, which
// exchanges the pairing token. URI is the custom-scheme equivalent for an explicit
// "Open in app" action (custom schemes are not domain-bound, so they launch an
// installed app on any self-hosted domain).
type PairingPayload struct {
	ServerName   string   `json:"server_name"`
	BaseURL      string   `json:"base_url"`
	PairingToken string   `json:"pairing_token"`
	URI          string   `json:"uri"`     // audiosilo://connect?... custom-scheme deep link
	WebURL       string   `json:"web_url"` // https://<base>/web/connect?token=... (encoded in the QR)
	PNGDataURI   string   `json:"qr_png_data_uri"`
	Links        AppLinks `json:"links"`
}

// AppLinks points clients at the ways to connect. Mobile app stores are
// placeholders until those apps ship.
type AppLinks struct {
	Web     string `json:"web"`
	Admin   string `json:"admin"`
	IOS     string `json:"ios,omitempty"`
	Android string `json:"android,omitempty"`
}

// inviteURL builds the shareable copy-invite link. The auth code rides in the URL
// fragment so it never reaches the server (and so never lands in access logs): the
// connect page reads it client-side and redeems via POST.
func (a *API) inviteURL(r *http.Request, code string) string {
	return a.baseURL(r) + "/connect#code=" + code
}

// baseURL determines the externally reachable base URL: configured PublicURL
// wins; otherwise it is derived from the request.
func (a *API) baseURL(r *http.Request) string {
	if a.cfg.PublicURL != "" {
		return strings.TrimRight(a.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// buildPairing constructs the pairing payload (and QR PNG) for a token.
func (a *API) buildPairing(r *http.Request, token string) (*PairingPayload, error) {
	base := a.baseURL(r)
	// HTTPS handoff encoded in the QR: scanning it opens the native app when the
	// app claims this domain (iOS Universal / Android App Link), otherwise it opens
	// the embedded web player's connect route, which exchanges the pairing token.
	webURL := base + "/web/connect?" + url.Values{"token": {token}}.Encode()
	// Custom-scheme deep link for an explicit "Open in app" button. Custom schemes
	// are not domain-bound, so this launches an installed app on any self-hosted
	// domain. The pairing token is single-use, so whichever client consumes the
	// link performs the one exchange.
	appURI := "audiosilo://connect?" + url.Values{
		"server": {base},
		"token":  {token},
	}.Encode()

	png, err := qrcode.Encode(webURL, qrcode.Medium, 512)
	if err != nil {
		return nil, err
	}
	return &PairingPayload{
		ServerName:   "AudioSilo",
		BaseURL:      base,
		PairingToken: token,
		URI:          appURI,
		WebURL:       webURL,
		PNGDataURI:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		Links: AppLinks{
			Web:   base + "/web",
			Admin: base + "/admin",
		},
	}, nil
}
