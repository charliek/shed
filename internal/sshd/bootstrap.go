package sshd

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

const (
	// maxBootstrapCommand caps the raw `_bootstrap` request line before it is
	// parsed at all. A legitimate request is a scope, an optional client kind,
	// and — in mtls mode — a base64 P-256 CSR: a few hundred bytes. The CA caps
	// the decoded DER at 8 KiB, so 16 KiB of request line is far past anything
	// real while keeping the parse (and the base64 decode) bounded. The cap is
	// mode-independent: it guards the parser itself, before any mode-specific
	// handling decides whether the CSR even matters.
	maxBootstrapCommand = 16 << 10

	// csrArgKey names the CSR argument. It is matched as a key= prefix and honored
	// in ANY position after the scope, so `control csr=...` (no kind) and
	// `control cli csr=...` are both valid. Splitting on the FIRST `=` only is
	// load-bearing: standard base64 uses `=` as padding.
	csrArgKey = "csr"

	// legacyMobileClientKind is the literal the mobile app sends today. It is
	// normalized to authtoken.ClientMobile rather than dropped on the floor.
	legacyMobileClientKind = "shed-mobile"
)

// errBootstrapNeedsCSR is what an mtls-mode server tells a client that
// bootstrapped without a CSR — i.e. any client built before client-certificate
// support existed. It is a protocol string that older-client users will paste
// into issues verbatim: keep the wording stable, and keep it actionable.
var errBootstrapNeedsCSR = errors.New("this server requires auth.mode: mtls; upgrade shed (client certificate support)")

// BootstrapInfo is the static, server-known half of a bootstrap bundle: the
// HTTP/HTTPS ports, the TLS pin (empty when TLS is off), the credential TTL,
// the effective auth mode, and — in mtls mode — the CA that issues client
// certificates. shed-server fills it in once and wires it via SetBootstrap.
type BootstrapInfo struct {
	HTTPPort       int
	HTTPSPort      int
	TLSFingerprint string // sha256:... of the server cert; "" when TLS is off
	// TokenTTL is the lifetime of the credential the bootstrap issues
	// (auth.token_ttl). It is reused verbatim as the client-certificate TTL in
	// mtls mode: both are the same short-lived, re-mintable capability, so a
	// second knob would only be a second thing to get wrong.
	TokenTTL time.Duration
	// AuthMode is the server's effective auth.mode (config.AuthMode*). Only
	// config.AuthModeMTLS changes behavior here; every other value takes the
	// bearer-token path.
	AuthMode string
	// CA issues client certificates in mtls mode. Required there, nil
	// otherwise.
	CA *servertls.CA
}

