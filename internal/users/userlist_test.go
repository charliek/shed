package users

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

// userListSSH is the auth.ssh block a server with a user list runs with: mtls
// forces enforce, and no key source of auth.ssh's own.
func userListSSH() *config.SSHAuthConfig {
	return &config.SSHAuthConfig{Mode: config.SSHAuthEnforce}
}

// sameExpiry reports whether two key expiries are the same: both none, or
// both the same instant.
func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func openUserList(t *testing.T, list map[string]config.UserConfig) *ConfigLookup {
	t.Helper()
	l, err := NewConfigLookup(ConfigOptions{SSH: userListSSH(), GitHubCacheDir: t.TempDir(), Users: list})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestUserListResolvesEachKeyToItsUser: with auth.users every key resolves to
// the user it is listed under, with that user's admin flag and the key's own
// label, expiry and script flag, whether it is listed by hand or published by
// the user's GitHub login. There is no implicit user, and an unknown key is
// nobody's.
func TestUserListResolvesEachKeyToItsUser(t *testing.T) {
	gh := newFakeGitHub(t)
	aliceKey, aliceLine := genKey(t)
	bobGH, bobGHLine := genKey(t)
	bobCI, bobCILine := genKey(t)
	stranger, _ := genKey(t)
	gh.set("bob-gh", 4242, bobGHLine)
	expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	l := openUserList(t, map[string]config.UserConfig{
		"alice": {Admin: true, Keys: []config.UserKeyConfig{{Key: aliceLine, Label: "laptop", Expires: expires.Format(time.RFC3339)}}},
		"bob":   {GitHub: "bob-gh", Keys: []config.UserKeyConfig{{Key: bobCILine, Label: "ci", Script: true}}},
	})

	for _, tc := range []struct {
		key    gossh.PublicKey
		user   string
		admin  bool
		source Source
		label  string
		script bool
		exp    *time.Time
	}{
		{aliceKey, "alice", true, SourceAdHoc, "laptop", false, &expires},
		{bobGH, "bob", false, SourceGitHub, "", false, nil},
		{bobCI, "bob", false, SourceAdHoc, "ci", true, nil},
	} {
		u, k, ok := l.UserForKey(fp(tc.key))
		if !ok {
			t.Fatalf("%s's %s key does not resolve", tc.user, tc.source)
		}
		if u.Name != tc.user || u.Admin != tc.admin || u.Implicit {
			t.Errorf("key resolves to %+v, want %s admin=%v", u, tc.user, tc.admin)
		}
		if k.Source != tc.source || k.Label != tc.label || k.Script != tc.script || !sameExpiry(k.Expires, tc.exp) {
			t.Errorf("%s key provenance %+v, want source %s label %q script %v expires %v", tc.user, k, tc.source, tc.label, tc.script, tc.exp)
		}
		if tc.source == SourceGitHub && k.Origin != "bob-gh" {
			t.Errorf("github key origin %q", k.Origin)
		}
	}
	if HasKey(l, fp(stranger)) {
		t.Error("an unknown key resolves to a user")
	}
	if u, ok := l.User(ImplicitName); ok {
		t.Errorf("a server with a user list has the implicit user: %+v", u)
	}
	if !HasUserList(l) {
		t.Error("HasUserList is false with a user list")
	}
	var names []string
	for _, u := range l.Users() {
		names = append(names, u.Name)
	}
	if strings.Join(names, ",") != "alice,bob" {
		t.Errorf("Users() = %v", names)
	}
	if u, ok := l.User("bob"); !ok || u.GitHub != "bob-gh" || u.Admin {
		t.Errorf("User(bob) = %+v %v", u, ok)
	}

	// Without a user list: the implicit user, as before.
	_, line := genKey(t)
	implicit, err := NewConfigLookup(ConfigOptions{SSH: &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, AuthorizedKeys: []string{line}}, GitHubCacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if HasUserList(implicit) {
		t.Error("HasUserList is true with no user list")
	}
	if us := implicit.Users(); len(us) != 1 || !us[0].Implicit {
		t.Errorf("Users() with no user list = %+v", us)
	}
}

// TestUserListGitHubKeyOfTwoUsersIsRefusedForBoth: a key the GitHub logins of
// two users both publish belongs to neither, never starts an unknown-key
// fetch, and is the other user's as soon as one stops publishing it.
func TestUserListGitHubKeyOfTwoUsersIsRefusedForBoth(t *testing.T) {
	gh := newFakeGitHub(t)
	shared, sharedLine := genKey(t)
	aliceOwn, aliceLine := genKey(t)
	bobOwn, bobLine := genKey(t)
	gh.set("alice-gh", 1001, aliceLine+sharedLine)
	gh.set("bob-gh", 1002, bobLine+sharedLine)

	l := openUserList(t, map[string]config.UserConfig{
		"alice": {GitHub: "alice-gh", Admin: true},
		"bob":   {GitHub: "bob-gh"},
	})
	if _, _, ok := l.UserForKey(fp(shared)); ok {
		t.Fatal("a key two users' GitHub logins publish resolves to one of them")
	}
	if u, _, _ := l.UserForKey(fp(aliceOwn)); u.Name != "alice" {
		t.Errorf("alice's own key resolves to %q", u.Name)
	}
	if u, _, _ := l.UserForKey(fp(bobOwn)); u.Name != "bob" {
		t.Errorf("bob's own key resolves to %q", u.Name)
	}
	calls := gh.keysCalls.Load() + gh.apiCalls.Load()
	if l.RefetchForUnknownKey(context.Background(), fp(shared)) {
		t.Error("the contested key became known by an unknown-key fetch")
	}
	if n := gh.keysCalls.Load() + gh.apiCalls.Load(); n != calls {
		t.Errorf("the contested key started a GitHub fetch (%d requests)", n-calls)
	}
	report, err := l.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range report.Logins {
		if row.Keys != 1 {
			t.Errorf("%s's row counts %d accepted keys, want 1 (the contested key is not accepted)", row.Login, row.Keys)
		}
	}

	// bob stops publishing it: it is alice's from the next fetch.
	gh.set("bob-gh", 1002, bobLine)
	if _, err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u, _, ok := l.UserForKey(fp(shared)); !ok || u.Name != "alice" {
		t.Errorf("after bob dropped it the key resolves to %q (%v), want alice", u.Name, ok)
	}
}

// TestUserListHandListedKeyStaysItsUsers: a key listed by hand under one user
// stays that user's when another user's GitHub login publishes it too; the
// other user does not get it.
func TestUserListHandListedKeyStaysItsUsers(t *testing.T) {
	gh := newFakeGitHub(t)
	k, line := genKey(t)
	gh.set("bob-gh", 1002, line)
	l := openUserList(t, map[string]config.UserConfig{
		"alice": {Admin: true, Keys: []config.UserKeyConfig{{Key: line, Label: "laptop"}}},
		"bob":   {GitHub: "bob-gh"},
	})
	u, key, ok := l.UserForKey(fp(k))
	if !ok || u.Name != "alice" || key.Source != SourceAdHoc {
		t.Errorf("the key resolves to %q (%v, %s), want alice's hand-listed key", u.Name, ok, key.Source)
	}
}

// TestUserListExpiredKey: a key past its expiry still resolves (so it never
// starts a GitHub fetch) but is not accepted; one whose expiry is ahead is.
func TestUserListExpiredKey(t *testing.T) {
	old, oldLine := genKey(t)
	fresh, freshLine := genKey(t)
	l := openUserList(t, map[string]config.UserConfig{
		"alice": {Admin: true, Keys: []config.UserKeyConfig{
			{Key: oldLine, Label: "old", Expires: "2020-01-01"},
			{Key: freshLine, Label: "fresh", Expires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		}},
	})
	if _, k, ok := l.UserForKey(fp(old)); !ok || !k.Expired(time.Now()) {
		t.Errorf("the expired key: resolves %v expired %v", ok, k.Expired(time.Now()))
	}
	if HasKey(l, fp(old)) {
		t.Error("an expired key is accepted")
	}
	if !HasKey(l, fp(fresh)) {
		t.Error("a key whose expiry is ahead is not accepted")
	}
}

// TestUserListGitHubIDFromConfig: a user's github_id is the account the login
// must name, whatever the server would pin.
func TestUserListGitHubIDFromConfig(t *testing.T) {
	gh := newFakeGitHub(t)
	k, line := genKey(t)
	_, adminLine := genKey(t)
	gh.set("bob-gh", 1002, line)
	list := func(id int64) map[string]config.UserConfig {
		return map[string]config.UserConfig{
			"alice": {Admin: true, Keys: []config.UserKeyConfig{{Key: adminLine, Label: "laptop"}}},
			"bob":   {GitHub: "bob-gh", GitHubID: id},
		}
	}
	if l := openUserList(t, list(9999)); HasKey(l, fp(k)) {
		t.Error("a login that names another account than its configured github_id gave its keys")
	}
	if l := openUserList(t, list(1002)); !HasKey(l, fp(k)) {
		t.Error("a login that names its configured github_id gave no keys")
	}
}

// TestUserListNeedsEnforceAndNoSSHSources: a user list is refused under any
// SSH mode but enforce, and beside auth.ssh's own key sources; a key listed
// under two users is refused. (Config load refuses all three first.)
func TestUserListNeedsEnforceAndNoSSHSources(t *testing.T) {
	_, line := genKey(t)
	one := map[string]config.UserConfig{"alice": {Admin: true, Keys: []config.UserKeyConfig{{Key: line, Label: "l"}}}}
	for _, tc := range []struct {
		name  string
		ssh   *config.SSHAuthConfig
		users map[string]config.UserConfig
	}{
		{"no auth.ssh", nil, one},
		{"off", &config.SSHAuthConfig{Mode: config.SSHAuthOff}, one},
		{"warn", &config.SSHAuthConfig{Mode: config.SSHAuthWarn}, one},
		{"inline keys too", &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, AuthorizedKeys: []string{line}}, one},
		{"keys file too", &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, AuthorizedKeysFile: "/dev/null"}, one},
		{"github_users too", &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, GitHubUsers: []string{"octocat"}}, one},
		{"a key under two users", userListSSH(), map[string]config.UserConfig{
			"alice": {Admin: true, Keys: []config.UserKeyConfig{{Key: line, Label: "l"}}},
			"bob":   {Keys: []config.UserKeyConfig{{Key: line, Label: "l"}}},
		}},
	} {
		if _, err := NewConfigLookup(ConfigOptions{SSH: tc.ssh, GitHubCacheDir: t.TempDir(), Users: tc.users}); err == nil {
			t.Errorf("%s: a user list was accepted", tc.name)
		}
	}
}

