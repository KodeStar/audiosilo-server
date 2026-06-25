package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
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

// loadOrCreateSelfSigned reuses a persisted cert/key pair when present,
// otherwise generates a long-lived self-signed certificate and saves it so a
// user who manually trusted the cert (LAN browsers / OS trust store) doesn't
// have to re-trust it after every restart.
func loadOrCreateSelfSigned(cfg *config.Config) (tls.Certificate, error) {
	certPath := cfg.TLS.CertFile
	keyPath := cfg.TLS.KeyFile
	if certPath == "" {
		certPath = filepath.Join(cfg.DataDir, "selfsigned-cert.pem")
	}
	if keyPath == "" {
		keyPath = filepath.Join(cfg.DataDir, "selfsigned-key.pem")
	}
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
