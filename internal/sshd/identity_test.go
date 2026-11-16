package sshd

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"path/filepath"
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

// identityBackend holds alice's running shed "box" and records the principal
// every exec and dial that reached it was made for.
type identityBackend struct {
	backend.Backend
	mu    sync.Mutex
	execs []backend.Principal
	dials []backend.Principal
}

func (b *identityBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	if name != "box" {
		return nil, config.ErrShedNotFoundSentinel
	}
	return &config.Shed{Name: "box", User: "alice", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}, nil
}

func (b *identityBackend) Exec(ctx context.Context, _ string, opts backend.ExecOptions) error {
	p, _ := backend.PrincipalFrom(ctx)
	b.mu.Lock()
	b.execs = append(b.execs, p)
	b.mu.Unlock()
	if opts.Stdout != nil {
		_, _ = opts.Stdout.Write([]byte("ran\n"))
	}
	return nil
}

func (b *identityBackend) DialService(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
	p, _ := backend.PrincipalFrom(ctx)
	b.mu.Lock()
	b.dials = append(b.dials, p)
	b.mu.Unlock()
	server, client := net.Pipe()
	go func() { _, _ = io.Copy(server, server) }()
	return client, nil
}

func (b *identityBackend) seen() (execs, dials []backend.Principal) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backend.Principal(nil), b.execs...), append([]backend.Principal(nil), b.dials...)
}

// people is a user lookup with alice and bob (not admins) and root (an admin),
// each owning one fresh key, in enforce mode.
type people struct {
	alice, bob, root gossh.Signer
	lookup           *userstest.Fake
}

func newPeople(t *testing.T) people {
	t.Helper()
	p := people{alice: testSigner(t), bob: testSigner(t), root: testSigner(t), lookup: userstest.New()}
	for name, s := range map[string]gossh.Signer{"alice": p.alice, "bob": p.bob, "root": p.root} {
		p.lookup.AddUser(users.User{Name: name, Admin: name == "root"})
		p.lookup.AddKey(name, users.Key{Fingerprint: gossh.FingerprintSHA256(s.PublicKey()), Source: users.SourceInline})
	}
	return p
}

func identityServer(t *testing.T, be backend.Backend, lookup users.Lookup) (*Server, string) {
	t.Helper()
	s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: config.SSHAuthEnforce, Users: lookup})
	if err != nil {
		t.Fatal(err)
	}
	return s, serveForTest(t, s)
}

func dialWith(t *testing.T, addr, user string, signers ...gossh.Signer) *gossh.Client {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signers...)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// runOn runs cmd in a new session channel on client and returns its stderr
// and exit status. subsystem, when set, is requested instead of a command.
func runOn(t *testing.T, client *gossh.Client, cmd, subsystem string) (string, int) {
	t.Helper()
	if subsystem != "" {
		return runSubsystemOn(t, client, subsystem)
	}
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errOut
	err = sess.Run(cmd)
	var exit *gossh.ExitError
	switch {
	case err == nil:
		return errOut.String(), 0
	case errors.As(err, &exit):
		return errOut.String(), exit.ExitStatus()
	}
	t.Fatalf("run: %v", err)
	return "", -1
}

// runSubsystemOn requests subsystem on a new session channel on client and
// returns its stderr and the exit status the server sent. A gossh.Session
// cannot wait for a subsystem (Wait needs a command it started), so this
// works on the session channel itself; a channel the server closes without
// an exit status fails the test.
func runSubsystemOn(t *testing.T, client *gossh.Client, subsystem string) (string, int) {
	t.Helper()
	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = ch.Close() }()
	status := make(chan int, 1)
	go func() {
		st := -1
		for r := range reqs {
			if r.Type == "exit-status" && len(r.Payload) == 4 {
				st = int(binary.BigEndian.Uint32(r.Payload))
			}
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
		status <- st
	}()
	ok, err := ch.SendRequest("subsystem", true, gossh.Marshal(struct{ Name string }{subsystem}))
	if err != nil || !ok {
		t.Fatalf("subsystem %s: granted %v, %v", subsystem, ok, err)
	}
	stdout := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, ch); close(stdout) }()
	var errOut bytes.Buffer
	_, _ = io.Copy(&errOut, ch.Stderr())
	<-stdout
	st := <-status
	if st < 0 {
		t.Fatalf("subsystem %s: the server sent no exit status (stderr %q)", subsystem, errOut.String())
	}
	return errOut.String(), st
}

