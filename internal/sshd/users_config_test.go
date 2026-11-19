package sshd

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// These tests drive the SSH door with users built from a real server.yaml: the
// config loader and the config lookup, as shed-server wires them.

// keyLine is s's public key as an authorized_keys line.
func keyLine(s gossh.Signer) string {
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(s.PublicKey())))
}

// loadUsersConfig loads body (after a backend block) as server.yaml through
// the CLI loader and builds the lookup and the key auth from it as
// shed-server does.
func loadUsersConfig(t *testing.T, body string) (*config.ServerConfig, KeyAuth) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	yaml := "name: test\ndefault_backend: firecracker\nfirecracker:\n  instance_dir: /tmp/ti\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServerConfigForCLI(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var list map[string]config.UserConfig
	if cfg.UsersConfigured() {
		list = cfg.Auth.Users
	}
	lookup, err := users.NewConfigLookup(users.ConfigOptions{SSH: cfg.EffectiveSSHAuth(), GitHubCacheDir: t.TempDir(), Users: list})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	return cfg, KeyAuth{Mode: lookup.Mode(), MaxAuthTries: lookup.MaxAuthTries(), Users: lookup}
}

// configPeople is alice and bob (users who are not admins; bob also has a
// script key and an expired key) and root (an admin), from a real config.
type configPeople struct {
	alice, bob, bobScript, bobExpired, root, stranger gossh.Signer
	auth                                              KeyAuth
}

func newConfigPeople(t *testing.T) configPeople {
	t.Helper()
	p := configPeople{
		alice: testSigner(t), bob: testSigner(t), bobScript: testSigner(t), bobExpired: testSigner(t),
		root: testSigner(t), stranger: testSigner(t),
	}
	_, p.auth = loadUsersConfig(t, "auth:\n  mode: mtls\n  users:\n"+
		"    alice:\n      keys:\n        - {key: \""+keyLine(p.alice)+"\", label: laptop}\n"+
		"    bob:\n      keys:\n"+
		"        - {key: \""+keyLine(p.bob)+"\", label: laptop}\n"+
		"        - {key: \""+keyLine(p.bobScript)+"\", label: ci, script: true}\n"+
		"        - {key: \""+keyLine(p.bobExpired)+"\", label: old, expires: \"2020-01-01\"}\n"+
		"    root:\n      admin: true\n      keys:\n        - {key: \""+keyLine(p.root)+"\", label: desk}\n")
	return p
}

// twoShedBackend is identityBackend with bob's running shed "crate" beside
// alice's "box".
type twoShedBackend struct{ *identityBackend }

func (b twoShedBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	if name == "crate" {
		return &config.Shed{Name: "crate", User: "bob", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}, nil
	}
	return b.identityBackend.GetShed(ctx, name)
}

func configServer(t *testing.T, be backend.Backend, auth KeyAuth) (*Server, string) {
	t.Helper()
	s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, auth)
	if err != nil {
		t.Fatal(err)
	}
	return s, serveForTest(t, s)
}

// TestConfigUsersAtTheSSHDoor: a shell or command, sftp and a forwarded port,
// for two users and an admin from a real config. Each user enters their own
// shed and is told the other's does not exist; the admin is told the shed
// belongs to another user and enters neither; only the owners' channels reach
// the backend, each as its owner.
func TestConfigUsersAtTheSSHDoor(t *testing.T) {
	who := newConfigPeople(t)
	be := &identityBackend{}
	_, addr := configServer(t, twoShedBackend{be}, who.auth)

	const notFound = "not found"
	const forbidden = "belongs to another user; an admin can list, stop and delete it but not enter it"
	for _, tc := range []struct {
		name   string
		signer gossh.Signer
		shed   string
		want   string // "" for entered
	}{
		{"alice in box", who.alice, "box", ""},
		{"alice in crate", who.alice, "crate", notFound},
		{"bob in crate", who.bob, "crate", ""},
		{"bob in box", who.bob, "box", notFound},
		{"root in box", who.root, "box", forbidden},
		{"root in crate", who.root, "crate", forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dialWith(t, addr, tc.shed, tc.signer)
			for _, sub := range []string{"", "sftp"} {
				stderr, status := runOn(t, client, "true", sub)
				if tc.want == "" && status != 0 {
					t.Errorf("session (subsystem %q): status %d stderr %q, want entered", sub, status, stderr)
				}
				if tc.want != "" && (status != 1 || !strings.Contains(stderr, tc.want)) {
					t.Errorf("session (subsystem %q): status %d stderr %q, want refused with %q", sub, status, stderr, tc.want)
				}
			}
			conn, err := client.Dial("tcp", "localhost:8080")
			if tc.want == "" && err != nil {
				t.Errorf("forward: %v", err)
			}
			if tc.want != "" && err == nil {
				t.Error("forward allowed")
			}
			if conn != nil {
				_ = conn.Close()
			}
		})
	}
	execs, dials := be.seen()
	if len(execs) != 4 || len(dials) != 2 {
		t.Fatalf("backend reached by execs %v dials %v, want alice's and bob's (2+2, 1+1)", execs, dials)
	}
	for _, p := range append(execs, dials...) {
		if p != backend.UserPrincipal("alice", false) && p != backend.UserPrincipal("bob", false) {
			t.Errorf("the backend was reached as %s", p)
		}
	}
}

