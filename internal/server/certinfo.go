package server

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Certificate describes a TLS certificate the server serves, for the admin
// console's Health > System.
type Certificate struct {
	Host       string    `json:"host"`   // the host it was issued for (autocert), or ""
	Issued     bool      `json:"issued"` // false: autocert hasn't obtained one yet
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	NotBefore  time.Time `json:"not_before"`
	NotAfter   time.Time `json:"not_after"`
	SelfSigned bool      `json:"self_signed"`
	DNSNames   []string  `json:"dns_names"`
}

// Certificates reads the certificates the configured TLS mode serves, from the
// files the server keeps them in: the self-signed pair, or autocert's cache (one
// per host). Mode off has none. It only reads; it never generates or requests one.
func Certificates(cfg *config.Config) ([]Certificate, error) {
	switch cfg.TLS.Mode {
	case config.TLSSelfSigned:
		path := cfg.TLS.CertFile
		if path == "" {
			path = filepath.Join(cfg.DataDir, "selfsigned-cert.pem")
		}
		c, err := readCert(path)
		if err != nil {
			return nil, err
		}
		return []Certificate{c}, nil
	case config.TLSAutocert:
		out := make([]Certificate, 0, len(cfg.TLS.Hosts))
		for _, h := range cfg.TLS.Hosts {
			// autocert.DirCache keeps a host's ECDSA key and chain in one PEM file
			// named after the host.
			c, err := readCert(filepath.Join(cfg.TLS.CacheDir, h))
			switch {
			case errors.Is(err, os.ErrNotExist):
				c = Certificate{DNSNames: []string{}}
			case err != nil:
				return nil, err
			}
			c.Host = h
			out = append(out, c)
		}
		return out, nil
	}
	return nil, nil
}

// readCert parses the first certificate in a PEM file.
func readCert(path string) (Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Certificate{}, err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return Certificate{}, errors.New("no certificate in " + filepath.Base(path))
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		x, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return Certificate{}, err
		}
		return Certificate{
			Issued:     true,
			Subject:    x.Subject.CommonName,
			Issuer:     x.Issuer.CommonName,
			NotBefore:  x.NotBefore,
			NotAfter:   x.NotAfter,
			SelfSigned: x.Subject.String() == x.Issuer.String(),
			DNSNames:   append([]string{}, x.DNSNames...),
		}, nil
	}
}
