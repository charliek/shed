package sshd

import (
	"path/filepath"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// TestReservedUserRefusedAtTheDoor: the username git is refused in every SSH
// mode, with and without a user list, even for a key that is on the list (and
// so would be accepted as any other user); another username is unchanged.
func TestReservedUserRefusedAtTheDoor(t *testing.T) {
	listed := testSigner(t)
	stranger := testSigner(t)
	fp := gossh.FingerprintSHA256(listed.PublicKey())

	implicit := func() users.Lookup { return userstest.Implicit(fp) }
	userList := func() users.Lookup {
		f := userstest.New()
		f.AddUser(users.User{Name: "alice"})
		f.AddKey("alice", users.Key{Fingerprint: fp, Source: users.SourceInline})
		return f
	}

	for _, tc := range []struct {
		name string
		mode string
		look func() users.Lookup
	}{
		{"zero value, no lookup", "", func() users.Lookup { return nil }},
		{"off", config.SSHAuthOff, implicit},
		{"warn", config.SSHAuthWarn, implicit},
		{"enforce", config.SSHAuthEnforce, implicit},
		{"off with a user list", config.SSHAuthOff, userList},
		{"warn with a user list", config.SSHAuthWarn, userList},
		{"enforce with a user list", config.SSHAuthEnforce, userList},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
				KeyAuth{Mode: tc.mode, Users: tc.look()})
			if err != nil {
				t.Fatal(err)
			}
			addr := serveForTest(t, s)
			if dialUser(t, addr, config.ReservedShedName, listed) {
				t.Error("the listed key was admitted as git")
			}
			if dialUser(t, addr, config.ReservedShedName, stranger) {
				t.Error("an unlisted key was admitted as git")
			}
			// Negative control: the same key as another username is admitted
			// wherever it was before (every row here lists it).
			if tc.look() != nil && !dialUser(t, addr, "gitx", listed) {
				t.Error("the listed key was refused as gitx")
			}
		})
	}
}

// TestReservedUserNeverTriggersARefetch: a git login in enforce mode must not
// make the lookup fetch GitHub keys again (it is refused before any lookup),
// while the same unknown key as an ordinary shed name does.
func TestReservedUserNeverTriggersARefetch(t *testing.T) {
	newcomer := testSigner(t)
	for _, tc := range []struct {
		user        string
		wantAdmit   bool
		wantFetches bool
	}{
		{config.ReservedShedName, false, false},
		{"gitx", true, true},
	} {
		t.Run(tc.user, func(t *testing.T) {
			lookup := &refetchLookup{Fake: userstest.Implicit(), upstream: gossh.FingerprintSHA256(newcomer.PublicKey())}
			be := &countingBackend{identityBackend: &identityBackend{}}
			_, addr := identityServer(t, be, lookup)
			if got := dialUser(t, addr, tc.user, newcomer); got != tc.wantAdmit {
				t.Errorf("admitted = %v, want %v", got, tc.wantAdmit)
			}
			if got := len(lookup.seen()) > 0; got != tc.wantFetches {
				t.Errorf("fetched = %v (%v), want %v", got, lookup.seen(), tc.wantFetches)
			}
			if n := be.gets.Load(); n != 0 {
				t.Errorf("a shed was looked up %d times", n)
			}
		})
	}
}
