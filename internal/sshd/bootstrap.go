package sshd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
)

// BootstrapInfo is the static, server-known half of a bootstrap bundle: the
// HTTP/HTTPS ports, the TLS pin (empty when TLS is off), and the token TTL.
// shed-server fills it in once and wires it via SetBootstrap.
type BootstrapInfo struct {
	HTTPPort       int
	HTTPSPort      int
	TLSFingerprint string // sha256:... of the server cert; "" when TLS is off
	TokenTTL       time.Duration
}

// bootstrapBundle is the JSON returned to a client over the _bootstrap SSH
// channel. The client knows the host it connected to, so it builds api_url
// itself from the returned port + scheme (the server cannot reliably know its
// own external hostname). The SDK/CLI decoders MUST keep these json tags in
// sync.
type bootstrapBundle struct {
	HTTPPort           int       `json:"http_port"`
	HTTPSPort          int       `json:"https_port"`
	TLSCertFingerprint string    `json:"tls_cert_fingerprint"`
	Token              string    `json:"token"`
	Scope              string    `json:"scope"`
	TokenID            string    `json:"token_id"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// SetBootstrap wires the shared HTTP token store and the static bundle metadata
// used by the _bootstrap SSH handler. Called once by shed-server before Start.
func (s *Server) SetBootstrap(tokens *authtoken.Store, info BootstrapInfo) {
	s.tokens = tokens
	s.bootstrap = &info
}

// handleBootstrap serves a reserved `_bootstrap` SSH session: it mints an HTTP
// bearer token over the already-authenticated SSH channel and returns the
// bundle (token + TLS pin + ports) as a single JSON line. It never touches the
// shed routing or the shell wrap.
func (s *Server) handleBootstrap(sess ssh.Session) {
	bundle, err := s.mintBootstrap(sess.PublicKey(), sess.RawCommand())
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "%v\n", err)
		_ = sess.Exit(1)
		return
	}
	if err := json.NewEncoder(sess).Encode(bundle); err != nil {
		log.Printf("bootstrap: write bundle: %v", err)
		_ = sess.Exit(1)
		return
	}
	_ = sess.Exit(0)
}

// mintBootstrap is the testable core of handleBootstrap: it authorizes the
// request and mints a token, independent of the SSH session plumbing.
//
// It RE-VERIFIES authorization rather than trusting the transport: keys are
// surfaced by gliderlabs even in `off`/`warn` mode (PublicKeyHandler returns
// true for them), so the handler requires `enforce` AND an allowlisted key
// before issuing a credential. rawCmd is the client's request line:
// "<scope> [<client-kind>]" (scope defaults to control).
func (s *Server) mintBootstrap(key gossh.PublicKey, rawCmd string) (bootstrapBundle, error) {
	if s.allowlist == nil || s.allowlist.Mode() != config.SSHAuthEnforce {
		return bootstrapBundle{}, errors.New("bootstrap requires auth.ssh.mode: enforce")
	}
	if key == nil || !s.allowlist.IsAuthorized(key) {
		return bootstrapBundle{}, errors.New("bootstrap: key not authorized")
	}
	if s.tokens == nil || s.bootstrap == nil {
		return bootstrapBundle{}, errors.New("bootstrap: token issuance not configured")
	}

	parts := strings.Fields(rawCmd)
	scope := authtoken.ScopeControl
	if len(parts) > 0 {
		scope = parts[0]
	}
	if !authtoken.ValidScope(scope) {
		return bootstrapBundle{}, fmt.Errorf("bootstrap: invalid scope %q", scope)
	}
	kind := ""
	if len(parts) > 1 {
		switch parts[1] {
		case authtoken.ClientCLI, authtoken.ClientHostAgent, authtoken.ClientDesktop:
			kind = parts[1]
		}
	}

	token, rec, err := s.tokens.Mint(gossh.FingerprintSHA256(key), scope, kind, s.bootstrap.TokenTTL)
	if err != nil {
		return bootstrapBundle{}, fmt.Errorf("bootstrap: mint: %w", err)
	}
	return bootstrapBundle{
		HTTPPort:           s.bootstrap.HTTPPort,
		HTTPSPort:          s.bootstrap.HTTPSPort,
		TLSCertFingerprint: s.bootstrap.TLSFingerprint,
		Token:              token,
		Scope:              scope,
		TokenID:            rec.ID,
		ExpiresAt:          rec.ExpiresAt,
	}, nil
}
