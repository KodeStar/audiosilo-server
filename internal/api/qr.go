package api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// PairingPayload is returned by the redeem/pair endpoints so a client can render
// a QR code (PNGDataURI) or deep-link directly (URI). The QR encodes URI.
type PairingPayload struct {
	ServerName   string   `json:"server_name"`
	BaseURL      string   `json:"base_url"`
	PairingToken string   `json:"pairing_token"`
	URI          string   `json:"uri"` // audiosilo:// deep link encoded in the QR
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
	deepLink := "audiosilo://pair?" + url.Values{
		"url":   {base},
		"token": {token},
	}.Encode()

	png, err := qrcode.Encode(deepLink, qrcode.Medium, 512)
	if err != nil {
		return nil, err
	}
	return &PairingPayload{
		ServerName:   "AudioSilo",
		BaseURL:      base,
		PairingToken: token,
		URI:          deepLink,
		PNGDataURI:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		Links: AppLinks{
			Web:   base + "/web",
			Admin: base + "/admin",
		},
	}, nil
}
