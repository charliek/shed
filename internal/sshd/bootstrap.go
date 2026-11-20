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
	"github.com/charliek/shed/internal/users"
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

// errBootstrapOpenServer is what a server whose SSH allowlist is not enforced
// (an open server) answers every `_bootstrap` request with. Released clients
// match this exact line to recognise an open server and fall back to plain
// HTTP (cmd/shed/server_add.go): keep it byte-identical.
var errBootstrapOpenServer = errors.New("bootstrap requires auth.ssh.mode: enforce")

// BootstrapInfo is the static, server-known half of a bootstrap bundle: the
// HTTPS port, the TLS pin, the certificate TTL, and the CA that issues client
// certificates. shed-server fills it in once and wires it via SetBootstrap.
type BootstrapInfo struct {
	HTTPSPort      int
	TLSFingerprint string // sha256:... of the server cert; "" when TLS is off
	// TokenTTL is the lifetime of the client certificate the bootstrap issues
	// (auth.token_ttl, a name kept from the removed bearer-token mode).
	TokenTTL time.Duration
	// CA issues client certificates. Set only in mtls mode; with it nil the
	// bootstrap issues nothing.
	CA *servertls.CA
}

// bootstrapBundle is the JSON returned to a client over the _bootstrap SSH
// channel. The client knows the host it connected to, so it builds api_url
// itself from the returned port + scheme (the server cannot reliably know its
// own external hostname).
//
//	{auth_mode:"mtls", https_port, tls_cert_fingerprint, client_cert,
//	 scope, cert_serial, expires_at}
//
// A 0.9.x server in token mode answered the same request with a bearer-token
// bundle (auth_mode "token", token, token_id, http_port); this server never
// does, and carries no code that could. The clients keep decoding both shapes
// so they still reach servers that have not been upgraded.
//
// The SDK/CLI/Rust/Dart decoders MUST keep these json tags in sync.
type bootstrapBundle struct {
	AuthMode           string    `json:"auth_mode"`
	HTTPSPort          int       `json:"https_port"`
	TLSCertFingerprint string    `json:"tls_cert_fingerprint"`
	ClientCert         string    `json:"client_cert,omitempty"`
	Scope              string    `json:"scope"`
	CertSerial         string    `json:"cert_serial,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// SetBootstrap wires the static bundle metadata used by the _bootstrap SSH
// handler. Called once by shed-server before Start.
func (s *Server) SetBootstrap(info BootstrapInfo) {
	s.bootstrap = &info
}

// handleBootstrap serves a reserved `_bootstrap` SSH session: it issues a
// client certificate over the already-authenticated SSH channel and returns the
// bundle (the certificate plus the TLS pin + port) as a single JSON line. It never touches the shed routing or the shell wrap.
//
// The certificate is issued for the key the connection authenticated with,
// as recorded by the VerifiedPublicKeyCallback (connIdentity), to the user
// that key resolved to then, and only while the key still resolves to that
// same user for this channel (principalFor).
func (s *Server) handleBootstrap(sess ssh.Session) {
	id, _ := connIdentityFrom(sess.Context())
	bundle, err := s.mintBootstrap(id, sess.RawCommand())
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

// errBootstrapNotAuthorized refuses a credential to a connection whose key
// does not resolve, now, to the user it authenticated as.
var errBootstrapNotAuthorized = errors.New("bootstrap: key not authorized")

// mintBootstrap is the testable core of handleBootstrap: it authorizes the
// request and issues a credential, independent of the SSH session plumbing.
//
// It RE-VERIFIES authorization rather than trusting the transport: keys are
// surfaced by gliderlabs even in `off`/`warn` mode (PublicKeyHandler returns
// true for them), so the handler requires `enforce` AND a connection whose key
// the user lookup still resolves to the user it authenticated as
// (principalForIdentity) before issuing a credential. The certificate names
// that user (the implicit user included), so it authenticates as nobody else
// whatever happens to the key later. rawCmd is the client's request line:
// "<scope> [<client-kind>] [csr=<base64>]" (scope defaults to control).
//
// Config load allows enforce only in mtls mode, so an open server always stops
// at the first check. Its message is a protocol string: released clients match
// it as a whole line to recognise an open server and fall back to plain HTTP
// (cmd/shed/server_add.go), so it must stay byte-identical.
func (s *Server) mintBootstrap(id connIdentity, rawCmd string) (bootstrapBundle, error) {
	if !s.keyAuth.enforced() {
		return bootstrapBundle{}, errBootstrapOpenServer
	}
	if id.key == nil {
		return bootstrapBundle{}, errBootstrapNotAuthorized
	}
	p, err := s.principalForIdentity(id)
	if err != nil {
		log.Printf("bootstrap refused: %v", err)
		return bootstrapBundle{}, errBootstrapNotAuthorized
	}
	if s.bootstrap == nil {
		return bootstrapBundle{}, errors.New("bootstrap: credential issuance not configured")
	}

	req, err := parseBootstrapRequest(rawCmd)
	if err != nil {
		return bootstrapBundle{}, err
	}
	if req.scope == authtoken.ScopeCredentials && (p.Script || s.keyAuth.script(id.fingerprint)) {
		return bootstrapBundle{}, errBootstrapScriptCredentials
	}
	user := p.User
	if p.Implicit {
		user = users.ImplicitName
	}
	return s.mintClientCert(id.key, user, req)
}

// errBootstrapScriptCredentials refuses a credentials certificate to a script
// key: a key used by automation never holds its user's credential bus.
var errBootstrapScriptCredentials = errors.New("bootstrap: a script key cannot take the credentials scope")

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

// mintClientCert signs the submitted CSR with the server's internal CA and
// returns the leaf.
//
// The issued identity comes entirely from the authenticated SSH channel (the
// key fingerprint and the user it resolved to; "" is the implicit user) and
// this request line (scope, kind); the CSR contributes only its public key.
// See servertls.CA.SignClientCSR.
func (s *Server) mintClientCert(key gossh.PublicKey, user string, req bootstrapRequest) (bootstrapBundle, error) {
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
		csrDER, gossh.FingerprintSHA256(key), user, req.scope, req.kind, s.bootstrap.TokenTTL)
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
	// in order. Decoding — and rejecting duplicates — is decodeCSRArgs' job.
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
// the mobile app sends today; folding it here keeps one spelling in the audit
// trail without a client release. Nothing downstream consumes the kind beyond
// bookkeeping, so the normalization is safe to apply uniformly.
func normalizeClientKind(arg string) string {
	switch arg {
	case authtoken.ClientCLI, authtoken.ClientBroker, authtoken.ClientDesktop, authtoken.ClientMobile:
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
