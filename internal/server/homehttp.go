package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Home HTTP (config.ServesHomeHTTP): a self-signed server's port answers plain
// HTTP as well as TLS, because the native apps can't trust a self-signed
// certificate. splitListener tells the two apart per connection, and
// homeHTTPGate serves plain HTTP only to clients on the home network.

// tlsRecordHandshake is the first byte of every TLS connection: the record type
// of the ClientHello. No plain HTTP request starts with it (a method is letters).
const tlsRecordHandshake = 0x16

// splitListener is one TCP listener for both protocols. It accepts connections
// and, each in its own goroutine so a silent client never holds up the others,
// reads the first byte: a TLS handshake record makes the connection a TLS server
// connection (http.Server serves it as https), anything else stays plain. The
// read is bounded by peek; a connection that sends nothing in time is closed.
type splitListener struct {
	net.Listener                 // the TCP listener: Addr, and the connections
	tls          *tls.Config     // the server's certificate, for the TLS ones
	peek         time.Duration   // how long a connection may take to send its first byte
	conns        chan accepted   // classified connections (or accept errors), for Accept
	ctx          context.Context // done once Close is called
	cancel       context.CancelFunc
}

// accepted is one result of the TCP listener, classified.
type accepted struct {
	conn net.Conn
	err  error
}

// newSplitListener starts splitting ln's connections; Close stops it.
func newSplitListener(ln net.Listener, cfg *tls.Config, peek time.Duration) *splitListener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &splitListener{Listener: ln, tls: cfg, peek: peek, conns: make(chan accepted), ctx: ctx, cancel: cancel}
	go l.acceptLoop()
	return l
}

// Accept returns the next classified connection: a *tls.Conn for a TLS client,
// else the plain connection. After Close it returns net.ErrClosed.
func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case a := <-l.conns:
		return a.conn, a.err
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close stops accepting and closes the connections still being classified.
func (l *splitListener) Close() error {
	l.cancel()
	return l.Listener.Close()
}

// acceptLoop accepts until Close. An accept error other than the listener being
// closed goes to Accept, so http.Server decides as it would on its own listener
// (back off on a temporary one, stop on any other); the loop backs off too, as
// http.Server does, rather than spin on a failing listener.
func (l *splitListener) acceptLoop() {
	var delay time.Duration
	for {
		c, err := l.Listener.Accept()
		if err == nil {
			delay = 0
			go l.classify(c)
			continue
		}
		if l.ctx.Err() != nil {
			return
		}
		select {
		case l.conns <- accepted{err: err}:
		case <-l.ctx.Done():
			return
		}
		delay = min(max(2*delay, 5*time.Millisecond), time.Second)
		select {
		case <-time.After(delay):
		case <-l.ctx.Done():
			return
		}
	}
}

// classify reads c's first byte and hands c to Accept as TLS or plain.
func (l *splitListener) classify(c net.Conn) {
	// Close (a shutdown) closes a connection still on its first byte.
	stop := context.AfterFunc(l.ctx, func() { _ = c.Close() })
	var first [1]byte
	_ = c.SetReadDeadline(time.Now().Add(l.peek))
	_, err := io.ReadFull(c, first[:])
	_ = c.SetReadDeadline(time.Time{})
	if !stop() || err != nil {
		_ = c.Close()
		return
	}
	var conn net.Conn = &peekedConn{Conn: c, first: first[0], unread: true}
	if first[0] == tlsRecordHandshake {
		conn = tls.Server(conn, l.tls)
	}
	select {
	case l.conns <- accepted{conn: conn}:
	case <-l.ctx.Done():
		_ = c.Close()
	}
}

// peekedConn is a connection whose first byte was read to classify it: Read
// returns that byte first, then reads on.
type peekedConn struct {
	net.Conn
	first  byte
	unread bool
}

func (c *peekedConn) Read(p []byte) (int, error) {
	if c.unread && len(p) > 0 {
		p[0], c.unread = c.first, false
		return 1, nil
	}
	return c.Conn.Read(p)
}

// homeHTTPGate serves a request that came over TLS, or over plain HTTP from a
// home-network client (homeClient); any other plain HTTP request is redirected
// to the same URL on https (308, so a POST stays a POST), or refused with a 400
// when its Host can't make that URL.
func homeHTTPGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || homeClient(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		target, ok := httpsURL(r)
		if !ok {
			http.Error(w, "This server needs HTTPS.", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

// homeClient reports whether a connection's remote address ("ip:port", the
// connection's own, never a forwarding header, which the client writes itself)
// is on the home network: config.HomeNetworkIP (private or link-local), or
// loopback, this machine itself.
func homeClient(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	return ap.Addr().Unmap().IsLoopback() || config.HomeNetworkIP(ap.Addr())
}

// httpsURL is r's URL on https: the same Host (and so the same port, which
// answers TLS too) and request URI. ok is false when the Host is missing or
// isn't a plain host[:port], or the request isn't for a path.
func httpsURL(r *http.Request) (target string, ok bool) {
	if r.Host == "" || !strings.HasPrefix(r.URL.Path, "/") {
		return "", false
	}
	u, err := url.Parse("https://" + r.Host + "/")
	if err != nil || u.Host != r.Host || u.User != nil || u.Hostname() == "" || u.Path != "/" {
		return "", false
	}
	return "https://" + r.Host + r.URL.RequestURI(), true
}