// TestConfigUsersKeysRefusedAtTheDoor: with a user list an unknown key and an
// expired key are refused at the handshake, under every username.
func TestConfigUsersKeysRefusedAtTheDoor(t *testing.T) {
	who := newConfigPeople(t)
	_, addr := configServer(t, twoShedBackend{&identityBackend{}}, who.auth)
	for name, s := range map[string]gossh.Signer{"stranger": who.stranger, "bob's expired key": who.bobExpired} {
		for _, user := range []string{"box", "crate", reservedBootstrapUser} {
			if dialUser(t, addr, user, s) {
				t.Errorf("%s admitted as %s", name, user)
			}
		}
	}
	if !dialUser(t, addr, "crate", who.bob) {
		t.Error("bob's own key was refused")
	}
}

// syncLog is a log output safe to write from the server's goroutines while
// the test reads it.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// loopbackRemote is a loopback peer address in an auth log line.
var loopbackRemote = regexp.MustCompile(`remote=127\.0\.0\.1:[0-9]+`)

// anyPort is an auth log with each loopback peer's port (a test client's,
// chosen by the kernel) written as PORT, so a test can compare whole lines.
func anyPort(text string) string {
	return loopbackRemote.ReplaceAllString(text, "remote=127.0.0.1:PORT")
}

// captureLog sends the standard logger's output, without its date prefix,
// to a buffer for the rest of the test.
func captureLog(t *testing.T) *syncLog {
	t.Helper()
	buf := &syncLog{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return buf
}

// TestConfigUsersAuthLogNamesTheUser: the accepted and refused auth log lines
// for a key the lookup resolves name the user it belongs to, whether they
// are an admin, and the key's label; an unknown key's line is as it always
// was; and on a server with no user list the implicit user is not named.
func TestConfigUsersAuthLogNamesTheUser(t *testing.T) {
	who := newConfigPeople(t)
	_, addr := configServer(t, twoShedBackend{&identityBackend{}}, who.auth)
	out := captureLog(t)
	fp := func(s gossh.Signer) string { return gossh.FingerprintSHA256(s.PublicKey()) }

	dialUser(t, addr, "crate", who.bob)
	dialUser(t, addr, "box", who.root)
	dialUser(t, addr, "crate", who.bobExpired)
	dialUser(t, addr, "crate", who.stranger)
	for _, want := range []string{
		fmt.Sprintf("SSH auth allowed: user=crate fingerprint=%s remote=127.0.0.1:PORT key_user=\"bob\" admin=false label=\"laptop\"\n", fp(who.bob)),
		fmt.Sprintf("SSH auth allowed: user=box fingerprint=%s remote=127.0.0.1:PORT key_user=\"root\" admin=true label=\"desk\"\n", fp(who.root)),
		fmt.Sprintf("SSH auth DENIED: user=crate fingerprint=%s remote=127.0.0.1:PORT (the key has expired) key_user=\"bob\" admin=false label=\"old\"\n", fp(who.bobExpired)),
		// An unknown key: nothing about any user.
		fmt.Sprintf("SSH auth DENIED: user=crate fingerprint=%s remote=127.0.0.1:PORT\n", fp(who.stranger)),
	} {
		if !strings.Contains(anyPort(out.String()), want) {
			t.Errorf("log does not contain %q:\n%s", want, out.String())
		}
	}

	// No user list: every key is the implicit user, who is not named.
	listed := testSigner(t)
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: config.SSHAuthEnforce, Users: userstest.Implicit(fp(listed))})
	if err != nil {
		t.Fatal(err)
	}
	dialUser(t, serveForTest(t, s), "box", listed)
	if want := fmt.Sprintf("SSH auth allowed: user=box fingerprint=%s remote=127.0.0.1:PORT\n", fp(listed)); !strings.Contains(anyPort(out.String()), want) {
		t.Errorf("log does not contain %q:\n%s", want, out.String())
	}
}

