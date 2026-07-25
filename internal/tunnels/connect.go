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
	// source, when non-nil, supplies the credential — a bearer token or a client
	// certificate — and transparently re-mints it (proactively before expiry,
	// reactively on an auth failure) so a long-lived tunnel survives the
	// credential TTL. nil ⇒ the static target.Token (legacy/plain path).
	source *clienttoken.Source
}

// NewConnectClient creates a Connect API client for the given target. A non-nil
// source makes the client self-refreshing; nil uses the static target.Token
// (and no token at all for the plain/legacy path).
func NewConnectClient(target ConnectTarget, source *clienttoken.Source) *ConnectClient {
	return &ConnectClient{target: target, source: source}
}

// authToken returns the bearer token to send for one attempt: the bearer half
// of the credential that attempt CAPTURED, else the static target.Token. It
// returns "" in mtls state (Credential.BearerToken is mode-gated) — the
// credential is in the handshake there, and the server does not read the header.
//
// It never returns a token for an unpinned (plain-TCP) target either — a bearer
// token must only travel over the pinned-TLS connection, never plaintext — so a
// misconfigured source can't leak one over the legacy path.
//
// It takes the captured credential rather than re-reading the Source, so the
// header and the handshake below carry the SAME generation — the one the
// caller recorded and will hand back to Refresh. See clienttoken.WithPinned.
func (c *ConnectClient) authToken(cred clienttoken.Credential) string {
	if c.target.TLSPin == "" {
		return ""
	}
	if c.source != nil {
		return cred.BearerToken()
	}
	return c.target.Token
}

// clientCertificate returns the client certificate to present at the TLS
// handshake, or nil. It is passed to servertls as a ClientCertSource so the
// certificate is fetched PER HANDSHAKE — a tunnel that outlives a cert rotation
// dials the next connection with the new one, with nothing to rebuild.
//
// ctx is the handshake's context: an attempt pins its captured credential onto
// it, so the certificate presented is the one that attempt recorded rather than
// whatever the Source holds by the time the handshake runs.
//
// Like authToken it is inert on an unpinned target: mtls is only ever served on
// the pinned TLS listener, and a certificate must not be offered to a peer whose
// identity has not been verified.
func (c *ConnectClient) clientCertificate(ctx context.Context) *tls.Certificate {
	if c.target.TLSPin == "" || c.source == nil {
		return nil
	}
	return c.source.CertificateFor(ctx)
}

// Dial opens a TCP tunnel to a shed VM port via the Connect API. It performs an
// HTTP upgrade handshake and returns the underlying connection (over pinned TLS
// when the target is pinned).
//
// When the client has a refreshing source it re-mints proactively before
// dialing and, once, reactively before re-dialing — so a tunnel outlives the
// credential TTL. The reactive trigger is an AUTH-SHAPED FAILURE, which on this
// path is either a 401 from the Connect route or a TLS-level rejection of the
// client certificate; the latter surfaces as a dial error with no HTTP status
// at all, so the retry cannot be keyed on the status alone.
func (c *ConnectClient) Dial(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	cred, sentGen := c.capture()
	conn, status, err := c.attemptDial(ctx, shedName, port, cred)
	// A credential can expire mid-tunnel: re-mint once and re-dial from scratch
	// (the raw upgrade connection can't be reused for a retry).
	if clienttoken.IsAuthFailure(status, err) && c.source != nil && c.source.Refreshable() {
		fresh, rerr := c.source.Refresh(sentGen)
		if rerr != nil {
			return nil, fmt.Errorf("connect API rejected the tunnel credential and the re-mint failed: %w", rerr)
		}
		conn, status, err = c.attemptDial(ctx, shedName, port, fresh)
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("connect API returned HTTP %d (expected 101)", status)
	}
	return conn, nil
}

// capture reads the credential this attempt will transmit and the generation it
// belongs to, in ONE atomic read, after a proactive freshness check. Both the
// bearer header and the TLS certificate are then derived from that single
// value, so the generation reported to Refresh after a rejection is provably
// the generation that was sent — see clienttoken.WithPinned for why re-reading
// the Source at each use site instead is a race.
//
// A client with no source (the legacy static/plain path) captures the zero
// credential; authToken falls back to target.Token and no certificate is ever
// offered.
func (c *ConnectClient) capture() (clienttoken.Credential, uint64) {
	if c.source == nil {
		return clienttoken.Credential{}, 0
	}
	c.source.EnsureFresh() // proactive: re-mint if within the expiry window
	return c.source.Current()
}

