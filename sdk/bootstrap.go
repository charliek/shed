package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// bootstrapUser is the reserved SSH username the shed server intercepts to mint
// and return an HTTP token bundle (see internal/sshd reservedBootstrapUser).
const bootstrapUser = "_bootstrap"

// bootstrapTimeout bounds the whole exchange (dial + handshake + command) when
// the caller's ctx has no earlier deadline. A var so tests can shorten it.
var bootstrapTimeout = 15 * time.Second

// Bundle is the result of an SSH bootstrap exchange: a freshly-minted HTTP
// bearer token plus the metadata a client needs to reach the HTTP API.
//
// The json tags MUST stay in sync with the server's bootstrapBundle in
// internal/sshd/bootstrap.go. The server returns ports (not a URL) because it
// can't reliably know its own external hostname — the caller builds the base
// URL from the host it dialed plus HTTPSPort (preferred, pinned via
// TLSCertFingerprint) or HTTPPort.
type Bundle struct {
	HTTPPort           int       `json:"http_port"`
	HTTPSPort          int       `json:"https_port"`
	TLSCertFingerprint string    `json:"tls_cert_fingerprint"`
	Token              string    `json:"token"`
	Scope              string    `json:"scope"`
	TokenID            string    `json:"token_id"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// Bootstrap dials target over SSH as the reserved _bootstrap user, verifies the
// server's host key against hostKeyPin (a SHA-256 fingerprint, "SHA256:..." as
// produced by ssh.FingerprintSHA256), and runs the reserved command to mint and
// return a token bundle for the given scope. clientKind is advisory audit
// metadata ("cli" / "host-agent" / "desktop"); it may be empty.
//
// It fails closed on a host-key mismatch — the pin is the trust root of the
// channel that issues the credential. The whole exchange is bounded by ctx.
func Bootstrap(ctx context.Context, target string, signer ssh.Signer, hostKeyPin, scope, clientKind string) (Bundle, error) {
	if hostKeyPin == "" {
		return Bundle{}, errors.New("sdk: bootstrap requires a host key pin")
	}
	if signer == nil {
		return Bundle{}, errors.New("sdk: bootstrap requires a signer")
	}
	if scope == "" {
		// A bare client-kind would be parsed as the scope by the server's
		// strings.Fields("<scope> [<kind>]") — require an explicit scope.
		return Bundle{}, errors.New("sdk: bootstrap requires a scope")
	}

	cfg := &ssh.ClientConfig{
		User:            bootstrapUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: pinnedHostKey(hostKeyPin),
	}

	// Bound the WHOLE exchange, including the dial. DialContext gates the connect
	// on this ctx, so a caller with no deadline (e.g. the host-agent's cancel-only
	// group ctx) can't block on the OS TCP timeout against a blackholed endpoint.
	// A shorter caller deadline still wins.
	ctx, cancel := context.WithTimeout(ctx, bootstrapTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return Bundle{}, fmt.Errorf("sdk: bootstrap dial %s: %w", target, err)
	}
	// Make the post-dial phase cancellable too: closing the raw conn unblocks both
	// the SSH handshake (NewClientConn does NOT honor ClientConfig.Timeout) and the
	// later session command when ctx fires (its deadline or a caller cancel). stop()
	// cancels the AfterFunc on a normal return so it can't fire (or leak) afterward.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(bootstrapTimeout))

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, target, cfg)
	if err != nil {
		_ = conn.Close()
		return Bundle{}, fmt.Errorf("sdk: bootstrap handshake: %w", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return Bundle{}, fmt.Errorf("sdk: bootstrap session: %w", err)
	}
	defer sess.Close()

	cmd := scope
	if clientKind != "" {
		cmd = scope + " " + clientKind
	}
	out, err := sess.Output(cmd)
	if err != nil {
		return Bundle{}, fmt.Errorf("sdk: bootstrap command %q: %w", cmd, err)
	}

	var b Bundle
	if err := json.Unmarshal(out, &b); err != nil {
		return Bundle{}, fmt.Errorf("sdk: bootstrap decode: %w", err)
	}
	if b.Token == "" {
		return Bundle{}, errors.New("sdk: bootstrap returned an empty token")
	}
	return b, nil
}

// pinnedHostKey returns a HostKeyCallback that accepts only a host key whose
// SHA-256 fingerprint equals pin, failing closed otherwise. The fingerprint is
// public, so a plain compare is sufficient (no secret to leak via timing).
func pinnedHostKey(pin string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if got := ssh.FingerprintSHA256(key); got != pin {
			return fmt.Errorf("sdk: host key pin mismatch: got %s, want %s", got, pin)
		}
		return nil
	}
}
