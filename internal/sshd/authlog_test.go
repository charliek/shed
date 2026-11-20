package sshd

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// fakeRemote is the peer address fakeConnMeta and fakeSSHContext report. Its
// port is below 1024, so no test client's kernel-chosen port is ever the same
// (ownAuthLines).
const fakeRemote = "127.0.0.1:999"

// authCtx is the connection context handlePublicKey reads for a request as
// user: the request's live metadata, as the BannerCallback records it.
func authCtx(t *testing.T, user string) *fakeSSHContext {
	ctx := newFakeSSHContext(t, user)
	ctx.SetValue(authConnKey{}, fakeConnMeta{user: user})
	return ctx
}

// TestSSHAuthLineShapes pins every SSH auth log line, whole: each one that
// names a key carries remote=<host:port> right after the fingerprint, the
// attempt-cap line (which names no key) right after user=, and the rest of
// each line is as it was. An offered key that is accepted logs nothing; the
// one "SSH auth allowed" line is written when the key is proven (the
// VerifiedPublicKeyCallback), in every mode.
func TestSSHAuthLineShapes(t *testing.T) {
	alice, expired, stranger, ca := testSigner(t), testSigner(t), testSigner(t), testSigner(t)
	script, desk := testSigner(t), testSigner(t)
	fp := func(k gossh.PublicKey) string { return gossh.FingerprintSHA256(k) }
	aliceFP, expiredFP, strangerFP := fp(alice.PublicKey()), fp(expired.PublicKey()), fp(stranger.PublicKey())
	scriptFP, deskFP := fp(script.PublicKey()), fp(desk.PublicKey())

	cert := &gossh.Certificate{Key: stranger.PublicKey(), CertType: gossh.UserCert, ValidPrincipals: []string{"box"}, ValidBefore: gossh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}

	// listed is a server with a user list: alice, with her laptop key and an
	// expired one, and root, an admin, with an unlabelled script key.
	listed := func() KeyAuth {
		l := userstest.New()
		l.AddUser(users.User{Name: "alice"})
		l.AddKey("alice", users.Key{Fingerprint: aliceFP, Source: users.SourceInline, Label: "laptop"})
		hourAgo := time.Now().Add(-time.Hour)
		l.AddKey("alice", users.Key{Fingerprint: expiredFP, Source: users.SourceInline, Label: "old", Expires: &hourAgo})
		l.AddUser(users.User{Name: "root", Admin: true})
		l.AddKey("root", users.Key{Fingerprint: scriptFP, Source: users.SourceInline, Script: true})
		return KeyAuth{Mode: config.SSHAuthEnforce, Users: l}
	}
	// implicit is a server with no user list whose one key has a label.
	implicit := func() KeyAuth {
		l := userstest.Implicit()
		l.AddKey(users.ImplicitName, users.Key{Fingerprint: deskFP, Source: users.SourceInline, Label: "desk"})
		return KeyAuth{Mode: config.SSHAuthEnforce, Users: l}
	}
	off := func() KeyAuth { return KeyAuth{} }
	warn := func() KeyAuth { return KeyAuth{Mode: config.SSHAuthWarn, Users: userstest.Implicit()} }
	fetching := func(live bool) func() KeyAuth {
		return func() KeyAuth {
			r := &refetchLookup{Fake: userstest.Implicit(), upstream: strangerFP}
			if live {
				return KeyAuth{Mode: config.SSHAuthEnforce, Users: liveRefetchLookup{r}}
			}
			return KeyAuth{Mode: config.SSHAuthEnforce, Users: r}
		}
	}
	tracking := func() KeyAuth { a := listed(); a.TrackConnections = true; return a }

	// offer is a key asked about as user (handlePublicKey).
	offer := func(user string, key gossh.PublicKey) func(*testing.T, *Server) bool {
		return func(t *testing.T, s *Server) bool { return s.handlePublicKey(authCtx(t, user), key) }
	}
	// prove is a signature by key for user that x/crypto has verified.
	prove := func(user string, key gossh.PublicKey) func(*testing.T, *Server) bool {
		return func(t *testing.T, s *Server) bool {
			cfg := s.sshServer.ServerConfigCallback(newFakeSSHContext(t, user))
			_, err := cfg.VerifiedPublicKeyCallback(fakeConnMeta{user: user}, key, &gossh.Permissions{}, gossh.KeyAlgoED25519)
			return err == nil
		}
	}
	// exhaust fails as many public-key attempts as the cap allows.
	exhaust := func(user string) func(*testing.T, *Server) bool {
		return func(t *testing.T, s *Server) bool {
			cfg := s.sshServer.ServerConfigCallback(newFakeSSHContext(t, user))
			for range cfg.MaxAuthTries {
				cfg.AuthLogCallback(fakeConnMeta{user: user}, "publickey", errors.New("refused"))
			}
			return true
		}
	}

	for _, tc := range []struct {
		name string
		auth func() KeyAuth
		act  func(*testing.T, *Server) bool
		ok   bool
		want string // the whole log; "" for none
	}{
		// At the offer.
		{"offer: accepted, enforce", listed, offer("box", alice.PublicKey()), true, ""},
		{"offer: reserved username", listed, offer("git", alice.PublicKey()), false,
			fmt.Sprintf("SSH auth DENIED: user=git fingerprint=%s remote=%s (reserved username)", aliceFP, fakeRemote)},
		{"offer: an SSH certificate", listed, offer("box", cert), false,
			fmt.Sprintf("SSH auth DENIED: user=box fingerprint=%s remote=%s (an SSH certificate; this server's users log in with plain keys)", fp(cert), fakeRemote)},
		{"offer: an expired key", listed, offer("box", expired.PublicKey()), false,
			fmt.Sprintf(`SSH auth DENIED: user=box fingerprint=%s remote=%s (the key has expired) key_user="alice" admin=false label="old"`, expiredFP, fakeRemote)},
		{"offer: an unknown key", listed, offer("box", stranger.PublicKey()), false,
			fmt.Sprintf("SSH auth DENIED: user=box fingerprint=%s remote=%s", strangerFP, fakeRemote)},
		{"offer: off mode", off, offer("box", stranger.PublicKey()), true,
			fmt.Sprintf("SSH auth attempt: user=box fingerprint=%s remote=%s (allowlist off)", strangerFP, fakeRemote)},
		{"offer: warn mode", warn, offer("box", stranger.PublicKey()), true,
			fmt.Sprintf("SSH auth WOULD DENY (warn mode): user=box fingerprint=%s remote=%s", strangerFP, fakeRemote)},
		{"offer: known after fetching GitHub logins", fetching(false), offer("box", stranger.PublicKey()), true,
			fmt.Sprintf("SSH auth: key %s remote=%s is known after fetching the GitHub logins again", strangerFP, fakeRemote)},
		{"offer: known after pulling the directory", fetching(true), offer("box", stranger.PublicKey()), true,
			fmt.Sprintf("SSH auth: key %s remote=%s is known after pulling the directory again", strangerFP, fakeRemote)},

		// When the key is proven.
		{"proven: allowed", listed, prove("box", alice.PublicKey()), true,
			fmt.Sprintf(`SSH auth allowed: user=box fingerprint=%s remote=%s key_user="alice" admin=false label="laptop"`, aliceFP, fakeRemote)},
		{"proven: allowed, a script key with no label", listed, prove("box", script.PublicKey()), true,
			fmt.Sprintf(`SSH auth allowed: user=box fingerprint=%s remote=%s key_user="root" admin=true`, scriptFP, fakeRemote)},
		{"proven: allowed, the implicit user's labelled key", implicit, prove("box", desk.PublicKey()), true,
			fmt.Sprintf(`SSH auth allowed: user=box fingerprint=%s remote=%s label="desk"`, deskFP, fakeRemote)},
		{"proven: allowed, off mode", off, prove("box", stranger.PublicKey()), true,
			fmt.Sprintf("SSH auth allowed: user=box fingerprint=%s remote=%s", strangerFP, fakeRemote)},
		{"proven: allowed, warn mode", warn, prove("box", stranger.PublicKey()), true,
			fmt.Sprintf("SSH auth allowed: user=box fingerprint=%s remote=%s", strangerFP, fakeRemote)},
		{"proven: reserved username", listed, prove("git", alice.PublicKey()), false,
			fmt.Sprintf("SSH auth DENIED: user=git fingerprint=%s remote=%s (reserved username)", aliceFP, fakeRemote)},
		{"proven: a key that resolves to nobody now", listed, prove("box", stranger.PublicKey()), false,
			fmt.Sprintf("SSH auth DENIED: user=box fingerprint=%s remote=%s (key %s is no longer authorized)", strangerFP, fakeRemote, strangerFP)},
		{"proven: not tracked", tracking, prove("box", alice.PublicKey()), false,
			fmt.Sprintf("SSH auth DENIED: user=box fingerprint=%s remote=%s (%v)", aliceFP, fakeRemote, errUntracked)},

		// The attempt cap, which names no key.
		{"attempt cap", listed, exhaust("box"), true,
			fmt.Sprintf("SSH auth: public-key attempt cap (%d) reached for user=box remote=%s — the client may be offering more keys than the cap allows, so its allowlisted key may never be tried. Raise auth.ssh.max_auth_tries, or use a per-host IdentityFile + IdentitiesOnly on the client.", defaultMaxAuthTries, fakeRemote)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, tc.auth())
			if err != nil {
				t.Fatal(err)
			}
			out := captureLog(t)
			if got := tc.act(t, s); got != tc.ok {
				t.Errorf("accepted = %v, want %v", got, tc.ok)
			}
			want := tc.want
			if want != "" {
				want += "\n"
			}
			if got := ownAuthLines(out.String()); got != want {
				t.Errorf("log:\n got %q\nwant %q\nwhole log: %q", got, want, out.String())
			}
		})
	}
}

