package sshd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users/userstest"
)

func testKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	return testSigner(t).PublicKey()
}

// TestBootstrapOpenServerRefusal pins the exact refusal an open server (one
// whose SSH allowlist is not enforced) gives every `_bootstrap` request.
// Released clients match this line verbatim to recognise an open server and
// fall back to plain HTTP (cmd/shed/server_add.go openServerMarker), so it must
// stay byte-identical whatever the request carries and whatever else is wired.
// The literal is spelled out here, not taken from errBootstrapOpenServer, so a
// change to the variable fails this test.
func TestBootstrapOpenServerRefusal(t *testing.T) {
	const want = "bootstrap requires auth.ssh.mode: enforce"
	key := testKey(t)
	listed := userstest.Implicit(gossh.FingerprintSHA256(key))
	csr := "control cli csr=" + testCSR(t, elliptic.P256())
	for _, tc := range []struct {
		name string
		auth KeyAuth
	}{
		{"no auth.ssh block", KeyAuth{}},
		{"auth.ssh off", KeyAuth{Mode: config.SSHAuthOff, Users: listed}},
		// In warn mode a listed key is admitted at the transport, so the
		// handler must refuse on its own.
		{"auth.ssh warn with the key listed", KeyAuth{Mode: config.SSHAuthWarn, Users: listed}},
	} {
		for _, cmd := range []string{"", "control", "control cli", "credentials host-agent", csr} {
			t.Run(tc.name+"/"+strings.SplitN(cmd, " csr=", 2)[0], func(t *testing.T) {
				s := &Server{keyAuth: tc.auth}
				// Even a server that somehow has a CA wired must not issue.
				s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TokenTTL: time.Hour, CA: testCA(t)})
				_, err := s.mintBootstrapKey(key, cmd)
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want exactly %q", err, want)
				}
			})
		}
	}
}

func TestMintBootstrapRejectsInvalidScope(t *testing.T) {
	key := testKey(t)
	s := mtlsServer(t, key, testCA(t))
	if _, err := s.mintBootstrapKey(key, "admin csr="+testCSR(t, elliptic.P256())); err == nil {
		t.Fatal("expected error for admin (removed) scope")
	}
}

func TestMintBootstrapUnconfigured(t *testing.T) {
	key := testKey(t)
	s := &Server{keyAuth: enforceKeys(key)} // no SetBootstrap
	_, err := s.mintBootstrapKey(key, "control csr="+testCSR(t, elliptic.P256()))
	if err == nil || err.Error() != "bootstrap: credential issuance not configured" {
		t.Fatalf("error = %v, want the not-configured refusal", err)
	}
}

// TestParseBootstrapRequestDefaultsToControl: an empty request line parses as
// the control scope with no kind and no CSR (an mtls server then refuses it for
// the missing CSR — see TestMintBootstrapMTLSRequiresCSR).
func TestParseBootstrapRequestDefaultsToControl(t *testing.T) {
	req, err := parseBootstrapRequest("")
	if err != nil {
		t.Fatal(err)
	}
	if req.scope != authtoken.ScopeControl || req.kind != "" || len(req.csrArgs) != 0 {
		t.Errorf("parseBootstrapRequest(\"\") = %+v, want control scope, no kind, no csr", req)
	}
}

// --- mtls bootstrap ---------------------------------------------------------

// testCA returns a freshly generated CA in a temp state dir.
func testCA(t *testing.T) *servertls.CA {
	t.Helper()
	dir := t.TempDir()
	ca, err := servertls.LoadOrGenerateCA(filepath.Join(dir, "ca_cert.pem"), filepath.Join(dir, "ca_key.pem"))
	if err != nil {
		t.Fatalf("LoadOrGenerateCA: %v", err)
	}
	return &ca
}

// testCSR returns a standard-base64 CSR on the given curve, as a client would
// put it on the request line.
func testCSR(t *testing.T, curve elliptic.Curve) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		// Deliberately requests an identity; the server must ignore it.
		Subject: pkix.Name{CommonName: "attacker-chosen", Organization: []string{"attacker-chosen"}},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// enforceKeys is enforce-mode key auth in which exactly the given keys belong
// to the implicit user (a server with no user list).
func enforceKeys(keys ...gossh.PublicKey) KeyAuth {
	fps := make([]string, len(keys))
	for i, k := range keys {
		fps[i] = gossh.FingerprintSHA256(k)
	}
	return KeyAuth{Mode: config.SSHAuthEnforce, Users: userstest.Implicit(fps...)}
}