// bootstrapBundle is the JSON returned to a client over the _bootstrap SSH
// channel. The client knows the host it connected to, so it builds api_url
// itself from the returned port + scheme (the server cannot reliably know its
// own external hostname).
//
// One struct carries both credential shapes, selected by auth_mode, with the
// mode-specific fields omitempty so each mode's object contains exactly its own
// keys and no empty placeholders:
//
//	token: {auth_mode:"token", http_port?, https_port, tls_cert_fingerprint,
//	        token, scope, token_id, expires_at}
//	mtls:  {auth_mode:"mtls", https_port, tls_cert_fingerprint, client_cert,
//	        scope, cert_serial, expires_at}
//
// A token bundle never carries client_cert/cert_serial; an mtls bundle never
// carries token/token_id/http_port (there is no bearer token to leak and no
// plain-HTTP listener to point at). auth_mode is new as of the mtls work — a
// pre-mtls client ignores the unknown key, which is exactly the legacy parity
// the token path preserves in the other direction.
//
// The SDK/CLI/Rust/Swift/Dart decoders MUST keep these json tags in sync.
type bootstrapBundle struct {
	AuthMode           string    `json:"auth_mode"`
	HTTPPort           int       `json:"http_port,omitempty"`
	HTTPSPort          int       `json:"https_port"`
	TLSCertFingerprint string    `json:"tls_cert_fingerprint"`
	Token              string    `json:"token,omitempty"`
	ClientCert         string    `json:"client_cert,omitempty"`
	Scope              string    `json:"scope"`
	TokenID            string    `json:"token_id,omitempty"`
	CertSerial         string    `json:"cert_serial,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// SetBootstrap wires the shared HTTP token store and the static bundle metadata
// used by the _bootstrap SSH handler. Called once by shed-server before Start.
//
// tokens may be nil in mtls mode: that path mints certificates and must never
// touch the token store, so the server does not hand it one.
func (s *Server) SetBootstrap(tokens *authtoken.Store, info BootstrapInfo) {
	s.tokens = tokens
	s.bootstrap = &info
}

// handleBootstrap serves a reserved `_bootstrap` SSH session: it issues an HTTP
// credential over the already-authenticated SSH channel and returns the bundle
// (bearer token or client certificate, plus the TLS pin + ports) as a single
// JSON line. It never touches the shed routing or the shell wrap.
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
// request and issues a credential, independent of the SSH session plumbing.
//
// It RE-VERIFIES authorization rather than trusting the transport: keys are
// surfaced by gliderlabs even in `off`/`warn` mode (PublicKeyHandler returns
// true for them), so the handler requires `enforce` AND an allowlisted key
// before issuing a credential. rawCmd is the client's request line:
// "<scope> [<client-kind>] [csr=<base64>]" (scope defaults to control).
func (s *Server) mintBootstrap(key gossh.PublicKey, rawCmd string) (bootstrapBundle, error) {
	if s.allowlist == nil || s.allowlist.Mode() != config.SSHAuthEnforce {
		return bootstrapBundle{}, errors.New("bootstrap requires auth.ssh.mode: enforce")
	}
	if key == nil || !s.allowlist.IsAuthorized(key) {
		return bootstrapBundle{}, errors.New("bootstrap: key not authorized")
	}
	if s.bootstrap == nil {
		return bootstrapBundle{}, errors.New("bootstrap: token issuance not configured")
	}

	req, err := parseBootstrapRequest(rawCmd)
	if err != nil {
		return bootstrapBundle{}, err
	}
	if s.bootstrap.AuthMode == config.AuthModeMTLS {
		return s.mintClientCert(key, req)
	}
	return s.mintToken(key, req)
}

// mintToken is the bearer-token half of the bootstrap: unchanged behavior,
// plus the new auth_mode field.
//
// A csr= argument is accepted and then COMPLETELY ignored — not decoded, not
// length-checked, not validated. That is deliberate legacy parity in both
// directions: a pre-mtls server ignores unknown request arguments, so a
// client that speaks CSRs to a token-mode server must get the same token
// bundle it would have got from the older binary, whatever the csr= value
// looks like. Erroring on a malformed CSR here would make a token-mode server
// stricter than the one it replaced.
func (s *Server) mintToken(key gossh.PublicKey, req bootstrapRequest) (bootstrapBundle, error) {
	if s.tokens == nil {
		return bootstrapBundle{}, errors.New("bootstrap: token issuance not configured")
	}
	token, rec, err := s.tokens.Mint(gossh.FingerprintSHA256(key), req.scope, req.kind, s.bootstrap.TokenTTL)
	if err != nil {
		return bootstrapBundle{}, fmt.Errorf("bootstrap: mint: %w", err)
	}
	return bootstrapBundle{
		// The field names the credential shape carried by THIS bundle, not the
		// server's auth.mode verbatim: a legacy open-mode server with
		// auth.ssh.mode: enforce also reaches here and also hands back a bearer
		// token, and the client's job is the same either way.
		AuthMode:           config.AuthModeToken,
		HTTPPort:           s.bootstrap.HTTPPort,
		HTTPSPort:          s.bootstrap.HTTPSPort,
		TLSCertFingerprint: s.bootstrap.TLSFingerprint,
		Token:              token,
		Scope:              req.scope,
		TokenID:            rec.ID,
		ExpiresAt:          rec.ExpiresAt,
	}, nil
}

// mintClientCert is the mtls half of the bootstrap: it signs the submitted CSR
// with the server's internal CA and returns the leaf. No bearer token is minted
// and the token store is never touched — in mtls mode the server is not even
// given one.
//
// The issued identity comes entirely from the authenticated SSH channel (the
// key fingerprint) and this request line (scope, kind); the CSR contributes
// only its public key. See servertls.CA.SignClientCSR.
// clientProtocolErrors are the CA errors that are part of the enrollment
// protocol: each describes something the CLIENT can fix (regenerate the key,
// use a supported algorithm) or must be told about (the CA needs rotating), and
// each has a stable string clients may match on. Everything else — a signing
// failure, an entropy failure, a bug — is an internal condition: it is logged
// server-side and reported generically, so implementation detail never crosses
// the channel.
var clientProtocolErrors = []error{
	servertls.ErrCSRTooLarge,
	servertls.ErrCSRInvalidDER,
	servertls.ErrCSRInvalidSignature,
	servertls.ErrCSRUnsupportedKey,
	servertls.ErrCSRWeakSignature,
	servertls.ErrCAExpiringSoon,
}

// clientIssuanceError maps a SignClientCSR failure onto what the client is
// allowed to see.
func clientIssuanceError(err error) error {
	for _, protocolErr := range clientProtocolErrors {
		if errors.Is(err, protocolErr) {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	log.Printf("bootstrap: certificate issuance failed: %v", err)
	return errors.New("bootstrap: certificate issuance failed")
}

func (s *Server) mintClientCert(key gossh.PublicKey, req bootstrapRequest) (bootstrapBundle, error) {
	if s.bootstrap.CA == nil {
		return bootstrapBundle{}, errors.New("bootstrap: client certificate issuance not configured")
	}
	if len(req.csrArgs) == 0 {
		return bootstrapBundle{}, errBootstrapNeedsCSR
	}
	csrDER, err := decodeCSRArgs(req.csrArgs)
	if err != nil {
		return bootstrapBundle{}, err
	}

	certDER, err := s.bootstrap.CA.SignClientCSR(
		csrDER, gossh.FingerprintSHA256(key), req.scope, req.kind, s.bootstrap.TokenTTL)
	if err != nil {
		return bootstrapBundle{}, clientIssuanceError(err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return bootstrapBundle{}, fmt.Errorf("bootstrap: parse issued certificate: %w", err)
	}

	return bootstrapBundle{
		AuthMode:           config.AuthModeMTLS,
		HTTPSPort:          s.bootstrap.HTTPSPort,
		TLSCertFingerprint: s.bootstrap.TLSFingerprint,
		ClientCert:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})),
		Scope:              req.scope,
		// Lower-case hex, the form every certificate tool prints; the client
		// treats it as an opaque string for logs and revocation bookkeeping.
		CertSerial: leaf.SerialNumber.Text(16),
		ExpiresAt:  leaf.NotAfter,
	}, nil
}

// bootstrapRequest is a parsed `_bootstrap` request line.
type bootstrapRequest struct {
	scope string
	kind  string // canonical authtoken client kind, or "" when absent/unknown
	// csrArgs holds the raw (still base64) value of every csr= argument seen,
	// in order. Decoding — and rejecting duplicates — is the mtls path's job,
	// because the token path must not validate this at all.
	csrArgs []string
}

// parseBootstrapRequest parses "<scope> [<client-kind>] [csr=<base64>]".
//
// The scope is positional and required-by-position (it defaults to control only
// when the line is empty). Everything after it is order-independent: any
// argument of the form csr=<value> is collected as a CSR, and the first
// remaining argument is the client kind. An unrecognized kind is dropped (""),
// as it always was — the kind is advisory bookkeeping.
func parseBootstrapRequest(rawCmd string) (bootstrapRequest, error) {
	if len(rawCmd) > maxBootstrapCommand {
		return bootstrapRequest{}, errors.New("bootstrap: request too long")
	}

	fields := strings.Fields(rawCmd)
	req := bootstrapRequest{scope: authtoken.ScopeControl}
	if len(fields) == 0 {
		return req, nil
	}
	req.scope = fields[0]
	if !authtoken.ValidScope(req.scope) {
		return bootstrapRequest{}, fmt.Errorf("bootstrap: invalid scope %q", req.scope)
	}

	kindSeen := false
	for _, arg := range fields[1:] {
		// Cut splits at the FIRST "=" only, so base64 padding stays in the
		// value rather than being mistaken for another key/value boundary.
		if k, v, isKV := strings.Cut(arg, "="); isKV && k == csrArgKey {
			req.csrArgs = append(req.csrArgs, v)
			continue
		}
		// Only the first non-csr argument is considered as the kind, matching
		// the pre-mtls parser (which only ever looked at position 1).
		if !kindSeen {
			kindSeen = true
			req.kind = normalizeClientKind(arg)
		}
	}
	return req, nil
}

// normalizeClientKind maps a wire client-kind spelling to its canonical
// authtoken constant, returning "" for anything unrecognized.
//
// "shed-mobile" is normalized rather than rejected because that is the literal
// the mobile app sends today; folding it here (in both token and mtls modes)
// keeps one spelling in the audit trail without a client release. Nothing
// downstream consumes the kind beyond bookkeeping, so the normalization is
// safe to apply uniformly.
func normalizeClientKind(arg string) string {
	switch arg {
	case authtoken.ClientCLI, authtoken.ClientHostAgent, authtoken.ClientDesktop, authtoken.ClientMobile:
		return arg
	case legacyMobileClientKind:
		return authtoken.ClientMobile
	default:
		return ""
	}
}

// decodeCSRArgs turns the collected csr= values into DER. Exactly one is
// required: two csr= arguments mean the client built the line wrong, and
// silently picking one of them would issue a certificate for a key the client
// may not be the one holding.
func decodeCSRArgs(values []string) ([]byte, error) {
	if len(values) > 1 {
		return nil, errors.New("bootstrap: duplicate csr argument")
	}
	if values[0] == "" {
		return nil, errors.New("bootstrap: empty csr")
	}
	// Strict() rejects non-canonical trailing bits; the standard alphabet
	// rejects the URL-safe one ("-"/"_"). Either way the client is emitting
	// something other than the agreed encoding, and a partial decode must not
	// be fed to the CSR parser.
	der, err := base64.StdEncoding.Strict().DecodeString(values[0])
	if err != nil {
		return nil, errors.New("bootstrap: csr: invalid base64")
	}
	return der, nil
}
