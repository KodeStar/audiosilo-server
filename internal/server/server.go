// Package server runs the HTTP(S) server with the configured TLS mode and
// supports graceful shutdown. TLS is configurable: off (behind a reverse
// proxy), a self-signed certificate (LAN), or Let's Encrypt via autocert. A
// self-signed server also answers plain HTTP on its port to home-network
// clients (tls.home_http; see homehttp.go).
package server

import (
	"context"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// readHeaderTimeout bounds a client's request headers, and with home HTTP its
// first byte (splitListener's peek).
const readHeaderTimeout = 10 * time.Second

// Serving is a server Start left running.
type Serving struct {
	wait func() error
	once sync.Once
	err  error
}

// Wait blocks until the server stops: ctx cancelled (connections then drain
// gracefully, nil) or serving failed (its error). Every call returns the same.
func (s *Serving) Wait() error {
	s.once.Do(func() { s.err = s.wait() })
	return s.err
}

// Start listens on the address cfg describes and serves handler there in the
// background until ctx is cancelled (Serving.Wait). A listen error (the port
// taken, a certificate that won't load) is returned at once, before anything
// is served.
func Start(ctx context.Context, cfg *config.Config, handler http.Handler, log *slog.Logger) (*Serving, error) {
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	// autocert (ACME TLS-ALPN-01/HTTP-01) only validates when Let's Encrypt can
	// reach this server on port 443. The default bind is :8080, so warn loudly
	// rather than let issuance fail silently - unless external :443 is forwarded
	// here at the NAT/OS level, in which case the warning is harmless.
	if cfg.TLS.Mode == config.TLSAutocert {
		if _, port, splitErr := net.SplitHostPort(cfg.Bind); splitErr == nil && port != "443" {
			log.Warn("autocert configured but bind port is not 443; certificate issuance "+
				"will fail unless external :443 is forwarded to this address",
				"bind", cfg.Bind, "port", port)
		}
	}
	srv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: readHeaderTimeout,
		// ReadTimeout bounds reading the whole request (headers + body), so a slow
		// client can't hold a control-plane connection open indefinitely. It is
		// safe for streaming, which is a GET with no request body. There is
		// deliberately no WriteTimeout: audio streaming/transcoding responses are
		// long-lived.
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
	}
	if cfg.TLS.Mode == config.TLSAutocert {
		srv.ErrorLog = autocertErrorLog()
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Bind)
	if err != nil {
		return nil, err
	}
	errCh := make(chan error, 1)
	switch {
	case cfg.ServesHomeHTTP():
		// One port, both protocols: the TLS connections get the certificate here
		// (so ALPN must offer HTTP/2 itself, as ServeTLS would), and the plain ones
		// pass homeHTTPGate.
		tlsCfg.NextProtos = []string{"h2", "http/1.1"}
		srv.Handler = homeHTTPGate(handler)
		log.Info("listening", "addr", cfg.Bind, "tls", cfg.TLS.Mode)
		log.Info("plain HTTP is served on the same port to home-network clients (tls.home_http); "+
			"other plain HTTP clients are redirected to https", "addr", cfg.Bind)
		go func() { errCh <- srv.Serve(newSplitListener(ln, tlsCfg, readHeaderTimeout)) }()
	case tlsCfg != nil:
		log.Info("listening", "addr", cfg.Bind, "tls", cfg.TLS.Mode)
		go func() { errCh <- srv.ServeTLS(ln, "", "") }() // certs come from TLSConfig
	default:
		log.Info("listening", "addr", cfg.Bind, "tls", "off")
		go func() { errCh <- srv.Serve(ln) }()
	}

	return &Serving{wait: func() error {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	}}, nil
}

// pemBlock PEM-encodes DER bytes under the given type.
func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}