// mtlsServer builds an enforce-mode mtls server whose user lookup resolves the
// given key.
func mtlsServer(t *testing.T, authorized gossh.PublicKey, ca *servertls.CA) *Server {
	t.Helper()
	s := &Server{keyAuth: enforceKeys(authorized)}
	s.SetBootstrap(BootstrapInfo{
		HTTPSPort:      8443,
		TLSFingerprint: "sha256:abc",
		TokenTTL:       time.Hour,
		CA:             ca,
	})
	return s
}

// bundleKeys marshals a bundle the way the wire does and returns its sorted
// JSON keys.
func bundleKeys(t *testing.T, b bootstrapBundle) []string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Errorf("bundle keys = %v, want %v", got, sorted)
	}
}

// TestMintBootstrapMTLSBundle asserts the bundle's exact wire shape (no bearer
// token, token_id or http_port: the server has no code that could mint them)
// and that the issued leaf is a real, CA-verifiable client cert bound to the
// SSH key that asked for it.
func TestMintBootstrapMTLSBundle(t *testing.T) {
	key := testKey(t)
	ca := testCA(t)
	s := mtlsServer(t, key, ca)

	bundle, err := s.mintBootstrapKey(key, "credentials host-agent csr="+testCSR(t, elliptic.P256()))
	if err != nil {
		t.Fatalf("mintBootstrap: %v", err)
	}

	assertKeys(t, bundleKeys(t, bundle), []string{
		"auth_mode", "cert_serial", "client_cert", "expires_at", "https_port", "scope", "tls_cert_fingerprint",
	})
	if bundle.AuthMode != config.AuthModeMTLS {
		t.Errorf("auth_mode = %q, want mtls", bundle.AuthMode)
	}
	if bundle.HTTPSPort != 8443 || bundle.TLSCertFingerprint != "sha256:abc" {
		t.Errorf("bundle metadata not propagated: %+v", bundle)
	}
	if bundle.Scope != authtoken.ScopeCredentials {
		t.Errorf("scope = %q, want credentials", bundle.Scope)
	}
	if bundle.CertSerial == "" {
		t.Error("empty cert_serial")
	}

	block, rest := pem.Decode([]byte(bundle.ClientCert))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("client_cert is not a single CERTIFICATE PEM block: %q", bundle.ClientCert)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}
	if got, want := leaf.Subject.CommonName, gossh.FingerprintSHA256(key); got != want {
		t.Errorf("subject CN = %q, want the SSH key fingerprint %q", got, want)
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != authtoken.ScopeCredentials {
		t.Errorf("subject OU = %v, want [credentials]", leaf.Subject.OrganizationalUnit)
	}
	if len(leaf.Subject.Organization) != 1 || leaf.Subject.Organization[0] != authtoken.ClientHostAgent {
		t.Errorf("subject O = %v, want [host-agent]", leaf.Subject.Organization)
	}
	if !bundle.ExpiresAt.Equal(leaf.NotAfter) {
		t.Errorf("expires_at = %s, want leaf NotAfter %s", bundle.ExpiresAt, leaf.NotAfter)
	}
	if got, want := bundle.CertSerial, leaf.SerialNumber.Text(16); got != want {
		t.Errorf("cert_serial = %q, want %q", got, want)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("issued cert does not verify against the CA: %v", err)
	}
}

// TestMintBootstrapMTLSCSRPositionIndependent covers the grammar: csr= is
// honored before the kind, after it, and with no kind at all.
func TestMintBootstrapMTLSCSRPositionIndependent(t *testing.T) {
	cases := []struct {
		name     string
		cmd      func(csr string) string
		wantKind string
	}{
		{"kind then csr", func(c string) string { return "control cli csr=" + c }, authtoken.ClientCLI},
		{"csr then kind", func(c string) string { return "control csr=" + c + " cli" }, authtoken.ClientCLI},
		{"csr only", func(c string) string { return "control csr=" + c }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := testKey(t)
			s := mtlsServer(t, key, testCA(t))
			bundle, err := s.mintBootstrapKey(key, tc.cmd(testCSR(t, elliptic.P256())))
			if err != nil {
				t.Fatalf("mintBootstrap: %v", err)
			}
			block, _ := pem.Decode([]byte(bundle.ClientCert))
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if len(leaf.Subject.Organization) > 0 {
				got = leaf.Subject.Organization[0]
			}
			if got != tc.wantKind {
				t.Errorf("client kind = %q, want %q", got, tc.wantKind)
			}
		})
	}
}