// remoteField is the remote=<host:port> field of an auth log line.
var remoteField = regexp.MustCompile(`remote=(\S+)`)

// ownAuthLines is the part of a captured log that a call with the fake
// connection metadata can have written: every "SSH auth" line that names no
// peer other than fakeRemote (a line with no remote= at all is kept, so a
// line that lost its address still fails the comparison). The process log is
// global, so it drops what can only come from another test's server: a line
// that is not an auth line (an unjoined "SSH: closed ..." writer), and an auth
// line that names another peer (a real connection's kernel-chosen port, which
// is never fakeRemote's port, below 1024).
func ownAuthLines(text string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if !strings.HasPrefix(line, "SSH auth") {
			continue
		}
		foreign := false
		for _, m := range remoteField.FindAllStringSubmatch(line, -1) {
			foreign = foreign || m[1] != fakeRemote
		}
		if !foreign {
			out.WriteString(line)
		}
	}
	return out.String()
}

// TestOwnAuthLines: the filter keeps this connection's auth lines, and an
// auth line with no address, whole and in order, and drops only another
// connection's auth lines and lines that are not auth lines.
func TestOwnAuthLines(t *testing.T) {
	ours := "SSH auth DENIED: user=box fingerprint=SHA256:a remote=" + fakeRemote + "\n"
	noAddress := "SSH auth allowed: user=box fingerprint=SHA256:b\n"
	in := ours +
		"SSH: closed 1 connection(s) of user \"bob\": no longer a user of this server\n" +
		"SSH auth DENIED: user=box fingerprint=SHA256:c remote=127.0.0.1:40000\n" +
		noAddress +
		"SSH session ended: user=box remote=127.0.0.1:41234\n"
	if got, want := ownAuthLines(in), ours+noAddress; got != want {
		t.Errorf("ownAuthLines:\n got %q\nwant %q", got, want)
	}
	if got := ownAuthLines(""); got != "" {
		t.Errorf("ownAuthLines(\"\") = %q", got)
	}
}