// TestSSHChannelsDecidedByTheGuard: a shell or command, sftp and a forwarded
// port on alice's shed. alice reaches it; bob (not an admin) is told it does
// not exist; root (an admin) is told it belongs to another user, before any
// wake, and enters nothing.
func TestSSHChannelsDecidedByTheGuard(t *testing.T) {
	who := newPeople(t)
	be := &identityBackend{}
	_, addr := identityServer(t, be, who.lookup)

	for _, tc := range []struct {
		name       string
		signer     gossh.Signer
		wantStatus int
		wantStderr string
		wantDial   bool
	}{
		{"alice", who.alice, 0, "", true},
		{"bob", who.bob, 1, "Error: shed 'box' not found", false},
		{"root", who.root, 1, "Error: shed 'box' belongs to another user; an admin can list, stop and delete it but not enter it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dialWith(t, addr, "box", tc.signer)
			for _, sub := range []string{"", "sftp"} {
				stderr, status := runOn(t, client, "true", sub)
				if status != tc.wantStatus || !strings.Contains(stderr, tc.wantStderr) {
					t.Errorf("%s session (subsystem %q): status %d stderr %q, want %d %q", tc.name, sub, status, stderr, tc.wantStatus, tc.wantStderr)
				}
			}
			conn, err := client.Dial("tcp", "localhost:8080")
			if tc.wantDial {
				if err != nil {
					t.Errorf("%s forward: %v", tc.name, err)
				} else {
					_ = conn.Close()
				}
			} else if err == nil {
				_ = conn.Close()
				t.Errorf("%s forward to alice's shed was allowed", tc.name)
			}
		})
	}
	execs, dials := be.seen()
	if len(execs) != 2 || len(dials) != 1 {
		t.Fatalf("backend reached by execs %v dials %v, want alice's two and one", execs, dials)
	}
	for _, p := range append(execs, dials...) {
		if p != backend.UserPrincipal("alice", false) {
			t.Errorf("the backend was reached as %s, want alice", p)
		}
	}
}

// TestSSHPrincipalResolvedPerChannel: the principal is resolved from the
// connection's key again for every channel. Once the key leaves the user
// lookup, the open connection can open no new session, sftp or forward.
func TestSSHPrincipalResolvedPerChannel(t *testing.T) {
	who := newPeople(t)
	be := &identityBackend{}
	_, addr := identityServer(t, be, who.lookup)
	client := dialWith(t, addr, "box", who.alice)
	if _, status := runOn(t, client, "true", ""); status != 0 {
		t.Fatalf("first session status %d", status)
	}

	who.lookup.RemoveKey(gossh.FingerprintSHA256(who.alice.PublicKey()))
	stderr, status := runOn(t, client, "true", "")
	if status != 1 || !strings.Contains(stderr, "no longer authorized") {
		t.Errorf("session after the key left: status %d stderr %q", status, stderr)
	}
	if _, status := runOn(t, client, "", "sftp"); status != 1 {
		t.Errorf("sftp after the key left: status %d", status)
	}
	if conn, err := client.Dial("tcp", "localhost:8080"); err == nil {
		_ = conn.Close()
		t.Error("forward after the key left was allowed")
	}
	if execs, dials := be.seen(); len(execs) != 1 || len(dials) != 0 {
		t.Errorf("backend reached after the key left: execs %d dials %d", len(execs), len(dials))
	}
}

