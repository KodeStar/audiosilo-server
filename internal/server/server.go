// Package server runs the HTTP(S) server with the configured TLS mode and
// supports graceful shutdown. TLS is configurable: off (behind a reverse
// proxy), a self-signed certificate (LAN), or Let's Encrypt via autocert.
package server

import (
	"context"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Run starts the server and blocks until ctx is cancelled, then drains
// connections gracefully.
func Run(ctx context.Context, cfg *config.Config, handler http.Handler, log *slog.Logger) error {
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No write timeout: audio streaming/transcoding responses are long-lived.
	}

	errCh := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			log.Info("listening", "addr", cfg.Bind, "tls", cfg.TLS.Mode)
			errCh <- srv.ListenAndServeTLS("", "") // certs come from TLSConfig
		} else {
			log.Info("listening", "addr", cfg.Bind, "tls", "off")
			errCh <- srv.ListenAndServe()
		}
	}()

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
}

// pemBlock PEM-encodes DER bytes under the given type.
func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}