// attemptDial performs a single dial + Connect upgrade with cred as its pinned
// credential — the bearer header and the handshake certificate both come from
// it. On a 101 it returns the established connection and status 101; on any
// other HTTP status it closes the connection and returns (nil, status, nil); a
// transport/handshake/cancellation error returns (nil, 0, err).
func (c *ConnectClient) attemptDial(ctx context.Context, shedName string, port uint16, cred clienttoken.Credential) (net.Conn, int, error) {
	// The dial carries the pinned credential so this attempt's handshake presents
	// the certificate it captured; cancellation is unaffected (the pinned context
	// derives from ctx), so the watcher below still uses the caller's ctx.
	conn, err := c.dial(clienttoken.WithPinned(ctx, cred))
	if err != nil {
		return nil, 0, err
	}
	token := c.authToken(cred)

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
//
// Like Dial, the probe recovers REACTIVELY from an auth-shaped failure: it
// re-mints once and probes again. Without that it was the one credential path
// with only a proactive refresh, so a certificate the server had already
// stopped accepting — de-authorized, revoked, or rejected after a mode flip —
// failed startup with a "re-run the command" message describing work the client
// was perfectly able to do itself.
func (c *ConnectClient) Probe(ctx context.Context, shedName string) error {
	cred, sentGen := c.capture()
	res := c.attemptProbe(ctx, shedName, cred)
	if clienttoken.IsAuthFailure(res.status, res.reqErr) && c.source != nil && c.source.Refreshable() {
		fresh, rerr := c.source.Refresh(sentGen)
		if rerr != nil {
			return fmt.Errorf("connect probe: shed-server rejected the tunnel credential and the re-mint failed: %w", rerr)
		}
		res = c.attemptProbe(ctx, shedName, fresh)
	}
	if res.reqErr != nil {
		// An mtls server's refusal arrives as a TLS alert, not a status — name it
		// as the credential problem it is rather than a generic transport error,
		// so the operator is not sent hunting for a network fault.
		if clienttoken.IsAuthFailure(0, res.reqErr) {
			return fmt.Errorf("connect probe: shed-server rejected the tunnel client certificate: %w", res.reqErr)
		}
		return fmt.Errorf("connect probe to %s: %w", c.target.Addr, res.reqErr)
	}
	return res.verdict
}

// probeResult is one probe attempt's outcome, split so Probe can classify an
// auth-shaped failure (which needs the raw status and the raw transport error)
// separately from the message it finally reports.
type probeResult struct {
	// status is the HTTP status, or 0 when the request produced no response.
	status int
	// reqErr is the RAW transport error, unwrapped, so IsAuthFailure sees the
	// TLS alert exactly as crypto/tls produced it. nil on a completed response.
	reqErr error
	// verdict is this attempt's answer: nil means authenticated and the Connect
	// route is reachable. Meaningful only when reqErr is nil.
	verdict error
}

// attemptProbe issues one probe with cred as its pinned credential.
//
// It builds its own transport per attempt, which is how the retry gets a fresh
// TLS handshake: a pooled connection still carries the handshake identity the
// server just rejected, so reusing one would replay the rejected certificate.
// (The control plane achieves the same thing with CloseIdleConnections on its
// long-lived shared transport; the probe is one-shot, so a fresh transport is
// simpler and equivalent.)
func (c *ConnectClient) attemptProbe(ctx context.Context, shedName string, cred clienttoken.Credential) probeResult {
	scheme := "http"
	if c.target.TLSPin != "" {
		scheme = "https"
	}
	// nil ⇒ DefaultTransport (plain HTTP). The client-cert source is always
	// passed, so the probe authenticates identically to a real dial.
	transport := servertls.PinnedTransport(c.target.TLSPin, c.clientCertificate)
	reqURL := fmt.Sprintf("%s://%s/api/sheds/%s/connect/0", scheme, c.target.Addr, url.PathEscape(shedName))
	req, err := http.NewRequestWithContext(clienttoken.WithPinned(ctx, cred), http.MethodGet, reqURL, nil)
	if err != nil {
		return probeResult{verdict: fmt.Errorf("build connect probe request: %w", err)}
	}
	if tok := c.authToken(cred); tok != "" {
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
		return probeResult{reqErr: err}
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
			return probeResult{status: resp.StatusCode} // authenticated, Connect route reachable
		}
		return probeResult{status: resp.StatusCode, verdict: fmt.Errorf(
			"connect probe: unexpected 400 from %s — not the shed Connect API (INVALID_PORT); check the endpoint", c.target.Addr)}
	case http.StatusUnauthorized:
		return probeResult{status: resp.StatusCode, verdict: fmt.Errorf(
			"connect probe: shed-server rejected the tunnel token (401); it may be missing or expired")}
	case http.StatusForbidden:
		return probeResult{status: resp.StatusCode, verdict: fmt.Errorf(
			"connect probe: tunnel token scope not accepted on the Connect route (403); the shed-server may be too old to accept a control token — upgrade it")}
	default:
		return probeResult{status: resp.StatusCode, verdict: fmt.Errorf(
			"connect probe: unexpected HTTP %d from the Connect route", resp.StatusCode)}
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
	tlsConn := tls.Client(conn, servertls.PinnedClientConfig(c.target.TLSPin, c.clientCertificate))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", c.target.Addr, err)
	}
	return tlsConn, nil
}
