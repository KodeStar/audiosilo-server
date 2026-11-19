package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// tlsConfig builds a *tls.Config for the configured TLS mode, or returns nil
// for plain HTTP (mode off / behind a reverse proxy).
func tlsConfig(cfg *config.Config) (*tls.Config, error) {
	switch cfg.TLS.Mode {
	case config.TLSOff:
		return nil, nil
	case config.TLSAutocert:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLS.Hosts...),
			Cache:      autocert.DirCache(cfg.TLS.CacheDir),
		}
		return m.TLSConfig(), nil
	default: // selfsigned
		cert, err := loadOrCreateSelfSigned(cfg)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	}
}

// noSNIRefusal is the reason autocert gives a TLS hello that names no host.
const noSNIRefusal = "acme/autocert: missing server name"

// noSNILogEnd ends the line net/http logs for that refusal.
var noSNILogEnd = []byte(": " + noSNIRefusal)

// handshakeErrorFrom starts the line net/http logs for a failed handshake,
// the client's address after it.
var handshakeErrorFrom = []byte("http: TLS handshake error from ")

// autocertErrorLog is the http.Server's error log under autocert, for a
// server bound to bind: the standard logger, less the handshake error for a
// hello that names no host from this machine (a loopback address, or bind's own
// host when it names an address: the healthcheck asks there). The image's
// healthcheck sends one every 30 seconds before a certificate is cached
// (launcher.Healthcheck); autocert refuses it before any order, and the line
// says nothing an owner could fix. The same refusal for any other client is
// still logged: something on the network asking the address without a name.
func autocertErrorLog(bind string) *log.Logger {
	q := quietNoSNI{w: log.Writer()}
	if host, _, err := net.SplitHostPort(bind); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil && !ip.IsUnspecified() {
			q.self = ip.Unmap()
		}
	}
	return log.New(q, log.Prefix(), log.Flags())
}

// quietNoSNI passes each log line to w but the no-SNI handshake error from
// this machine: a loopback address or self (the bind address's host, when set).
type quietNoSNI struct {
	w    io.Writer
	self netip.Addr
}

func (q quietNoSNI) Write(p []byte) (int, error) {
	line := bytes.TrimRight(p, "\n")
	if rest, ok := bytes.CutSuffix(line, noSNILogEnd); ok {
		if i := bytes.Index(rest, handshakeErrorFrom); i >= 0 && q.fromHere(string(rest[i+len(handshakeErrorFrom):])) {
			return len(p), nil
		}
	}
	return q.w.Write(p)
}

// fromHere reports whether hostport (a client's "ip:port") is a loopback
// address or q.self.
func (q quietNoSNI) fromHere(hostport string) bool {
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	return ip.IsLoopback() || q.self.IsValid() && ip == q.self
}

// certValidMargin is how long past now a cached certificate must stay valid
// for CachedCertName to name its host: one about to expire would be ordered
// again by the handshake that names it.
const certValidMargin = time.Hour

// CachedCertName is the first of tls.hosts whose certificate autocert would
// answer a hello naming it with from its cache, valid until past
// certValidMargin, so the handshake can't start an order: the ECDSA one
// (autocert's cache key is the bare host name for the ECDSA certificate any Go
// client is given, host+"+rsa" for RSA), its key matching, the host on it. ""
// when none is: none issued yet, one expired, a host that isn't plain ASCII
// (autocert keys its IDNA form, which this does not compute) or a cache this
// process may not read. The healthcheck names it to ask /healthz itself.
func CachedCertName(ctx context.Context, cfg *config.Config, now time.Time) string {
	cache := autocert.DirCache(cfg.TLS.CacheDir)
	for _, host := range cfg.TLS.Hosts {
		name := strings.ToLower(strings.TrimSuffix(host, "."))
		if name == "" || !plainHost(name) {
			continue
		}
		data, err := cache.Get(ctx, name)
		if err != nil {
			continue
		}
		pair, err := tls.X509KeyPair(data, data) // the key's block first, then the chain's
		if err != nil || pair.Leaf == nil {
			continue
		}
		leaf := pair.Leaf
		if _, ecdsaKey := leaf.PublicKey.(*ecdsa.PublicKey); !ecdsaKey ||
			now.Before(leaf.NotBefore) || now.Add(certValidMargin).After(leaf.NotAfter) ||
			leaf.VerifyHostname(name) != nil {
			continue
		}
		return name
	}
	return ""
}

// plainHost reports whether name is lower-case ASCII letters, digits, dots and
// hyphens: a host autocert keys as written.
func plainHost(name string) bool {
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// selfSignedPaths are where the self-signed pair lives: tls.cert_file/key_file,
// else beside the data.
func selfSignedPaths(cfg *config.Config) (cert, key string) {
	cert, key = cfg.TLS.CertFile, cfg.TLS.KeyFile
	if cert == "" {
		cert = filepath.Join(cfg.DataDir, "selfsigned-cert.pem")
	}
	if key == "" {
		key = filepath.Join(cfg.DataDir, "selfsigned-key.pem")
	}
	return cert, key
}

// loadOrCreateSelfSigned reuses a persisted cert/key pair when present,
// otherwise generates a long-lived self-signed certificate and saves it so a
// user who manually trusted the cert (LAN browsers / OS trust store) doesn't
// have to re-trust it after every restart.
func loadOrCreateSelfSigned(cfg *config.Config) (tls.Certificate, error) {
	certPath, keyPath := selfSignedPaths(cfg)
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return tls.LoadX509KeyPair(certPath, keyPath)
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AudioSilo"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pemBlock("CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pemBlock("EC PRIVATE KEY", keyDER)
	// Best-effort persistence; failure to write just means a new cert next boot.
	_ = os.WriteFile(certPath, certPEM, 0o600)
	_ = os.WriteFile(keyPath, keyPEM, 0o600)
	return tls.X509KeyPair(certPEM, keyPEM)
}
