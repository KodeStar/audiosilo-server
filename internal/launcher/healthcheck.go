package launcher

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/server"
)

// healthcheckTimeout bounds one healthcheck: the server's /healthz answers within
// its own two-second database probe, so this leaves room for a slow handshake and
// still ends well inside the image's ten-second HEALTHCHECK timeout (the
// Dockerfile's --timeout; Docker's own default is thirty).
const healthcheckTimeout = 5 * time.Second

// Healthcheck asks the server running from dataDir whether it is healthy: a GET
// of /healthz on the address config.yaml and the AUDIOSILO_* environment give
// it (bind and tls.mode, read as the server reads them at start), answered 200,
// or 503 "starting" (server.StartingStatus): the server listens before it opens
// the database, and a start's migrations can outlast any start period, so a
// starting server is healthy and an orchestrator never restarts it mid-way.
// It backs `audiosilo healthcheck`, the Docker image's HEALTHCHECK (server#217):
// nil is healthy, an error says why not. It opens nothing in the data folder
// but config.yaml (and under autocert the certificate cache), writes nothing
// and needs no curl in the image. A data folder without a config.yaml is
// unhealthy: a server saves one before it listens, so none there means no
// server is running from that folder (a --data that isn't the server's), never
// the defaults guessed at.
//
// An unspecified bind host (0.0.0.0, ::, empty) is asked on 127.0.0.1. With
// tls.mode selfsigned it speaks https without checking the certificate: it is
// the server asking itself, it sends nothing secret, and a self-signed
// certificate must not make a live server look dead.
//
// Under autocert it names a host only once autocert holds a valid certificate
// for it in its cache (server.CachedCertName): a handshake naming one of
// tls.hosts starts an issuance whenever that host has no valid certificate,
// and every failed one counts against Let's Encrypt's failed-validation limit,
// while the check would read unhealthy for a state no restart fixes. With one
// cached it asks /healthz on the listener naming that host, as any client
// would. With none it connects to the listener and sends a TLS hello naming no
// host, which autocert refuses at once, before its cache or any order; the
// refusal (a TLS alert) or a finished handshake is healthy, since the server
// accepted the connection and answered the hello. That proves the server is up
// and serving, not that its database answers.
//
// A bind or TLS change saved in the console takes effect at the next start, so
// until then this asks the new address (the saved one) and fails: a restart
// applies it.
func Healthcheck(ctx context.Context, dataDir string, w io.Writer) error {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	cfg, err := loadSettled(abs)
	if err != nil {
		return err
	}
	addr, err := healthAddr(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	if cfg.TLS.Mode == config.TLSAutocert {
		if host := server.CachedCertName(ctx, cfg, time.Now()); host != "" {
			return getHealthz(ctx, addr, host, true, w)
		}
		if err := helloWithoutName(ctx, addr); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "ok: TLS listener at", addr)
		return nil
	}
	return getHealthz(ctx, addr, "", cfg.TLS.Mode != config.TLSOff, w)
}

// configSettle is how long loadSettled waits before reading config.yaml again,
// and configReads how many times it reads it at most.
const (
	configSettle = 100 * time.Millisecond
	configReads  = 3
)

// loadSettled reads the config in dataDir as Healthcheck needs it: a read that
// raced a save from the console (config.Save rewrites the file in place: an
// empty file, or one changed while it was read) is read again after a moment,
// rather than taken for the defaults or a parse error.
func loadSettled(dataDir string) (*config.Config, error) {
	path := config.Path(dataDir)
	for i := 1; ; i++ {
		before, statErr := os.Stat(path)
		cfg, firstRun, err := config.Load(dataDir)
		after, _ := os.Stat(path)
		settled := statErr == nil && after != nil && before.Size() > 0 &&
			before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
		if firstRun {
			return nil, fmt.Errorf("no %s: no server has started from %s", path, dataDir)
		}
		if settled || i == configReads {
			if err != nil {
				return nil, fmt.Errorf("reading the config: %w", err)
			}
			return cfg, nil
		}
		time.Sleep(configSettle)
	}
}

// getHealthz asks addr's /healthz for a 200, or a 503 "starting". host, when
// set, is the name the request carries (its TLS server name and Host); the
// connection goes to addr either way.
func getHealthz(ctx context.Context, addr, host string, tlsOn bool, w io.Writer) error {
	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	urlHost := addr
	if host != "" {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		urlHost = net.JoinHostPort(host, port)
	}
	target := (&url.URL{Scheme: scheme, Host: urlHost, Path: "/healthz"}).String()
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // never through an HTTP(S)_PROXY the container sets: this is the server asking itself
			// Always the listener at addr, whatever host the URL names.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // the server's own loopback probe; see Healthcheck
			},
			DisableKeepAlives: true,
		},
		// A redirect (home HTTP's https redirect, a proxy's) is not the answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("no answer from %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusOK:
		_, _ = fmt.Fprintln(w, "ok:", target)
		return nil
	case resp.StatusCode == http.StatusServiceUnavailable && startingAnswer(body):
		_, _ = fmt.Fprintln(w, "starting:", target)
		return nil
	}
	return fmt.Errorf("%s answered %s", target, resp.Status)
}

// startingAnswer reports whether body is a starting server's answer
// (server.StartGate).
func startingAnswer(body []byte) bool {
	var v struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(body, &v) == nil && v.Status == server.StartingStatus
}

// helloWithoutName sends addr a TLS hello naming no host (see Healthcheck):
// nil on a finished handshake or a TLS alert.
func helloWithoutName(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("no answer from %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	// No ServerName, and tls.Client never takes one from the address: no SNI.
	err = tls.Client(conn, &tls.Config{InsecureSkipVerify: true}).HandshakeContext(ctx) //nolint:gosec // nothing is sent past the hello
	if err == nil {
		return nil
	}
	var opErr *net.OpError // crypto/tls reports a received alert as a "remote error"
	if errors.As(err, &opErr) && opErr.Op == "remote error" {
		return nil
	}
	return fmt.Errorf("no TLS answer from %s: %w", addr, err)
}

// healthAddr is the host:port the server cfg describes listens on, an
// unspecified host asked on 127.0.0.1 (where baseURL, for a browser, says
// localhost).
func healthAddr(cfg *config.Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.Bind)
	if err != nil {
		return "", fmt.Errorf("invalid bind address %q: %w", cfg.Bind, err)
	}
	if unspecifiedHost(host) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// unspecifiedHost reports whether a bind address's host listens on every
// interface: empty, 0.0.0.0, :: or another spelling of them.
func unspecifiedHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "" || ip != nil && ip.IsUnspecified()
}
