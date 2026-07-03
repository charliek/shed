package tunnels

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/vmutil"
)

// ConnectTarget describes how to reach a shed-server's Connect API. Addr is the
// host:port to dial. When TLSPin is set, the connection is wrapped in TLS
// verified against that pinned cert fingerprint; when Token is set, it is sent
// as the bearer token on the upgrade request (the Connect route accepts a
// control or credentials token). An empty TLSPin and Token reproduce the legacy
// plain-TCP, unauthenticated dial byte-for-byte.
type ConnectTarget struct {
	Addr   string // host:port to dial
	TLSPin string // "sha256:<hex>"; empty = plain TCP
	Token  string // bearer token (control or credentials); empty = no Authorization header
}

// ConnectClient dials shed VMs via the shed-server Connect API.
type ConnectClient struct {
	target ConnectTarget
	// source, when non-nil, supplies the bearer token and transparently re-mints
	// it (proactively before expiry, reactively on a 401) so a long-lived tunnel
	// survives the token TTL. nil ⇒ the static target.Token (legacy/plain path).
	source *clienttoken.Source
}

// NewConnectClient creates a Connect API client for the given target. A non-nil
// source makes the client self-refreshing; nil uses the static target.Token
// (and no token at all for the plain/legacy path).
func NewConnectClient(target ConnectTarget, source *clienttoken.Source) *ConnectClient {
	return &ConnectClient{target: target, source: source}
}

// authToken returns the bearer token to send: the live token from the refreshing
// source when present, else the static target.Token. It never returns a token for
// an unpinned (plain-TCP) target — a bearer token must only travel over the
// pinned-TLS connection, never plaintext — so a misconfigured source can't leak
// one over the legacy path.
func (c *ConnectClient) authToken() string {
	if c.target.TLSPin == "" {
		return ""
	}
	if c.source != nil {
		return c.source.Token()
	}
	return c.target.Token
}

// Dial opens a TCP tunnel to a shed VM port via the Connect API. It performs an
// HTTP upgrade handshake and returns the underlying connection (over pinned TLS
// when the target is pinned). When the client has a refreshing source, it
// re-mints the token proactively before dialing and, on a 401, once reactively
// before re-dialing — so a tunnel outlives the control-token TTL.
func (c *ConnectClient) Dial(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	if c.source != nil {
		c.source.EnsureFresh() // proactive: re-mint if within the expiry window
	}
	sent := c.authToken()
	conn, status, err := c.attemptDial(ctx, shedName, port, sent)
	if err != nil {
		return nil, err
	}
	// A control token can expire mid-tunnel: on a 401, re-mint once and re-dial
	// from scratch (the raw upgrade connection can't be reused for a retry).
	if status == http.StatusUnauthorized && c.source != nil && c.source.Refreshable() {
		if _, rerr := c.source.Refresh(sent); rerr != nil {
			return nil, fmt.Errorf("connect API returned 401 and token re-mint failed: %w", rerr)
		}
		// Re-read through authToken so the retry re-applies the plaintext guard
		// (never a bearer over an unpinned connection) rather than the raw token.
		if conn, status, err = c.attemptDial(ctx, shedName, port, c.authToken()); err != nil {
			return nil, err
		}
	}
	if status != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("connect API returned HTTP %d (expected 101)", status)
	}
	return conn, nil
}

// attemptDial performs a single dial + Connect upgrade with the given bearer
// token. On a 101 it returns the established connection and status 101; on any
// other HTTP status it closes the connection and returns (nil, status, nil); a
// transport/handshake/cancellation error returns (nil, 0, err).
func (c *ConnectClient) attemptDial(ctx context.Context, shedName string, port uint16, token string) (net.Conn, int, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, 0, err
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
	if token != "" {
		fmt.Fprintf(&req, "Authorization: Bearer %s\r\n", token)
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr // watcher closed conn on cancel; report cancellation, not a write error
		}
		return nil, 0, fmt.Errorf("send upgrade request: %w", err)
	}

	// Read the response.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr // watcher closed conn on cancel; report cancellation, not a read error
		}
		return nil, 0, fmt.Errorf("read upgrade response: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.Body != nil {
			resp.Body.Close()
		}
		conn.Close()
		return nil, resp.StatusCode, nil
	}

	// Stop the cancel-watcher before handing the conn back so it can't close the
	// conn we return; if ctx was cancelled mid-handshake, fail instead of
	// returning a conn the watcher may already have closed.
	stopWatch()
	if ctx.Err() != nil {
		conn.Close()
		return nil, 0, ctx.Err()
	}

	return &vmutil.BufferedConn{Conn: conn, Reader: reader}, http.StatusSwitchingProtocols, nil
}

// Probe verifies the tunnel can authenticate to the Connect API without
// contacting any guest, so a broken tunnel fails loudly at startup instead of
// resetting every connection. It issues a plain GET to the real Connect route
// with port 0: the auth middleware runs first, then handleConnect rejects
// port 0 with 400 INVALID_PORT *before* dialing the guest. So:
//
//   - 400 → authenticated and the Connect route is reachable (regardless of
//     whether any guest port is listening yet)
//   - 401 → the token is missing/invalid/expired
//   - 403 → the token's scope isn't accepted on the Connect route (e.g. an
//     older shed-server that still gates Connect on the credentials scope)
//   - anything else / a transport or TLS-pin error → also a failure
//
// The caller bounds the probe with ctx's deadline (there is no internal
// timeout), so a stalled server can't wedge startup.
func (c *ConnectClient) Probe(ctx context.Context, shedName string) error {
	if c.source != nil {
		c.source.EnsureFresh() // probe with a fresh token so a near-expiry one doesn't fail startup
	}
	scheme := "http"
	if c.target.TLSPin != "" {
		scheme = "https"
	}
	transport := servertls.PinnedTransport(c.target.TLSPin) // nil ⇒ DefaultTransport (plain HTTP)
	reqURL := fmt.Sprintf("%s://%s/api/sheds/%s/connect/0", scheme, c.target.Addr, url.PathEscape(shedName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build connect probe request: %w", err)
	}
	if tok := c.authToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := &http.Client{Transport: transport}
	if transport != nil {
		// One-shot probe: close our own pinned transport's idle keep-alive conn so
		// it doesn't linger for the (long-lived) daemon worker's lifetime. Guarded
		// so we never touch the shared DefaultTransport used by the plain path.
		defer client.CloseIdleConnections()
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect probe to %s: %w", c.target.Addr, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusBadRequest:
		// The only 400 handleConnect returns on this route is INVALID_PORT (for
		// port 0), emitted *after* auth passes. Confirm the shed error code so a
		// generic 400 from a proxy, wrong service, or TLS-terminating middlebox
		// doesn't false-pass as "authenticated" (see internal/api/connect.go).
		var apiErr config.APIError
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Error.Code == "INVALID_PORT" {
			return nil // authenticated, Connect route reachable
		}
		return fmt.Errorf("connect probe: unexpected 400 from %s — not the shed Connect API (INVALID_PORT); check the endpoint", c.target.Addr)
	case http.StatusUnauthorized:
		return fmt.Errorf("connect probe: shed-server rejected the tunnel token (401); it may be missing or expired")
	case http.StatusForbidden:
		return fmt.Errorf("connect probe: tunnel token scope not accepted on the Connect route (403); the shed-server may be too old to accept a control token — upgrade it")
	default:
		return fmt.Errorf("connect probe: unexpected HTTP %d from the Connect route", resp.StatusCode)
	}
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
