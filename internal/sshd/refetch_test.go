package sshd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// refetchLookup is a user lookup that can look for unknown keys: a fetch
// makes the key in upstream known, as a GitHub login that lists it would.
// It records every ask.
type refetchLookup struct {
	*userstest.Fake
	mu       sync.Mutex
	upstream string // fingerprint a fetch finds
	asks     []string
}

var _ users.UnknownKeyRefetcher = (*refetchLookup)(nil)

func (r *refetchLookup) RefetchForUnknownKey(_ context.Context, fp string) bool {
	r.mu.Lock()
	r.asks = append(r.asks, fp)
	up := r.upstream
	r.mu.Unlock()
	if fp != up {
		return false
	}
	r.AddKey(users.ImplicitName, users.Key{Fingerprint: fp, Source: users.SourceGitHub, Origin: "octocat"})
	return true
}

func (r *refetchLookup) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asks...)
}

// countingBackend counts every shed lookup.
type countingBackend struct {
	*identityBackend
	gets atomic.Int64
}

func (b *countingBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	b.gets.Add(1)
	return b.identityBackend.GetShed(ctx, name)
}

// dialUser completes a handshake as user and reports whether it was admitted.
func dialUser(t *testing.T, addr, user string, signer gossh.Signer) bool {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return false
	}
	_ = client.Close()
	return true
}

// TestUnknownKeyRefetchAtTheDoor: on a lookup that can fetch and not nudge
// (GitHub logins: the rule there is unchanged), an unknown key logging in as
// _bootstrap or as any valid shed name asks the lookup to fetch again, and is
// admitted on that same attempt when the fetch finds it (whether the shed
// exists is decided per channel, after authentication). A name that is not a
// valid shed name never asks, and a key the lookup knows never asks: an
// expired one is refused at the door without asking. Deciding never looks a
// shed up: nothing about sheds is read for a peer that has not authenticated.
// (A lookup that can nudge, a directory's list, is unknownshed_test.go's.)
func TestUnknownKeyRefetchAtTheDoor(t *testing.T) {
	newcomer := testSigner(t)
	stranger := testSigner(t)
	expired := testSigner(t)

	for _, tc := range []struct {
		user         string
		signer       gossh.Signer
		wantAdmitted bool
		wantAsk      bool
	}{
		{"box", newcomer, true, true},
		{reservedBootstrapUser, newcomer, true, true},
		{"no-such-shed", newcomer, true, true},
		{"../../etc", newcomer, false, false},
		{"Not_A_Shed", newcomer, false, false},
		{"box", stranger, false, true},
		{"box", expired, false, false}, // known but expired: refused here, and never asks
	} {
		t.Run(tc.user, func(t *testing.T) {
			lookup := &refetchLookup{Fake: userstest.Implicit(), upstream: gossh.FingerprintSHA256(newcomer.PublicKey())}
			hourAgo := time.Now().Add(-time.Hour)
			lookup.AddKey(users.ImplicitName, users.Key{
				Fingerprint: gossh.FingerprintSHA256(expired.PublicKey()),
				Source:      users.SourceInline,
				Expires:     &hourAgo,
			})
			be := &countingBackend{identityBackend: &identityBackend{}}
			_, addr := identityServer(t, be, lookup)
			if got := dialUser(t, addr, tc.user, tc.signer); got != tc.wantAdmitted {
				t.Errorf("admitted = %v, want %v", got, tc.wantAdmitted)
			}
			if got := len(lookup.seen()) > 0; got != tc.wantAsk {
				t.Errorf("asked the lookup to fetch = %v (%v), want %v", got, lookup.seen(), tc.wantAsk)
			}
			if n := be.gets.Load(); n != 0 {
				t.Errorf("the handshake looked a shed up %d times", n)
			}
		})
	}
}

// liveRefetchLookup is a refetchLookup whose users change while the server
// runs (users.BoundCredentialsOnly), as a directory's list is.
type liveRefetchLookup struct{ *refetchLookup }

var _ users.BoundCredentialsOnly = liveRefetchLookup{}

func (liveRefetchLookup) BoundCredentialsOnly() {}

// TestUnknownKeyRefetchLogLine: the line logged when a fetch makes an unknown
// key known names what was fetched. A lookup whose users change while the
// server runs (a directory's list) says it pulled the directory and never
// names GitHub; any other prints, byte for byte, the line it always has.
func TestUnknownKeyRefetchLogLine(t *testing.T) {
	const github = "SSH auth: key %s remote=127.0.0.1:PORT is known after fetching the GitHub logins again"
	const directory = "SSH auth: key %s remote=127.0.0.1:PORT is known after pulling the directory again"
	for _, tc := range []struct {
		name      string
		live      bool
		want, not string
	}{
		{"GitHub logins", false, github, directory},
		{"a directory's list", true, directory, github},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newcomer := testSigner(t)
			fp := gossh.FingerprintSHA256(newcomer.PublicKey())
			base := &refetchLookup{Fake: userstest.Implicit(), upstream: fp}
			var lookup users.Lookup = base
			if tc.live {
				lookup = liveRefetchLookup{base}
			}
			_, addr := identityServer(t, &identityBackend{}, lookup)
			out := captureLog(t)
			if !dialUser(t, addr, "box", newcomer) {
				t.Fatal("the key the fetch found was refused")
			}
			lines := strings.Split(anyPort(out.String()), "\n")
			if !slices.Contains(lines, fmt.Sprintf(tc.want, fp)) {
				t.Errorf("no line %q in the log:\n%s", fmt.Sprintf(tc.want, fp), out)
			}
			if slices.Contains(lines, fmt.Sprintf(tc.not, fp)) || (tc.live && strings.Contains(out.String(), "GitHub")) {
				t.Errorf("the other lookup's line in the log:\n%s", out)
			}
		})
	}
}

// TestUnknownKeyRefetchOnlyInEnforce: in warn mode an unknown key is let in
// without a fetch (it is let in anyway); nothing is asked.
func TestUnknownKeyRefetchOnlyInEnforce(t *testing.T) {
	newcomer := testSigner(t)
	lookup := &refetchLookup{Fake: userstest.Implicit(), upstream: gossh.FingerprintSHA256(newcomer.PublicKey())}
	s, err := NewServer(&identityBackend{}, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: config.SSHAuthWarn, Users: lookup})
	if err != nil {
		t.Fatal(err)
	}
	addr := serveForTest(t, s)
	if !dialUser(t, addr, "box", newcomer) {
		t.Error("warn mode refused a key")
	}
	if asks := lookup.seen(); len(asks) != 0 {
		t.Errorf("warn mode asked the lookup to fetch: %v", asks)
	}
}

// TestUnknownKeyRefetchNeedsALookupThatFetches: a lookup that cannot fetch
// (the plain double) refuses an unknown key as before.
func TestUnknownKeyRefetchNeedsALookupThatFetches(t *testing.T) {
	_, addr := identityServer(t, &identityBackend{}, userstest.Implicit())
	if dialUser(t, addr, "box", testSigner(t)) {
		t.Error("an unknown key was admitted with no lookup that fetches")
	}
}