// TestLogUser: a name made of a shed name's or a special name's characters is
// printed as it is; anything else is quoted, so it cannot break or disguise
// the line.
func TestLogUser(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"box", "box"},
		{"git", "git"},
		{"_bootstrap", "_bootstrap"},
		{"_api", "_api"},
		{"my-shed-2", "my-shed-2"},
		{"", `""`},
		{"Git", `"Git"`},
		{"a b", `"a b"`},
		{"box\nSSH auth allowed: user=root", `"box\nSSH auth allowed: user=root"`},
		{"\x1b[2Kbox", `"\x1b[2Kbox"`},
		{"box\u202etig", `"box\u202etig"`},
		{"a\"b", `"a\"b"`},
	} {
		if got := logUser(tc.name); got != tc.want {
			t.Errorf("logUser(%q) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestAuthLogQuotesAHostileUsername: the username a client asks for, before
// it has authenticated, cannot forge or disguise an auth log line: a newline,
// an escape sequence and a bidirectional override all come out quoted, on the
// line they belong to, accepted key or not.
func TestAuthLogQuotesAHostileUsername(t *testing.T) {
	who := newConfigPeople(t)
	_, addr := configServer(t, twoShedBackend{&identityBackend{}}, who.auth)
	out := captureLog(t)
	fp := func(s gossh.Signer) string { return gossh.FingerprintSHA256(s.PublicKey()) }

	const forged = "SSH auth allowed: user=box fingerprint=FORGED"
	for _, user := range []string{"crate\n" + forged, "\x1b[2K\rbox", "box\u202etig"} {
		dialUser(t, addr, user, who.bob)
		dialUser(t, addr, user, who.stranger)
		for _, want := range []string{
			fmt.Sprintf("SSH auth allowed: user=%q fingerprint=%s remote=127.0.0.1:PORT key_user=\"bob\" admin=false label=\"laptop\"\n", user, fp(who.bob)),
			fmt.Sprintf("SSH auth DENIED: user=%q fingerprint=%s remote=127.0.0.1:PORT\n", user, fp(who.stranger)),
		} {
			if !strings.Contains(anyPort(out.String()), want) {
				t.Errorf("log does not contain %q:\n%s", want, out.String())
			}
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, forged) || strings.ContainsAny(line, "\x1b\r\u202e") {
			t.Errorf("a client's username forged or disguised a log line: %q", line)
		}
	}
}

// TestConfigUsersUnknownKeyIsNobodyInEveryMode: on a server with a user list
// an unknown key is never the implicit user and never an admin, whatever SSH
// mode the key auth is handed (config load allows only enforce; this holds for
// any caller).
func TestConfigUsersUnknownKeyIsNobodyInEveryMode(t *testing.T) {
	who := newConfigPeople(t)
	for _, mode := range []string{"", config.SSHAuthOff, config.SSHAuthWarn, config.SSHAuthEnforce} {
		auth := who.auth
		auth.Mode = mode
		s := &Server{keyAuth: auth}
		if p, err := s.principalFor(identityCtx(s, who.stranger.PublicKey())); err == nil {
			t.Errorf("mode %q: an unknown key is %s", mode, p)
		}
		_, addr := configServer(t, twoShedBackend{&identityBackend{}}, auth)
		if dialUser(t, addr, "box", who.stranger) {
			t.Errorf("mode %q: an unknown key was admitted", mode)
		}
	}
}

// TestConfigUsersScriptKeyAtBootstrap: a script key gets a control
// certificate at _bootstrap but never a credentials one; its user's other
// key gets both.
func TestConfigUsersScriptKeyAtBootstrap(t *testing.T) {
	who := newConfigPeople(t)
	s, addr := configServer(t, twoShedBackend{&identityBackend{}}, who.auth)
	s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TLSFingerprint: "sha256:abc", TokenTTL: time.Hour, CA: testCA(t)})

	bootstrap := func(signer gossh.Signer, scope string) (bootstrapBundle, string, error) {
		t.Helper()
		client := dialWith(t, addr, reservedBootstrapUser, signer)
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sess.Close() }()
		var out, errOut bytes.Buffer
		sess.Stdout, sess.Stderr = &out, &errOut
		err = sess.Run(scope + " cli csr=" + testCSR(t, elliptic.P256()))
		var b bootstrapBundle
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &b)
		}
		return b, errOut.String(), err
	}
	if _, stderr, err := bootstrap(who.bobScript, "credentials"); err == nil || !strings.Contains(stderr, "script key cannot take the credentials scope") {
		t.Errorf("script key credentials bootstrap: err %v stderr %q, want the script-key refusal", err, stderr)
	}
	if b, stderr, err := bootstrap(who.bobScript, "control"); err != nil || b.Scope != "control" {
		t.Errorf("script key control bootstrap: %v %q %+v", err, stderr, b)
	}
	if b, stderr, err := bootstrap(who.bob, "credentials"); err != nil || b.Scope != "credentials" {
		t.Errorf("bob's own key credentials bootstrap: %v %q %+v", err, stderr, b)
	}

	// The script flag reaches the session's principal.
	if p, err := s.principalFor(identityCtx(s, who.bobScript.PublicKey())); err != nil || !p.Script || p.User != "bob" || p.Admin {
		t.Errorf("script key principal %+v %v", p, err)
	}
}

