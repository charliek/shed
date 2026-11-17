package sshd

import (
	"bytes"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// bootstrapOn runs a control enrolment on client (a connection logged in as
// _bootstrap) and returns the user the issued certificate names, its stderr
// and the run error.
func bootstrapOn(t *testing.T, client *gossh.Client) (user string, bound bool, stderr string, err error) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	if err := sess.Run("control cli csr=" + testCSR(t, elliptic.P256())); err != nil {
		return "", false, errOut.String(), err
	}
	var b bootstrapBundle
	if err := json.Unmarshal(out.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(b.ClientCert))
	if block == nil {
		t.Fatal("no certificate in the bundle")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	user, bound, err = servertls.ClientCertUser(leaf)
	if err != nil {
		t.Fatal(err)
	}
	return user, bound, errOut.String(), nil
}

// echoTimeout bounds one echo through a forward in the binding tests.
const echoTimeout = 10 * time.Second

// TestSSHConnectionKeepsTheUserItAuthenticatedAs: key K is alice's when three
// connections authenticate with it: as box (alice's shed), as crate (bob's
// shed) and as _bootstrap. K then moves to bob with no restart (as a GitHub
// refresh can move it). On every one of those connections a new channel is
// refused, the enrolment included, even on crate, which bob may enter: the
// connections are alice's. The forward box opened before the move keeps
// working. A new connection with K is bob's, and is enrolled as bob.
func TestSSHConnectionKeepsTheUserItAuthenticatedAs(t *testing.T) {
	who := newPeople(t)
	k := who.alice
	be := &identityBackend{}
	s, addr := identityServer(t, twoShedBackend{be}, who.lookup)
	s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TokenTTL: time.Hour, CA: testCA(t)})

	if user, bound, stderr, err := bootstrapOn(t, dialWith(t, addr, reservedBootstrapUser, k)); err != nil || !bound || user != "alice" {
		t.Fatalf("enrolment before the move: user %q bound %v stderr %q err %v, want alice", user, bound, stderr, err)
	}
	box := dialWith(t, addr, "box", k)
	crate := dialWith(t, addr, "crate", k)
	boot := dialWith(t, addr, reservedBootstrapUser, k)
	open, err := box.Dial("tcp", "localhost:8080")
	if err != nil {
		t.Fatalf("forward before the move: %v", err)
	}
	defer func() { _ = open.Close() }()
	// echo sends msg through the forward and reads it back, within
	// echoTimeout: a forwarded channel has no read deadline, so the round
	// trip runs aside and the forward is closed if it does not come back in
	// time, so a regression fails the test instead of hanging it.
	echo := func(msg string) error {
		done := make(chan error, 1)
		go func() {
			if _, err := open.Write([]byte(msg)); err != nil {
				done <- err
				return
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(open, buf); err != nil {
				done <- err
				return
			}
			if string(buf) != msg {
				done <- fmt.Errorf("read %q, want %q", buf, msg)
				return
			}
			done <- nil
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(echoTimeout):
			_ = open.Close()
			return fmt.Errorf("no echo of %q within %s", msg, echoTimeout)
		}
	}
	if err := echo("before"); err != nil {
		t.Fatal(err)
	}
	execsBefore, dialsBefore := be.seen()

	who.lookup.AddKey("bob", users.Key{Fingerprint: gossh.FingerprintSHA256(k.PublicKey()), Source: users.SourceGitHub, Origin: "bob-gh"})

	const refused = "no longer belongs to the user this connection authenticated as"
	for name, c := range map[string]*gossh.Client{"box": box, "crate": crate} {
		for _, sub := range []string{"", "sftp"} {
			if stderr, status := runOn(t, c, "true", sub); status != 1 || !strings.Contains(stderr, refused) {
				t.Errorf("new channel (subsystem %q) on the %s connection after the move: status %d stderr %q, want refused", sub, name, status, stderr)
			}
		}
		if conn, err := c.Dial("tcp", "localhost:8080"); err == nil {
			_ = conn.Close()
			t.Errorf("a new forward on the %s connection was opened after the move", name)
		}
	}
	if user, _, stderr, err := bootstrapOn(t, boot); err == nil || !strings.Contains(stderr, "key not authorized") {
		t.Errorf("enrolment on the connection made before the move: user %q err %v stderr %q, want refused", user, err, stderr)
	}
	if err := echo("after"); err != nil {
		t.Errorf("the forward opened before the move stopped: %v", err)
	}
	if execs, dials := be.seen(); len(execs) != len(execsBefore) || len(dials) != len(dialsBefore) {
		t.Errorf("a channel after the move reached the backend: execs %v dials %v", execs, dials)
	}

	if user, bound, stderr, err := bootstrapOn(t, dialWith(t, addr, reservedBootstrapUser, k)); err != nil || !bound || user != "bob" {
		t.Errorf("enrolment with K after the move: user %q bound %v stderr %q err %v, want bob", user, bound, stderr, err)
	}
	if stderr, status := runOn(t, dialWith(t, addr, "crate", k), "true", ""); status != 0 {
		t.Errorf("a new connection with K to bob's crate: status %d stderr %q, want entered as bob", status, stderr)
	}
	if execs, _ := be.seen(); len(execs) == 0 || execs[len(execs)-1] != backend.UserPrincipal("bob", false) {
		t.Errorf("the new connection reached the backend as %v, want bob", execs)
	}
}

// TestSSHImplicitConnectionIsNotANamedUsers: a connection that authenticated
// as the implicit user opens nothing as a named user once its key becomes
// one (and the enrolment it asks for after names no named user).
func TestSSHImplicitConnectionIsNotANamedUsers(t *testing.T) {
	k := testSigner(t)
	fp := gossh.FingerprintSHA256(k.PublicKey())
	lookup := userstest.Implicit(fp)
	be := &identityBackend{}
	s, addr := identityServer(t, be, lookup)
	s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TokenTTL: time.Hour, CA: testCA(t)})

	box := dialWith(t, addr, "box", k)
	boot := dialWith(t, addr, reservedBootstrapUser, k)
	if stderr, status := runOn(t, box, "true", ""); status != 0 {
		t.Fatalf("the implicit user in box: status %d stderr %q", status, stderr)
	}
	lookup.AddUser(users.User{Name: "alice"})
	lookup.AddKey("alice", users.Key{Fingerprint: fp, Source: users.SourceAdHoc})
	if stderr, status := runOn(t, box, "true", ""); status != 1 || !strings.Contains(stderr, "no longer belongs") {
		t.Errorf("the implicit user's connection after K became alice's: status %d stderr %q, want refused", status, stderr)
	}
	if _, _, stderr, err := bootstrapOn(t, boot); err == nil {
		t.Errorf("the implicit user's connection enrolled after K became alice's (stderr %q)", stderr)
	}
}
