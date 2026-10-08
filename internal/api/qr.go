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
// installed app on any self-hosted domain). The pairing token is as redeemable as
// its origin: redeemed from an invite it inherits the invite's uses and expiry (one
// QR can pair several devices); from a recovery code it carries a short TTL of its
// own; from /auth/pair or the demo flow it is single-use and lasts pairingTTL.
type PairingPayload struct {
	ServerName   string   `json:"server_name"`
	BaseURL      string   `json:"base_url"`
	PairingToken string   `json:"pairing_token"`
	URI          string   `json:"uri"`     // audiosilo://connect?... custom-scheme deep link
	WebURL       string   `json:"web_url"` // https://<base>/web/connect?token=... (encoded in the QR)
	PNGDataURI   string   `json:"qr_png_data_uri"`
	Links        AppLinks `json:"links"`
	// CodeExpiresAt/UsesRemaining describe the parent invite when the token was
	// minted by redeeming one, so the connect page can say how far the QR goes.
	// Advisory: concurrent exchanges may consume uses after the redeem. Empty/nil
	// for recovery codes, /auth/pair and demo tokens (and nil = unlimited).
	CodeExpiresAt string `json:"code_expires_at,omitempty"`
	UsesRemaining *int   `json:"uses_remaining,omitempty"`
	// Addresses is the server's home and away addresses (the `addresses`
	// capability), also carried by URI and WebURL as home=/away= params; nil when
	// neither is known.
	Addresses *Addresses `json:"addresses,omitempty"`
}

// Addresses is the server's address on the household network (Home) and the one
// that works from anywhere (Away, the public_url). A native app pairs with either
// and switches to Home when it can reach it. Either may be empty.
type Addresses struct {
	Home string `json:"home,omitempty"`
	Away string `json:"away,omitempty"`
}

// addresses is the server's home and away addresses as seen from r: the
// configured lan_url and public_url, else a home address derived from the
// request's own Host when that is a home-network one (config.Addresses). Like
// baseURL it does not trust X-Forwarded-*, and a proxied request (one carrying
// any forwarding header) derives no home address: its Host and scheme are the
// proxy's upstream (a container name, a bridge IP, plain http behind TLS), not
// an address a device can use.
func (a *API) addresses(r *http.Request) Addresses {
	host := r.Host
	if proxied(r) {
		host = ""
	}
	home, away := a.config().Addresses(requestScheme(r), host)
	return Addresses{Home: home, Away: away}
}

// proxied reports whether r passed through a reverse proxy: it carries a
// forwarding header. A device on the home network talking to the server
// directly sends none.
func proxied(r *http.Request) bool {
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// pairingAddresses is addresses for a pairing payload: nil when both are empty.
func (a *API) pairingAddresses(r *http.Request) *Addresses {
	if ad := a.addresses(r); ad != (Addresses{}) {
		return &ad
	}
	return nil
}

// query is the addresses as link params (home=, away=), only those set; "" for
// none, else a leading "&" so it appends to a link's existing query.
func (ad *Addresses) query() string {
	if ad == nil {
		return ""
	}
	v := url.Values{}
	if ad.Home != "" {
		v.Set("home", ad.Home)
	}
	if ad.Away != "" {
		v.Set("away", ad.Away)
	}
	if len(v) == 0 {
		return ""
	}
	return "&" + v.Encode()
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
	if u := a.config().PublicURL; u != "" {
		return strings.TrimRight(u, "/")
	}
	return requestScheme(r) + "://" + r.Host
}

// requestScheme is the scheme r arrived over: https on a TLS connection, else http.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// buildPairing constructs the pairing payload (and QR PNG) for a token.
func (a *API) buildPairing(r *http.Request, token string) (*PairingPayload, error) {
	base := a.baseURL(r)
	addrs := a.pairingAddresses(r)
	// HTTPS handoff encoded in the QR: scanning it opens the native app when the
	// app claims this domain (iOS Universal / Android App Link), otherwise it opens
	// the embedded web player's connect route, which exchanges the pairing token.
	// The home/away params come after the existing ones, so a link without them
	// reads exactly as before (older clients ignore params they don't know).
	webURL := base + "/web/connect?" + url.Values{"token": {token}}.Encode() + addrs.query()
	// Custom-scheme deep link for an explicit "Open in app" button. Custom schemes
	// are not domain-bound, so this launches an installed app on any self-hosted
	// domain. How many devices can exchange the token is governed by its origin
	// (see PairingPayload): an invite-derived token honors the invite's uses.
	appURI := "audiosilo://connect?" + url.Values{
		"server": {base},
		"token":  {token},
	}.Encode() + addrs.query()

	png, err := qrcode.Encode(webURL, qrcode.Medium, 512)
	if err != nil {
		return nil, err
	}
	return &PairingPayload{
		ServerName:   a.config().DisplayName(),
		BaseURL:      base,
		PairingToken: token,
		URI:          appURI,
		WebURL:       webURL,
		PNGDataURI:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		Addresses:    addrs,
		Links: AppLinks{
			Web:   base + "/web",
			Admin: base + "/admin",
		},
	}, nil
}