// switchingLookup answers its first UserForKey from before and every later one
// from the embedded Fake: the user list changing right after the verified
// callback bound the connection, before anything else asks it.
type switchingLookup struct {
	*userstest.Fake
	before *userstest.Fake
	calls  atomic.Int32
}

func (l *switchingLookup) UserForKey(fp string) (users.User, users.Key, bool) {
	if l.calls.Add(1) == 1 {
		return l.before.UserForKey(fp)
	}
	return l.Fake.UserForKey(fp)
}

// TestSSHAuthAllowedNamesTheBoundUser: the "SSH auth allowed" line describes
// the user the connection was bound to, from the lookup that bound it, even
// when the list changes before the line is written: a key moved to another
// user still names its first user, and a key dropped from the list still
// names its user.
func TestSSHAuthAllowedNamesTheBoundUser(t *testing.T) {
	key := testSigner(t).PublicKey()
	kfp := gossh.FingerprintSHA256(key)
	before := userstest.New()
	before.AddUser(users.User{Name: "alice"})
	before.AddKey("alice", users.Key{Fingerprint: kfp, Source: users.SourceInline, Label: "laptop"})

	moved := userstest.New()
	moved.AddUser(users.User{Name: "alice"})
	moved.AddUser(users.User{Name: "bob", Admin: true})
	moved.AddKey("bob", users.Key{Fingerprint: kfp, Source: users.SourceInline, Label: "stolen"})
	dropped := userstest.New()
	dropped.AddUser(users.User{Name: "alice"})

	want := fmt.Sprintf(`SSH auth allowed: user=box fingerprint=%s remote=%s key_user="alice" admin=false label="laptop"`+"\n", kfp, fakeRemote)
	for name, after := range map[string]*userstest.Fake{"moved to bob": moved, "dropped": dropped} {
		t.Run(name, func(t *testing.T) {
			lookup := &switchingLookup{Fake: after, before: before}
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, KeyAuth{Mode: config.SSHAuthEnforce, Users: lookup})
			if err != nil {
				t.Fatal(err)
			}
			out := captureLog(t)
			cfg := s.sshServer.ServerConfigCallback(newFakeSSHContext(t, "box"))
			if _, err := cfg.VerifiedPublicKeyCallback(fakeConnMeta{user: "box"}, key, &gossh.Permissions{}, gossh.KeyAlgoED25519); err != nil {
				t.Fatalf("the verified key was refused: %v", err)
			}
			if got := ownAuthLines(out.String()); got != want {
				t.Errorf("log:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestLogRemote: the peer address as a line prints it, and "-" when there is
// none on record.
func TestLogRemote(t *testing.T) {
	if got := logRemote(fakeConnMeta{}.RemoteAddr()); got != fakeRemote {
		t.Errorf("logRemote = %q, want %q", got, fakeRemote)
	}
	if got := logRemote(nil); got != "-" {
		t.Errorf("logRemote(nil) = %q, want -", got)
	}
}