// genCert returns an SSH user certificate for key, signed by a fresh CA and
// valid forever, and its authorized_keys line.
func genCert(t *testing.T, key gossh.PublicKey) (gossh.PublicKey, string) {
	t.Helper()
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := gossh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert := &gossh.Certificate{Key: key, CertType: gossh.UserCert, KeyId: "k", ValidPrincipals: []string{"shed"}, ValidBefore: gossh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return cert, string(gossh.MarshalAuthorizedKey(cert))
}

// TestUserListRefusesCertificates: on a server with a user list an SSH
// certificate is nobody's. Listed by hand it fails the load; published by a
// user's GitHub login (GitHub does not take certificates as account keys, but
// the list is not trusted to be what GitHub takes) it resolves to no user,
// whether it holds a key another user lists (an alias of that user, an admin
// here) or a key nobody lists. The plain key keeps resolving to its own user.
// With no user list a certificate is accepted as before, as the implicit
// user's.
func TestUserListRefusesCertificates(t *testing.T) {
	rootKey, rootLine := genKey(t)
	other, _ := genKey(t)
	rootCert, rootCertLine := genCert(t, rootKey)
	otherCert, otherCertLine := genCert(t, other)

	_, err := NewConfigLookup(ConfigOptions{SSH: userListSSH(), GitHubCacheDir: t.TempDir(), Users: map[string]config.UserConfig{
		"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "desk"}}},
		"bob":  {Keys: []config.UserKeyConfig{{Key: rootCertLine, Label: "cert"}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "SSH certificate") {
		t.Errorf("a certificate listed by hand: %v, want a refusal naming the certificate", err)
	}

	gh := newFakeGitHub(t)
	gh.set("bob-gh", 4242, rootCertLine+otherCertLine)
	l := openUserList(t, map[string]config.UserConfig{
		"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "desk"}}},
		"bob":  {GitHub: "bob-gh"},
	})
	if u, _, ok := l.UserForKey(fp(rootKey)); !ok || u.Name != "root" {
		t.Errorf("root's plain key resolves to %q (%v), want root", u.Name, ok)
	}
	// Both certificates reached the key set (so what refuses them below is
	// the lookup, not the fetch).
	l.mu.RLock()
	_, rootCertIn := l.keys[fp(rootCert)]
	_, otherCertIn := l.keys[fp(otherCert)]
	l.mu.RUnlock()
	if !rootCertIn || !otherCertIn {
		t.Fatalf("the certificates bob's login publishes are not in the key set (%v, %v)", rootCertIn, otherCertIn)
	}
	for name, cert := range map[string]gossh.PublicKey{"a certificate of root's key": rootCert, "a certificate of an unlisted key": otherCert} {
		if u, _, ok := l.UserForKey(fp(cert)); ok {
			t.Errorf("%s from bob's GitHub login resolves to %q (admin %v)", name, u.Name, u.Admin)
		}
		if HasKey(l, fp(cert)) {
			t.Errorf("%s is accepted", name)
		}
	}

	implicit, err := NewConfigLookup(ConfigOptions{
		SSH:            &config.SSHAuthConfig{Mode: config.SSHAuthEnforce, AuthorizedKeys: []string{strings.TrimSpace(otherCertLine)}},
		GitHubCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if u, _, ok := implicit.UserForKey(fp(otherCert)); !ok || !u.Implicit {
		t.Errorf("with no user list a listed certificate resolves to %+v (%v), want the implicit user as before", u, ok)
	}
}

// TestUserListZeroTimeExpiry: an expiry of the earliest time Go holds (the
// zero time) is an expiry long past, never "no expiry".
func TestUserListZeroTimeExpiry(t *testing.T) {
	k, line := genKey(t)
	_, rootLine := genKey(t)
	l := openUserList(t, map[string]config.UserConfig{
		"root":  {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "desk"}}},
		"alice": {Keys: []config.UserKeyConfig{{Key: line, Label: "old", Expires: "0001-01-01T00:00:00Z"}}},
	})
	_, key, ok := l.UserForKey(fp(k))
	if !ok || key.Expires == nil || !key.Expired(time.Now()) {
		t.Errorf("the key: resolves %v expires %v expired %v, want resolved and expired", ok, key.Expires, key.Expired(time.Now()))
	}
	if HasKey(l, fp(k)) {
		t.Error("a key that expired at the zero time is accepted")
	}
}

// TestAdminCanAct is the start-up warning's check: some admin has a GitHub
// login, or a key that is not marked script and has not expired.
func TestAdminCanAct(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	key := func(expires string, script bool) config.UserKeyConfig {
		return config.UserKeyConfig{Key: "k", Label: "l", Expires: expires, Script: config.StrictBool(script)}
	}
	for _, tc := range []struct {
		name string
		list map[string]config.UserConfig
		want bool
	}{
		{"an admin with a login", map[string]config.UserConfig{"root": {Admin: true, GitHub: "root-gh"}}, true},
		{"an admin with a key that never expires", map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{key("", false)}}}, true},
		{"an admin with a key that expires later", map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{key("2027-01-01", false)}}}, true},
		{"every admin key expired", map[string]config.UserConfig{
			"root": {Admin: true, Keys: []config.UserKeyConfig{key("2020-01-01", false), key("0001-01-01T00:00:00Z", false)}},
			"bob":  {GitHub: "bob-gh", Keys: []config.UserKeyConfig{key("", false)}},
		}, false},
		{"the admin's live key is a script key", map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{key("2020-01-01", false), key("", true)}}}, false},
	} {
		if got := adminCanAct(tc.list, now); got != tc.want {
			t.Errorf("%s: adminCanAct = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBindingDoesNotFollowAKeyMovedByARefresh: key K is alice's (her GitHub
// login publishes it) when a credential is bound to her. A refresh, with no
// restart, then finds K under bob's login only: K resolves to bob, and the
// credential bound to alice resolves to nobody (ResolveBound), while one bound
// to bob, enrolled after, resolves to bob. A credential bound to the implicit
// user never resolves to a named user, and an unbound one (issued before
// bindings existed) follows the key.
func TestBindingDoesNotFollowAKeyMovedByARefresh(t *testing.T) {
	gh := newFakeGitHub(t)
	k, kLine := genKey(t)
	bobOwn, bobLine := genKey(t)
	gh.set("alice-gh", 1001, kLine)
	gh.set("bob-gh", 1002, bobLine)
	l := openUserList(t, map[string]config.UserConfig{
		"alice": {GitHub: "alice-gh", Admin: true},
		"bob":   {GitHub: "bob-gh"},
	})
	alice, ok := l.User("alice")
	if !ok {
		t.Fatal("no alice")
	}
	bound := BindingOf(alice)
	now := time.Now()
	if u, _, ok := ResolveBound(l, fp(k), bound, now); !ok || u.Name != "alice" || !u.Admin {
		t.Fatalf("before the refresh K bound to alice resolves to %+v %v", u, ok)
	}

	gh.set("alice-gh", 1001, "")
	gh.set("bob-gh", 1002, bobLine+kLine)
	if _, err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u, _, ok := l.UserForKey(fp(k)); !ok || u.Name != "bob" {
		t.Fatalf("after the refresh K resolves to %q %v, want bob", u.Name, ok)
	}
	if u, _, ok := ResolveBound(l, fp(k), bound, now); ok {
		t.Errorf("a credential bound to alice resolves to %+v after K moved to bob", u)
	}
	bob, _ := l.User("bob")
	if u, _, ok := ResolveBound(l, fp(k), BindingOf(bob), now); !ok || u.Name != "bob" {
		t.Errorf("a credential bound to bob resolves to %+v %v", u, ok)
	}
	if _, _, ok := ResolveBound(l, fp(k), BindingOf(Implicit()), now); ok {
		t.Error("a credential bound to the implicit user resolves to a named user")
	}
	if u, _, ok := ResolveBound(l, fp(k), Binding{}, now); !ok || u.Name != "bob" {
		t.Errorf("an unbound credential resolves to %+v %v, want K's user now (bob)", u, ok)
	}
	if u, _, ok := ResolveBound(l, fp(bobOwn), BindingOf(bob), now); !ok || u.Name != "bob" {
		t.Errorf("bob's own key: %+v %v", u, ok)
	}
}