// TestMintBootstrapMTLSRequiresCSR pins the exact upgrade message an old client
// (which sends no csr=) gets from an mtls server. It is a protocol string.
func TestMintBootstrapMTLSRequiresCSR(t *testing.T) {
	key := testKey(t)
	s := mtlsServer(t, key, testCA(t))
	for _, cmd := range []string{"", "control", "control cli", "credentials host-agent"} {
		_, err := s.mintBootstrapKey(key, cmd)
		if err == nil {
			t.Fatalf("%q: expected error", cmd)
		}
		const want = "this server requires auth.mode: mtls; upgrade shed (client certificate support)"
		if err.Error() != want {
			t.Errorf("%q: error = %q, want %q", cmd, err.Error(), want)
		}
	}
}

// TestMintBootstrapMTLSCSRErrors covers the parse-level rejections and the
// verbatim passthrough of the CA's validation ladder.
func TestMintBootstrapMTLSCSRErrors(t *testing.T) {
	valid := testCSR(t, elliptic.P256())
	cases := []struct {
		name    string
		cmd     string
		wantErr string
	}{
		{"duplicate csr", "control csr=" + valid + " csr=" + valid, "bootstrap: duplicate csr argument"},
		{"duplicate csr with kind between", "control csr=" + valid + " cli csr=" + valid, "bootstrap: duplicate csr argument"},
		{"empty csr", "control csr=", "bootstrap: empty csr"},
		{"invalid base64", "control csr=!!!", "bootstrap: csr: invalid base64"},
		{"url-safe alphabet", "control csr=a-_b", "bootstrap: csr: invalid base64"},
		{"trailing garbage", "control csr=" + valid + "@@", "bootstrap: csr: invalid base64"},
		{"non-canonical padding bits", "control csr=YR==", "bootstrap: csr: invalid base64"},
		{"not a csr", "control csr=" + base64.StdEncoding.EncodeToString([]byte("nope")), "bootstrap: csr: invalid DER"},
		{"trailing der", "control csr=" + base64.StdEncoding.EncodeToString(
			append(mustB64(t, valid), 0x00)), "bootstrap: csr: invalid DER"},
		{"wrong curve", "control csr=" + testCSR(t, elliptic.P384()), "bootstrap: csr: unsupported key type (need P-256)"},
		{"invalid scope", "admin csr=" + valid, `bootstrap: invalid scope "admin"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := testKey(t)
			s := mtlsServer(t, key, testCA(t))
			_, err := s.mintBootstrapKey(key, tc.cmd)
			if err == nil {
				t.Fatalf("expected error %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMintBootstrapRequestTooLong caps the raw line before parsing — it guards
// the parser, not the credential.
func TestMintBootstrapRequestTooLong(t *testing.T) {
	long := "control cli csr=" + strings.Repeat("A", 16<<10)
	key := testKey(t)
	_, err := mtlsServer(t, key, testCA(t)).mintBootstrapKey(key, long)
	if err == nil || err.Error() != "bootstrap: request too long" {
		t.Fatalf("error = %v, want bootstrap: request too long", err)
	}
}

// TestMintBootstrapMTLSWithoutCA fails closed rather than panicking when the
// allowlist is enforced but no CA was wired.
func TestMintBootstrapMTLSWithoutCA(t *testing.T) {
	key := testKey(t)
	s := mtlsServer(t, key, testCA(t))
	s.bootstrap.CA = nil
	_, err := s.mintBootstrapKey(key, "control csr="+testCSR(t, elliptic.P256()))
	if err == nil || !strings.Contains(err.Error(), "client certificate issuance not configured") {
		t.Fatalf("error = %v, want client-certificate-not-configured", err)
	}
}

// TestMintBootstrapClientKindNormalization pins the kind table, including the
// shed-mobile → mobile fold, as burned into the issued certificate.
func TestMintBootstrapClientKindNormalization(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"cli", authtoken.ClientCLI},
		{"host-agent", authtoken.ClientHostAgent},
		{"desktop", authtoken.ClientDesktop},
		{"mobile", authtoken.ClientMobile},
		{"shed-mobile", authtoken.ClientMobile},
		{"bogus", ""},
		{"CLI", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run("mtls/"+tc.arg, func(t *testing.T) {
			key := testKey(t)
			s := mtlsServer(t, key, testCA(t))
			cmd := strings.TrimRight("control "+tc.arg, " ") + " csr=" + testCSR(t, elliptic.P256())
			bundle, err := s.mintBootstrapKey(key, cmd)
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode([]byte(bundle.ClientCert))
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if len(leaf.Subject.Organization) > 0 {
				got = leaf.Subject.Organization[0]
			}
			if got != tc.want {
				t.Errorf("client kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMintBootstrapMTLSNeedsAuthorization keeps issuance behind its two gates:
// enforce mode and an allowlisted key.
func TestMintBootstrapMTLSNeedsAuthorization(t *testing.T) {
	csr := "control csr=" + testCSR(t, elliptic.P256())
	s := mtlsServer(t, testKey(t), testCA(t))
	if _, err := s.mintBootstrapKey(testKey(t), csr); err == nil {
		t.Error("expected error for unauthorized key")
	}
	if _, err := s.mintBootstrapKey(nil, csr); err == nil {
		t.Error("expected error for nil key")
	}
}

// TestMintBootstrapRequestLengthBoundary pins the exact edge of
// maxBootstrapCommand: a line of exactly the cap length must still parse and
// mint normally (the cap must not be off-by-one against the legitimate max),
// while one byte over is rejected with the same message the far-over-cap case
// uses. Padding is done with extra spaces, which strings.Fields collapses, so
// it inflates the raw line without changing what it parses to.
func TestMintBootstrapRequestLengthBoundary(t *testing.T) {
	pad := func(t *testing.T, cmd string, target int) string {
		t.Helper()
		if len(cmd) > target {
			t.Fatalf("test setup: base command (%d bytes) already exceeds target %d", len(cmd), target)
		}
		return cmd + strings.Repeat(" ", target-len(cmd))
	}

	t.Run("mtls exactly at cap accepted", func(t *testing.T) {
		key := testKey(t)
		s := mtlsServer(t, key, testCA(t))
		cmd := pad(t, "control cli csr="+testCSR(t, elliptic.P256()), maxBootstrapCommand)
		bundle, err := s.mintBootstrapKey(key, cmd)
		if err != nil {
			t.Fatalf("mintBootstrap at exact cap (%d bytes): %v", len(cmd), err)
		}
		if bundle.ClientCert == "" {
			t.Error("expected an issued client cert")
		}
	})

	t.Run("one byte over cap rejected", func(t *testing.T) {
		key := testKey(t)
		s := mtlsServer(t, key, testCA(t))
		cmd := pad(t, "control cli", maxBootstrapCommand) + " "
		if len(cmd) != maxBootstrapCommand+1 {
			t.Fatalf("test setup: cmd length = %d, want %d", len(cmd), maxBootstrapCommand+1)
		}
		_, err := s.mintBootstrapKey(key, cmd)
		if err == nil || err.Error() != "bootstrap: request too long" {
			t.Fatalf("error = %v, want bootstrap: request too long", err)
		}
	})
}

// TestMintBootstrapBareCSRWordIsNotCSRArg pins that the literal word "csr"
// with no '=' is an ordinary (unrecognized) client kind, not a CSR: only the
// key=value form is ever collected into csrArgs. With no real csr= present,
// this must fall through to the same upgrade error a client that sent nothing
// at all would get.
func TestMintBootstrapBareCSRWordIsNotCSRArg(t *testing.T) {
	t.Run("mtls", func(t *testing.T) {
		key := testKey(t)
		s := mtlsServer(t, key, testCA(t))
		_, err := s.mintBootstrapKey(key, "control csr")
		if err == nil || err.Error() != errBootstrapNeedsCSR.Error() {
			t.Fatalf("error = %v, want %v (bare \"csr\" word must not satisfy the CSR requirement)", err, errBootstrapNeedsCSR)
		}
	})
}

// TestMintBootstrapUppercaseCSRKeyNotRecognized pins that the csr= match is
// case-sensitive: "CSR=..." is never collected as a CSR argument, so the
// client gets the same upgrade error as if it had sent no csr= at all.
func TestMintBootstrapUppercaseCSRKeyNotRecognized(t *testing.T) {
	valid := testCSR(t, elliptic.P256())
	cmd := "control CSR=" + valid

	t.Run("mtls treated as no csr", func(t *testing.T) {
		key := testKey(t)
		s := mtlsServer(t, key, testCA(t))
		_, err := s.mintBootstrapKey(key, cmd)
		if err == nil || err.Error() != errBootstrapNeedsCSR.Error() {
			t.Fatalf("error = %v, want %v (CSR= must not satisfy the lowercase csr= requirement)", err, errBootstrapNeedsCSR)
		}
	})
}

// TestMintBootstrapCSRLiteralAsScope pins that a csr=... argument occupying
// field 0 (the scope position) is rejected as an invalid scope: the parser
// never treats field 0 as anything but the scope, however it's spelled.
func TestMintBootstrapCSRLiteralAsScope(t *testing.T) {
	valid := testCSR(t, elliptic.P256())
	cmd := "csr=" + valid
	wantErr := fmt.Sprintf("bootstrap: invalid scope %q", cmd)

	t.Run("mtls", func(t *testing.T) {
		key := testKey(t)
		s := mtlsServer(t, key, testCA(t))
		_, err := s.mintBootstrapKey(key, cmd)
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
	})
}

// TestMintBootstrapWhitespaceSeparators pins strings.Fields' behavior: tabs
// and runs of spaces between arguments parse identically to a single space.
func TestMintBootstrapWhitespaceSeparators(t *testing.T) {
	valid := testCSR(t, elliptic.P256())
	key := testKey(t)
	s := mtlsServer(t, key, testCA(t))
	bundle, err := s.mintBootstrapKey(key, "control\tcli   csr="+valid)
	if err != nil {
		t.Fatalf("mintBootstrap: %v", err)
	}
	if bundle.Scope != authtoken.ScopeControl {
		t.Errorf("scope = %q, want control", bundle.Scope)
	}
	block, _ := pem.Decode([]byte(bundle.ClientCert))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Subject.Organization) != 1 || leaf.Subject.Organization[0] != authtoken.ClientCLI {
		t.Errorf("client kind = %v, want [cli]", leaf.Subject.Organization)
	}
}

// TestMintBootstrapInvalidScopeEscapesControlBytes is the terminal-injection
// guard: an invalid scope carrying control/ANSI-escape bytes must reach the
// client only through the %q-escaped form, never as raw bytes a naive
// terminal would interpret.
func TestMintBootstrapInvalidScopeEscapesControlBytes(t *testing.T) {
	key := testKey(t)
	s := mtlsServer(t, key, testCA(t))
	scope := "\x1b[31mbogus"
	_, err := s.mintBootstrapKey(key, scope+" cli")
	if err == nil {
		t.Fatal("expected error for invalid scope")
	}
	if !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("error = %q, want it to contain the escaped %%q form of the ESC byte", err.Error())
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("error = %q, contains a raw ESC byte -- terminal injection risk", err.Error())
	}
}

// TestClientIssuanceError pins the whitelist directly, without going through
// SignClientCSR: every whitelisted CA sentinel passes through with the
// "bootstrap: " prefix (and remains errors.Is-matchable), while anything else
// — an arbitrary internal error — is replaced with the generic message, so
// implementation detail never crosses the channel.
func TestClientIssuanceError(t *testing.T) {
	for _, sentinel := range clientProtocolErrors {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("signing: %w", sentinel)
			got := clientIssuanceError(wrapped)
			want := "bootstrap: " + wrapped.Error()
			if got.Error() != want {
				t.Errorf("clientIssuanceError(%v) = %q, want %q", wrapped, got.Error(), want)
			}
			if !errors.Is(got, sentinel) {
				t.Errorf("clientIssuanceError(%v) lost the sentinel: errors.Is = false", wrapped)
			}
		})
	}

	t.Run("unexpected error is not passed through", func(t *testing.T) {
		got := clientIssuanceError(fmt.Errorf("some internal detail"))
		const want = "bootstrap: certificate issuance failed"
		if got.Error() != want {
			t.Errorf("clientIssuanceError = %q, want %q", got.Error(), want)
		}
	})
}

// mintBootstrapKey mints as a connection that authenticated with key just now
// would: its identity binds the user key resolves to now (newConnIdentity). A
// key that resolves to nobody gives a connection with no identity, which
// mintBootstrap refuses (after the open-server check, as on the wire).
func (s *Server) mintBootstrapKey(key gossh.PublicKey, rawCmd string) (bootstrapBundle, error) {
	var id connIdentity
	if key != nil {
		if bound, err := s.newConnIdentity(key, reservedBootstrapUser); err == nil {
			id = bound
		}
	}
	return s.mintBootstrap(id, rawCmd)
}