// unsignedKey offers a key the server accepts when asked about it, and then
// answers with a signature in a format the key cannot have. The server refuses
// that attempt without dropping the connection (a signature that does not
// verify would drop it), and the client goes on to its next key: the key was
// asked about and accepted, and never used to sign.
type unsignedKey struct{ pub gossh.PublicKey }

func (s unsignedKey) PublicKey() gossh.PublicKey { return s.pub }
func (s unsignedKey) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	return &gossh.Signature{Format: gossh.KeyAlgoRSA, Blob: []byte("not a signature")}, nil
}

// queryThenSign returns the key sequences in which bob's key is asked about
// and accepted, and then alice's key signs. If the server took the identity
// from a key it was only asked about, it would be bob's: the first sequence
// catches an identity taken from the first accepted key, the second one taken
// from the last accepted key while alice's key sat in a cache of more than one
// key (asked about first, then bob's, then used to sign without being asked
// about again).
func queryThenSign(who people) map[string][]gossh.Signer {
	return map[string][]gossh.Signer{
		"bob asked about, alice signs":                    {unsignedKey{who.bob.PublicKey()}, who.alice},
		"alice asked about, bob asked about, alice signs": {unsignedKey{who.alice.PublicKey()}, unsignedKey{who.bob.PublicKey()}, who.alice},
	}
}

// TestSSHIdentityIsTheSigningKey is the x/crypto regression test for the
// session's identity: a key that was queried but not used to sign does not
// become the identity.
func TestSSHIdentityIsTheSigningKey(t *testing.T) {
	who := newPeople(t)
	for name, signers := range queryThenSign(who) {
		t.Run(name, func(t *testing.T) {
			be := &identityBackend{}
			_, addr := identityServer(t, be, who.lookup)
			client := dialWith(t, addr, "box", signers...)
			stderr, status := runOn(t, client, "true", "")
			if status != 0 {
				t.Fatalf("session status %d stderr %q: the identity is not alice's", status, stderr)
			}
			execs, _ := be.seen()
			if len(execs) != 1 || execs[0] != backend.UserPrincipal("alice", false) {
				t.Fatalf("session identity %v, want alice", execs)
			}
		})
	}
}

// TestBootstrapIdentityIsTheSigningKey is the same regression test at
// _bootstrap: the certificate is issued for the key that signed, never for a
// key that was only queried.
func TestBootstrapIdentityIsTheSigningKey(t *testing.T) {
	who := newPeople(t)
	for name, signers := range queryThenSign(who) {
		t.Run(name, func(t *testing.T) { bootstrapAsSigners(t, who, signers) })
	}
}

func bootstrapAsSigners(t *testing.T, who people, signers []gossh.Signer) {
	s, addr := identityServer(t, &identityBackend{}, who.lookup)
	s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TLSFingerprint: "sha256:abc", TokenTTL: time.Hour, CA: testCA(t)})

	client := dialWith(t, addr, reservedBootstrapUser, signers...)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	if err := sess.Run("control cli csr=" + testCSR(t, elliptic.P256())); err != nil {
		t.Fatalf("bootstrap: %v (stderr %q)", err, errOut.String())
	}
	var bundle bootstrapBundle
	if err := json.Unmarshal(out.Bytes(), &bundle); err != nil {
		t.Fatalf("bundle: %v: %s", err, out.String())
	}
	block, _ := pem.Decode([]byte(bundle.ClientCert))
	if block == nil {
		t.Fatal("no certificate in the bundle")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if want := gossh.FingerprintSHA256(who.alice.PublicKey()); leaf.Subject.CommonName != want {
		t.Fatalf("certificate issued for %s, want alice's key %s", leaf.Subject.CommonName, want)
	}
}