// TestConfigUsersIdentityIsTheSigningKey is the x/crypto regression test with
// two named users from a real config: a key that was asked about but not used
// to sign does not become the session's identity, nor the certificate's at
// _bootstrap.
func TestConfigUsersIdentityIsTheSigningKey(t *testing.T) {
	who := newConfigPeople(t)
	asPeople := people{alice: who.alice, bob: who.bob}
	for name, signers := range queryThenSign(asPeople) {
		t.Run(name, func(t *testing.T) {
			be := &identityBackend{}
			s, addr := configServer(t, twoShedBackend{be}, who.auth)
			client := dialWith(t, addr, "box", signers...)
			if stderr, status := runOn(t, client, "true", ""); status != 0 {
				t.Fatalf("session status %d stderr %q: the identity is not alice's", status, stderr)
			}
			if execs, _ := be.seen(); len(execs) != 1 || execs[0] != backend.UserPrincipal("alice", false) {
				t.Fatalf("session identity %v, want alice", execs)
			}

			s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TokenTTL: time.Hour, CA: testCA(t)})
			bc := dialWith(t, addr, reservedBootstrapUser, signers...)
			sess, err := bc.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sess.Close() }()
			var out bytes.Buffer
			sess.Stdout = &out
			if err := sess.Run("control cli csr=" + testCSR(t, elliptic.P256())); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			var b bootstrapBundle
			if err := json.Unmarshal(out.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if cn := certCN(t, b.ClientCert); cn != gossh.FingerprintSHA256(who.alice.PublicKey()) {
				t.Fatalf("certificate issued for %s, want alice's key", cn)
			}
		})
	}
}

// TestConfigNoUserListUnchanged: with no auth.users every accepted key is the
// implicit user, who enters every shed whatever user its record carries.
func TestConfigNoUserListUnchanged(t *testing.T) {
	key := testSigner(t)
	cfg, auth := loadUsersConfig(t, "auth:\n  mode: mtls\n  ssh:\n    authorized_keys: [\""+keyLine(key)+"\"]\n")
	if cfg.UsersConfigured() || users.HasUserList(auth.Users) {
		t.Fatal("a config with no auth.users has a user list")
	}
	be := &identityBackend{}
	_, addr := configServer(t, twoShedBackend{be}, auth)
	for _, shed := range []string{"box", "crate"} {
		client := dialWith(t, addr, shed, key)
		if stderr, status := runOn(t, client, "true", ""); status != 0 {
			t.Errorf("implicit user in %s: status %d stderr %q", shed, status, stderr)
		}
	}
	execs, _ := be.seen()
	if len(execs) != 2 {
		t.Fatalf("execs %v", execs)
	}
	for _, p := range execs {
		if p != backend.ImplicitPrincipal() {
			t.Errorf("reached as %s, want the implicit user", p)
		}
	}
}

