package tunnels

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/vmutil"
)

// ConnectTarget describes how to reach a shed-server's Connect API. Addr is the
// host:port to dial. When TLSPin is set, the connection is wrapped in TLS
// verified against that pinned cert fingerprint; when Token is set, it is sent
// as the credentials-scoped bearer token on the upgrade request. An empty
// TLSPin and Token reproduce the legacy plain-TCP, unauthenticated dial
// byte-for-byte.
type ConnectTarget struct {
	Addr   string // host:port to dial
	TLSPin string // "sha256:<hex>"; empty = plain TCP
	Token  string // credentials bearer token; empty = no Authorization header
}

// ConnectClient dials shed VMs via the shed-server Connect API.
type ConnectClient struct {
	target ConnectTarget
}

// NewConnectClient creates a new Connect API client for the given target.
func NewConnectClient(target ConnectTarget) *ConnectClient {
	return &ConnectClient{target: target}
}

// Dial opens a TCP tunnel to a shed VM port via the Connect API. It performs an
// HTTP upgrade handshake and returns the underlying connection (over pinned TLS
// when the target is pinned).
func (c *ConnectClient) Dial(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	// The upgrade write + http.ReadResponse below don't honor ctx on their own,
	// so a server that accepts the connection but never sends 101 would block
	// forever — wedging Tunnel.Stop(). Close the conn if ctx is cancelled during
	// the handshake.
	stopWatch := closeConnsOnCancel(ctx, conn)
	defer stopWatch()

	// Send the HTTP upgrade request (over TLS when c.dial wrapped the conn).
	path := fmt.Sprintf("/api/sheds/%s/connect/%d", shedName, port)
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: shed-tcp\r\n", path, c.target.Addr)
	if c.target.Token != "" {
		fmt.Fprintf(&req, "Authorization: Bearer %s\r\n", c.target.Token)
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr // watcher closed conn on cancel; report cancellation, not a write error
		}
		return nil, fmt.Errorf("send upgrade request: %w", err)
	}

	// Read the response.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr // watcher closed conn on cancel; report cancellation, not a read error
		}
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.Body != nil {
			resp.Body.Close()
		}
		conn.Close()
		return nil, fmt.Errorf("connect API returned HTTP %d (expected 101)", resp.StatusCode)
	}

	// Stop the cancel-watcher before handing the conn back so it can't close the
	// conn we return; if ctx was cancelled mid-handshake, fail instead of
	// returning a conn the watcher may already have closed.
	stopWatch()
	if ctx.Err() != nil {
		conn.Close()
		return nil, ctx.Err()
	}

	return &vmutil.BufferedConn{Conn: conn, Reader: reader}, nil
}

// dial opens the transport to the Connect API: a raw TCP conn (byte-identical
// to the legacy path), or — when the target is pinned — a TLS conn whose server
// cert is verified against the pin. A raw net.Dialer is used rather than
// http.Client because the tunnel is long-lived and must not inherit request
// timeouts.
func (c *ConnectClient) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.target.Addr)
	if err != nil {
		return nil, fmt.Errorf("connect to shed-server at %s: %w", c.target.Addr, err)
	}
	if c.target.TLSPin == "" {
		return conn, nil // plain TCP — unchanged
	}
	tlsConn := tls.Client(conn, servertls.PinnedClientConfig(c.target.TLSPin))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", c.target.Addr, err)
	}
	return tlsConn, nil
}