// TestBootstrapRefusedOnceTheKeyLeaves: _bootstrap resolves the connection's
// key for its own channel, so a key that left the lookup after the
// connection opened gets no certificate.
func TestBootstrapRefusedOnceTheKeyLeaves(t *testing.T) {
	who := newPeople(t)
	s, addr := identityServer(t, &identityBackend{}, who.lookup)
	s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TokenTTL: time.Hour, CA: testCA(t)})
	client := dialWith(t, addr, reservedBootstrapUser, who.alice)
	who.lookup.RemoveKey(gossh.FingerprintSHA256(who.alice.PublicKey()))
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	if err := sess.Run("control cli csr=" + testCSR(t, elliptic.P256())); err == nil || out.Len() != 0 {
		t.Fatalf("bootstrap after the key left: err %v stdout %q", err, out.String())
	}
	if !strings.Contains(errOut.String(), "key not authorized") {
		t.Errorf("stderr %q", errOut.String())
	}
}

// TestUnknownKeyIsImplicitOnlyWithoutAUserList: in off and warn mode an
// unresolved key is the implicit user, as before users existed, but only on
// a server whose lookup has the implicit user; with a user list it is nobody.
func TestUnknownKeyIsImplicitOnlyWithoutAUserList(t *testing.T) {
	stranger := testSigner(t)
	for _, tc := range []struct {
		name   string
		lookup users.Lookup
		mode   string
		want   bool
	}{
		{"off, no lookup", nil, "", true},
		{"warn, no user list", userstest.Implicit(), config.SSHAuthWarn, true},
		{"warn, a user list", newPeople(t).lookup, config.SSHAuthWarn, false},
		{"enforce", userstest.Implicit(), config.SSHAuthEnforce, false},
	} {
		s := &Server{keyAuth: KeyAuth{Mode: tc.mode, Users: tc.lookup}}
		ctx := context.WithValue(context.Background(), connIdentityKey{}, connIdentity{key: stranger.PublicKey(), fingerprint: gossh.FingerprintSHA256(stranger.PublicKey())})
		p, err := s.principalFor(ctx)
		if got := err == nil && p == backend.ImplicitPrincipal(); got != tc.want {
			t.Errorf("%s: principal %v err %v, want implicit=%v", tc.name, p, err, tc.want)
		}
	}
	if _, err := (&Server{}).principalFor(context.Background()); !errors.Is(err, errNoIdentity) {
		t.Errorf("a context with no key: %v", err)
	}
}

// hostMountedBackend is identityBackend whose "box" carries a host directory:
// a shed alice made while she was an admin.
type hostMountedBackend struct{ *identityBackend }

func (b hostMountedBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	sh, err := b.identityBackend.GetShed(ctx, name)
	if err != nil {
		return nil, err
	}
	sh.ProjectMounts = []config.MountConfig{{Source: "/srv/code", Target: "/home/shed/code"}}
	return sh, nil
}

// TestSSHDemotedUserIsRefusedAHostMountedShed: alice, now a user who is not an
// admin, cannot open a new shell, sftp or forward into her own shed that
// carries a host directory, and is told why (not that the shed is someone
// else's); nothing reaches the backend.
func TestSSHDemotedUserIsRefusedAHostMountedShed(t *testing.T) {
	who := newPeople(t)
	be := &identityBackend{}
	_, addr := identityServer(t, hostMountedBackend{be}, who.lookup)
	const want = "Error: shed 'box' carries host directories or server mounts; only an admin can enter or start it"
	client := dialWith(t, addr, "box", who.alice)
	for _, sub := range []string{"", "sftp"} {
		stderr, status := runOn(t, client, "true", sub)
		if status != 1 || !strings.Contains(stderr, want) {
			t.Errorf("alice session (subsystem %q): status %d stderr %q, want 1 %q", sub, status, stderr, want)
		}
	}
	if conn, err := client.Dial("tcp", "localhost:8080"); err == nil {
		_ = conn.Close()
		t.Error("alice's forward into her host-mounted shed was allowed")
	} else if !strings.Contains(err.Error(), "carries host directories") {
		t.Errorf("alice's forward refused with %v, want the host-resource reason", err)
	}
	if execs, dials := be.seen(); len(execs) != 0 || len(dials) != 0 {
		t.Errorf("the backend was reached: execs %v dials %v", execs, dials)
	}
}