// certCN is the Subject CN of the PEM certificate in a bootstrap bundle.
func certCN(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("no certificate in the bundle")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// userCert is an SSH user certificate for signer's key, signed by a fresh CA
// and valid forever, and a signer that presents it.
func userCert(t *testing.T, signer gossh.Signer) (*gossh.Certificate, gossh.Signer) {
	t.Helper()
	cert := &gossh.Certificate{Key: signer.PublicKey(), CertType: gossh.UserCert, KeyId: "k", ValidPrincipals: []string{"box", "crate", reservedBootstrapUser}, ValidBefore: gossh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, testSigner(t)); err != nil {
		t.Fatal(err)
	}
	cs, err := gossh.NewCertSigner(cert, signer)
	if err != nil {
		t.Fatal(err)
	}
	return cert, cs
}

// TestConfigUsersCertificateRefusedAtTheDoor: on a server with a user list
// an SSH certificate is refused at the handshake under every username, even
// by a lookup that resolves it (here one that knows nothing of
// certificates: the door does not depend on the lookup refusing it), so
// whoever holds a user's key cannot log in as a second identity. The plain
// key still logs in. With no user list a certificate the lookup accepts logs
// in as before.
func TestConfigUsersCertificateRefusedAtTheDoor(t *testing.T) {
	root, alice := testSigner(t), testSigner(t)
	cert, certSigner := userCert(t, root)

	lookup := userstest.New()
	lookup.AddUser(users.User{Name: "root", Admin: true})
	lookup.AddUser(users.User{Name: "alice"})
	lookup.AddKey("root", users.Key{Fingerprint: gossh.FingerprintSHA256(root.PublicKey()), Source: users.SourceAdHoc})
	// The certificate as alice's, as a source that let one through would
	// have it.
	lookup.AddKey("alice", users.Key{Fingerprint: gossh.FingerprintSHA256(cert), Source: users.SourceGitHub})
	_, addr := identityServer(t, &identityBackend{}, lookup)
	for _, user := range []string{"box", "crate", reservedBootstrapUser} {
		if dialUser(t, addr, user, certSigner) {
			t.Errorf("a certificate of root's key was admitted as %s", user)
		}
	}
	if !dialUser(t, addr, "box", root) {
		t.Error("root's plain key was refused")
	}
	if dialUser(t, addr, "box", alice) {
		t.Error("alice's plain key, which the lookup does not hold, was admitted")
	}

	implicit := userstest.Implicit()
	implicit.AddKey(users.ImplicitName, users.Key{Fingerprint: gossh.FingerprintSHA256(cert), Source: users.SourceInline})
	_, addr = identityServer(t, &identityBackend{}, implicit)
	if !dialUser(t, addr, "box", certSigner) {
		t.Error("with no user list a certificate the lookup accepts was refused")
	}
}

// TestConfigUsersKeyExpiresWhileConnected: a hand-listed key that expires
// while its SSH connection stays open opens no new channel (a session or a
// forwarded port) and no new connection, and the channel it opened before
// expiry keeps working.
func TestConfigUsersKeyExpiresWhileConnected(t *testing.T) {
	alice, root := testSigner(t), testSigner(t)
	// RFC 3339 holds whole seconds: between one and two seconds from now.
	expires := time.Now().Add(2 * time.Second).UTC().Truncate(time.Second)
	_, auth := loadUsersConfig(t, "auth:\n  mode: mtls\n  users:\n"+
		"    alice:\n      keys:\n        - {key: \""+keyLine(alice)+"\", label: laptop, expires: \""+expires.Format(time.RFC3339)+"\"}\n"+
		"    root:\n      admin: true\n      keys:\n        - {key: \""+keyLine(root)+"\", label: desk}\n")
	be := &identityBackend{}
	_, addr := configServer(t, twoShedBackend{be}, auth)

	client := dialWith(t, addr, "box", alice)
	if stderr, status := runOn(t, client, "true", ""); status != 0 {
		t.Fatalf("session before expiry: status %d stderr %q", status, stderr)
	}
	open, err := client.Dial("tcp", "localhost:8080")
	if err != nil {
		t.Fatalf("forward before expiry: %v", err)
	}
	defer func() { _ = open.Close() }()
	echo := func(msg string) error {
		if _, err := open.Write([]byte(msg)); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(open, buf); err != nil {
			return err
		}
		if string(buf) != msg {
			return fmt.Errorf("read %q, want %q", buf, msg)
		}
		return nil
	}
	if err := echo("before"); err != nil {
		t.Fatalf("forward before expiry: %v", err)
	}

	time.Sleep(time.Until(expires) + 100*time.Millisecond)

	if stderr, status := runOn(t, client, "true", ""); status != 1 || !strings.Contains(stderr, "expired") {
		t.Errorf("new session after expiry: status %d stderr %q, want refused as expired", status, stderr)
	}
	if conn, err := client.Dial("tcp", "localhost:8080"); err == nil {
		_ = conn.Close()
		t.Error("new forward after expiry was opened")
	}
	if dialUser(t, addr, "box", alice) {
		t.Error("new connection after expiry was admitted")
	}
	if err := echo("after"); err != nil {
		t.Errorf("the forward opened before expiry stopped working: %v", err)
	}
	if execs, dials := be.seen(); len(execs) != 1 || len(dials) != 1 {
		t.Errorf("the backend was reached by execs %v dials %v, want only the channels opened before expiry", execs, dials)
	}
}
