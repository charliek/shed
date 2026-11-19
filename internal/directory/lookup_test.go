package directory

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	gossh "golang.org/x/crypto/ssh"
)

// newTestKey is a fresh Ed25519 key: its authorized_keys line and its
// fingerprint.
func newTestKey(t *testing.T) (line, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pk))), gossh.FingerprintSHA256(pk)
}

// fpOf is the fingerprint of a key line.
func fpOf(t *testing.T, line string) string {
	t.Helper()
	pk, err := config.ParseUserKey(line)
	if err != nil {
		t.Fatal(err)
	}
	return gossh.FingerprintSHA256(pk)
}

// listKey is one element of an entry's keys, as JSON. expires "" is absent.
func listKey(line, label, expires string) string {
	s := `{"key":` + strconv.Quote(line) + `,"label":` + strconv.Quote(label)
	if expires != "" {
		s += `,"expires":` + strconv.Quote(expires)
	}
	return s + "}"
}

// listEntry is one user entry, as JSON.
func listEntry(name string, admin bool, keys ...string) string {
	return `{"name":` + strconv.Quote(name) + `,"admin":` + strconv.FormatBool(admin) + `,"keys":[` + strings.Join(keys, ",") + `]}`
}

// decoded is the List DecodeList makes of these entries.
func decoded(t *testing.T, entries ...string) *List {
	t.Helper()
	l, err := DecodeList(listBody(entries...), worldOrg)
	if err != nil {
		t.Fatalf("DecodeList: %v", err)
	}
	return l
}

// localAdmin is a local admin's config with these keys (label k0, k1, ...).
func localAdmin(lines ...string) config.UserConfig {
	uc := config.UserConfig{Admin: true}
	for i, line := range lines {
		uc.Keys = append(uc.Keys, config.UserKeyConfig{Key: line, Label: "k" + strconv.Itoa(i)})
	}
	return uc
}

// mustLookup is a lookup of these local admins holding list (nil: none) from
// the start, fixed: a list given so has no trust time (newLookup). A test of
// the trust time installs its lists (Install).
func mustLookup(t *testing.T, opts LookupOptions, list *List) *Lookup {
	t.Helper()
	l, err := newLookupOrNot(opts, list)
	if err != nil {
		t.Fatalf("newLookup: %v", err)
	}
	return l
}

// newLookupOrNot builds a lookup through the exported constructor when there
// is no list, and through newLookup, its fixed-list form, when there is one.
func newLookupOrNot(opts LookupOptions, list *List) (*Lookup, error) {
	if list == nil {
		return NewLookup(opts)
	}
	return newLookup(opts, list)
}

func names(us []users.User) string {
	var out []string
	for _, u := range us {
		out = append(out, u.Name)
	}
	return strings.Join(out, " ")
}

func skips(s *snapshot) string {
	var out []string
	for _, sk := range s.skipped {
		out = append(out, sk.Name+"="+string(sk.Reason))
	}
	return strings.Join(out, " ")
}

// TestLookupReaders checks every reader against one snapshot of local admins
// and a list: whose each key is, with its source, label and expiry; each
// user; the sorted union; no implicit user; enforce mode and the configured
// attempt cap.
func TestLookupReaders(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	l := mustLookup(t, LookupOptions{
		LocalAdmins:  map[string]config.UserConfig{"root": localAdmin(rootLine)},
		MaxAuthTries: 4,
	}, decoded(t,
		listEntry("carol", false, listKey(keyCarol, "phone", "2099-01-01T00:00:00Z")),
		listEntry("ada", true, listKey(keyAda, "laptop", "")),
		listEntry("bob", false, listKey(keyBob, "desk", "")),
	))

	expiry := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		fp      string
		user    users.User
		source  users.Source
		label   string
		expires *time.Time
	}{
		{rootFP, users.User{Name: "root", Admin: true}, users.SourceAdHoc, "k0", nil},
		{fpOf(t, keyAda), users.User{Name: "ada", Admin: true}, users.SourceDirectory, "laptop", nil},
		{fpOf(t, keyBob), users.User{Name: "bob"}, users.SourceDirectory, "desk", nil},
		{fpOf(t, keyCarol), users.User{Name: "carol"}, users.SourceDirectory, "phone", &expiry},
	} {
		u, k, ok := l.UserForKey(tc.fp)
		if !ok || u != tc.user {
			t.Errorf("UserForKey(%s) = %+v, %v; want %+v", tc.fp, u, ok, tc.user)
			continue
		}
		if k.Fingerprint != tc.fp || k.Source != tc.source || k.Label != tc.label || k.Script || k.Origin != "" {
			t.Errorf("key of %s = %+v; want source %s label %q, not script", tc.user.Name, k, tc.source, tc.label)
		}
		if (k.Expires == nil) != (tc.expires == nil) || (k.Expires != nil && !k.Expires.Equal(*tc.expires)) {
			t.Errorf("key of %s expires %v, want %v", tc.user.Name, k.Expires, tc.expires)
		}
		if !users.HasKey(l, tc.fp) {
			t.Errorf("HasKey(%s) false", tc.user.Name)
		}
		if _, _, ok := users.ResolveBound(l, tc.fp, users.BindingOf(tc.user), time.Now()); !ok {
			t.Errorf("ResolveBound refuses %s's own key", tc.user.Name)
		}
		if got, ok := l.User(tc.user.Name); !ok || got != tc.user {
			t.Errorf("User(%q) = %+v, %v", tc.user.Name, got, ok)
		}
	}

	_, unknownFP := newTestKey(t)
	for _, fp := range []string{unknownFP, "", strings.ToLower(rootFP), "MD5:" + rootFP} {
		if _, _, ok := l.UserForKey(fp); ok {
			t.Errorf("UserForKey(%q) resolved", fp)
		}
	}
	if _, _, ok := users.ResolveBound(l, fpOf(t, keyAda), users.Binding{Bound: true, Name: "bob"}, time.Now()); ok {
		t.Error("ResolveBound: ada's key resolved for a credential bound to bob")
	}
	if _, ok := l.User(users.ImplicitName); ok {
		t.Error("User(ImplicitName) found a user")
	}
	if !users.HasUserList(l) {
		t.Error("HasUserList is false")
	}
	if _, ok := l.User("dave"); ok {
		t.Error("User(dave) found a user the list does not name")
	}
	if got := names(l.Users()); got != "ada bob carol root" {
		t.Errorf("Users() = %q", got)
	}
	if l.Mode() != config.SSHAuthEnforce || l.MaxAuthTries() != 4 {
		t.Errorf("Mode %q, MaxAuthTries %d", l.Mode(), l.MaxAuthTries())
	}
}

// TestLookupLocalAdminNameSkipped checks a list entry named like a local
// admin is skipped and the local entry wins whole: none of the entry's keys,
// its admin flag or its expiries is added to the local admin.
func TestLookupLocalAdminNameSkipped(t *testing.T) {
	localLine, localFP := newTestKey(t)
	for _, tc := range []struct {
		name string
		keys []string
	}{
		{"its own key", []string{listKey(keyAda, "laptop", "")}},
		// The local key again, with another expiry and label.
		{"its own key and the local one", []string{listKey(keyAda, "laptop", ""), listKey(localLine, "other", "2020-01-01T00:00:00Z")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := localAdmin(localLine)
			local.Keys[0].Expires = "2098-06-01T00:00:00Z"
			l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"ada": local}}, decoded(t,
				// The list's ada is not an admin.
				listEntry("ada", false, tc.keys...),
				listEntry("bob", false, listKey(keyBob, "desk", "")),
			))

			if _, _, ok := l.UserForKey(fpOf(t, keyAda)); ok {
				t.Error("the list's ada's key resolved")
			}
			u, k, ok := l.UserForKey(localFP)
			if !ok || u != (users.User{Name: "ada", Admin: true}) {
				t.Fatalf("local key resolves to %+v, %v; want the local admin ada", u, ok)
			}
			want := time.Date(2098, 6, 1, 0, 0, 0, 0, time.UTC)
			if k.Source != users.SourceAdHoc || k.Label != "k0" || k.Expires == nil || !k.Expires.Equal(want) {
				t.Errorf("local key = %+v; want the local admin's own label and expiry", k)
			}
			if got, _ := l.User("ada"); !got.Admin {
				t.Error("ada is not an admin: the list's flag was taken")
			}
			if got := names(l.Users()); got != "ada bob" {
				t.Errorf("Users() = %q", got)
			}
			if got := skips(l.snap.Load()); got != "ada=local-admin-name" {
				t.Errorf("skip report %q", got)
			}
		})
	}
}

// TestLookupLocalAdminKeySkipped checks a list entry that lists a local
// admin's key is skipped whole, for any key the decoder counted, used or
// not: as a key it would use, as one it would not (an empty label, an expiry
// it cannot read), and as the same key with another comment. The local
// admin keeps the key.
func TestLookupLocalAdminKeySkipped(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"used", listKey(rootLine, "stolen", "")},
		{"empty label", listKey(rootLine, "", "")},
		{"unreadable expiry", listKey(rootLine, "x", "2027-01-01")},
		{"another comment", listKey(rootLine+" eve@laptop.example.test", "c", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := decoded(t,
				listEntry("eve", true, listKey(keyBob, "desk", ""), tc.key),
				listEntry("ada", false, listKey(keyAda, "laptop", "")),
			)
			if len(list.Users) != 2 {
				t.Fatalf("the decoder skipped eve itself (%v): this case does not reach the lookup's rule", list.Skipped)
			}
			l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": localAdmin(rootLine)}}, list)
			if u, k, ok := l.UserForKey(rootFP); !ok || u.Name != "root" || k.Source != users.SourceAdHoc {
				t.Errorf("root's key resolves to %+v %+v %v", u, k, ok)
			}
			if _, _, ok := l.UserForKey(fpOf(t, keyBob)); ok {
				t.Error("eve's other key resolved: eve was not skipped whole")
			}
			if _, ok := l.User("eve"); ok {
				t.Error("eve is a user")
			}
			if got := names(l.Users()); got != "ada root" {
				t.Errorf("Users() = %q", got)
			}
			if got := skips(l.snap.Load()); got != "eve=local-admin-key" {
				t.Errorf("skip report %q", got)
			}
		})
	}
}

// TestLookupExpiredLocalKeyStillExcludes checks a local admin's key whose
// expiry has already passed still belongs to the local admin alone: a list
// entry that lists it is skipped whole, and the key is still the local
// admin's (expired, so refused).
func TestLookupExpiredLocalKeyStillExcludes(t *testing.T) {
	liveLine, _ := newTestKey(t)
	pastLine, pastFP := newTestKey(t)
	local := localAdmin(liveLine, pastLine)
	local.Keys[1].Expires = "2020-01-01T00:00:00Z"
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": local}}, decoded(t,
		listEntry("eve", true, listKey(keyBob, "desk", ""), listKey(pastLine, "old", "")),
		listEntry("ada", false, listKey(keyAda, "laptop", "")),
	))
	u, k, ok := l.UserForKey(pastFP)
	if !ok || u.Name != "root" || k.Source != users.SourceAdHoc || !k.Expired(time.Now()) {
		t.Errorf("the expired local key resolves to %+v %+v %v; want root's, expired", u, k, ok)
	}
	if users.HasKey(l, pastFP) {
		t.Error("the expired local key is accepted")
	}
	if _, ok := l.User("eve"); ok {
		t.Error("eve is a user: an expired local key did not exclude her")
	}
	if _, _, ok := l.UserForKey(fpOf(t, keyBob)); ok {
		t.Error("eve's other key resolved")
	}
	if got := skips(l.snap.Load()); got != "eve=local-admin-key" {
		t.Errorf("skip report %q", got)
	}
}

// TestLookupHandBuiltLocalKeyInKeysOnly checks a hand-built entry whose Keys
// hold a local admin's key while its Fingerprints are missing is skipped
// whole all the same: the lookup's rule reads both.
func TestLookupHandBuiltLocalKeyInKeysOnly(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	list := &List{Users: []User{
		{Name: "eve", Admin: true, Keys: []Key{{Fingerprint: fpOf(t, keyBob), Label: "desk"}, {Fingerprint: rootFP, Label: "stolen"}}},
		{Name: "ada", Keys: []Key{{Fingerprint: fpOf(t, keyAda), Label: "laptop"}}},
	}}
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": localAdmin(rootLine)}}, list)
	if u, k, ok := l.UserForKey(rootFP); !ok || u.Name != "root" || k.Source != users.SourceAdHoc {
		t.Errorf("root's key resolves to %+v %+v %v", u, k, ok)
	}
	if _, ok := l.User("eve"); ok {
		t.Error("eve is a user")
	}
	if _, _, ok := l.UserForKey(fpOf(t, keyBob)); ok {
		t.Error("eve's other key resolved")
	}
	if got := names(l.Users()); got != "ada root" {
		t.Errorf("Users() = %q", got)
	}
	if got := skips(l.snap.Load()); got != "eve=local-admin-key" {
		t.Errorf("skip report %q", got)
	}
}

// TestLookupOwnSkipsSortedByName checks the lookup reports its own skips
// sorted by name whatever order a hand-built list's users are in.
func TestLookupOwnSkipsSortedByName(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	zedLine, _ := newTestKey(t)
	list := &List{Users: []User{
		{Name: "zed", Keys: []Key{{Fingerprint: fpOf(t, keyAda), Label: "a"}}},
		{Name: "root", Keys: []Key{{Fingerprint: fpOf(t, keyBob), Label: "b"}}},
		{Name: "eve", Keys: []Key{{Fingerprint: rootFP, Label: "r"}}},
		{Name: "ci", Keys: []Key{{Fingerprint: fpOf(t, keyCarol), Label: "c"}}},
	}}
	l := mustLookup(t, LookupOptions{
		LocalAdmins:    map[string]config.UserConfig{"root": localAdmin(rootLine), "zed": localAdmin(zedLine)},
		ServiceClients: []string{"ci"},
	}, list)
	if got, want := skips(l.snap.Load()), "ci=service-client-name eve=local-admin-key root=local-admin-name zed=local-admin-name"; got != want {
		t.Errorf("skip report\n got %q\nwant %q", got, want)
	}
}

// TestLookupServiceClientNameSkipped checks a list entry named like an
// auth.clients entry is skipped: neither the name nor its keys are a user.
// (internal/api's TestDirectoryServiceClientUnchanged checks the client
// itself still authenticates.)
func TestLookupServiceClientNameSkipped(t *testing.T) {
	rootLine, _ := newTestKey(t)
	l := mustLookup(t, LookupOptions{
		LocalAdmins:    map[string]config.UserConfig{"root": localAdmin(rootLine)},
		ServiceClients: []string{"ci-runner"},
	}, decoded(t,
		listEntry("ci-runner", true, listKey(keyBob, "desk", "")),
		listEntry("ada", false, listKey(keyAda, "laptop", "")),
	))
	if _, ok := l.User("ci-runner"); ok {
		t.Error("ci-runner is a user")
	}
	if _, _, ok := l.UserForKey(fpOf(t, keyBob)); ok {
		t.Error("the entry's key resolved")
	}
	if got := names(l.Users()); got != "ada root" {
		t.Errorf("Users() = %q", got)
	}
	if got := skips(l.snap.Load()); got != "ci-runner=service-client-name" {
		t.Errorf("skip report %q", got)
	}
}

// TestLookupWithoutListedUsers checks an empty list, a list whose every
// entry was skipped, and no list at all leave the local admins alone, and
// still no implicit user.
func TestLookupWithoutListedUsers(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	opts := LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": localAdmin(rootLine)}}
	for _, tc := range []struct {
		name   string
		list   *List
		listed bool
	}{
		{"no list", nil, false},
		{"empty list", decoded(t), true},
		{"all skipped", decoded(t, listEntry("root", true, listKey(keyAda, "l", "")), `null`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := mustLookup(t, opts, tc.list)
			if got := names(l.Users()); got != "root" {
				t.Errorf("Users() = %q", got)
			}
			if u, _, ok := l.UserForKey(rootFP); !ok || u.Name != "root" {
				t.Error("root's key does not resolve")
			}
			if _, _, ok := l.UserForKey(fpOf(t, keyAda)); ok {
				t.Error("a skipped entry's key resolved")
			}
			if !users.HasUserList(l) {
				t.Error("HasUserList is false")
			}
			if s := l.snap.Load(); (s.listed != nil) != tc.listed {
				t.Errorf("listed set present = %v, want %v", s.listed != nil, tc.listed)
			}
		})
	}
}

// TestLookupKeyExpiry checks a key with an expiry, a local admin's or a
// listed one, is reported with it, and is refused from that instant on by
// the checks the front doors make (users.HasKey, users.ResolveBound), as a
// key of auth.users is; and that no listed key is a script key.
func TestLookupKeyExpiry(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	pastLine, pastFP := newTestKey(t)
	at := time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)
	local := localAdmin(rootLine, pastLine)
	local.Keys[0].Expires = at.Format(time.RFC3339)
	local.Keys[1].Expires = "2020-01-01"
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": local}}, decoded(t,
		listEntry("ada", false, listKey(keyAda, "laptop", at.Format(time.RFC3339))),
		listEntry("bob", false, listKey(keyBob, "desk", "2020-01-01T00:00:00Z")),
		listEntry("carol", false, listKey(keyCarol, "phone", "")),
	))

	for _, fp := range []string{rootFP, fpOf(t, keyAda)} {
		u, k, ok := l.UserForKey(fp)
		if !ok || k.Expires == nil || !k.Expires.Equal(at) {
			t.Fatalf("UserForKey(%s) = %+v %+v %v; want the key with its expiry", fp, u, k, ok)
		}
		if k.Script {
			t.Errorf("%s's key is a script key", u.Name)
		}
		b := users.BindingOf(u)
		if _, _, ok := users.ResolveBound(l, fp, b, at.Add(-time.Nanosecond)); !ok {
			t.Errorf("%s's key refused before its expiry", u.Name)
		}
		for _, now := range []time.Time{at, at.Add(time.Nanosecond), at.Add(time.Hour)} {
			if _, _, ok := users.ResolveBound(l, fp, b, now); ok {
				t.Errorf("%s's key accepted at %s, expiry %s", u.Name, now, at)
			}
		}
		if !users.HasKey(l, fp) {
			t.Errorf("HasKey(%s) false today, before its expiry", u.Name)
		}
	}
	// Past expiries: still the user's key (the SSH door says "expired", not
	// "unknown"), never accepted.
	for _, fp := range []string{pastFP, fpOf(t, keyBob)} {
		u, k, ok := l.UserForKey(fp)
		if !ok || !k.Expired(time.Now()) {
			t.Errorf("UserForKey(%s) = %+v %+v %v; want the user's key, expired", fp, u, k, ok)
		}
		if users.HasKey(l, fp) {
			t.Errorf("HasKey accepts %s's expired key", u.Name)
		}
	}
	if _, k, _ := l.UserForKey(fpOf(t, keyCarol)); k.Expires != nil || k.Script {
		t.Errorf("carol's key = %+v; want no expiry, not script", k)
	}
}

// TestLookupSnapshotIsImmutable checks nothing a caller holds changes what
// the lookup answers: a key's expiry it was given, or the list it was built
// from.
func TestLookupSnapshotIsImmutable(t *testing.T) {
	rootLine, rootFP := newTestKey(t)
	local := localAdmin(rootLine)
	local.Keys[0].Expires = "2099-01-01T00:00:00Z"
	list := decoded(t, listEntry("ada", false, listKey(keyAda, "laptop", "2099-01-01T00:00:00Z")))
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": local}}, list)

	for _, fp := range []string{rootFP, fpOf(t, keyAda)} {
		_, k, _ := l.UserForKey(fp)
		*k.Expires = time.Time{}
		if _, again, _ := l.UserForKey(fp); again.Expires.Year() != 2099 {
			t.Errorf("changing a returned expiry changed the lookup's: %v", again.Expires)
		}
	}
	*list.Users[0].Keys[0].Expires = time.Time{}
	list.Users[0].Admin = true
	list.Users[0].Name = "mallory"
	if u, k, ok := l.UserForKey(fpOf(t, keyAda)); !ok || u.Name != "ada" || u.Admin || k.Expires.Year() != 2099 {
		t.Errorf("changing the list changed the lookup: %+v %+v %v", u, k, ok)
	}
	local.Keys[0].Key = keyBob
	if _, _, ok := l.UserForKey(fpOf(t, keyBob)); ok {
		t.Error("changing the config changed the lookup")
	}
}

// TestLookupSkipReport checks the skip report holds rule 7's skips, in the
// answer's order, then the lookup's own, by name; and that each key is in
// one of the two key sets at most. The snapshot is the first and fixed: gen
// 1, no trust deadline, not expired.
func TestLookupSkipReport(t *testing.T) {
	rootLine, _ := newTestKey(t)
	extraLine, _ := newTestKey(t)
	l := mustLookup(t, LookupOptions{
		LocalAdmins:    map[string]config.UserConfig{"root": localAdmin(rootLine), "zed": localAdmin(extraLine)},
		ServiceClients: []string{"ci"},
	}, decoded(t,
		listEntry("zed", true, listKey(keyAda, "a", "")),
		`{"name":"broken","admin":"yes","keys":[]}`,
		listEntry("ci", false, listKey(keyBob, "b", "")),
		listEntry("Bad", false, listKey(keyCarol, "c", "")),
		listEntry("eve", false, listKey(rootLine, "r", "")),
	))
	s := l.snap.Load()
	if got, want := skips(s), "broken=malformed Bad=invalid-name ci=service-client-name eve=local-admin-key zed=local-admin-name"; got != want {
		t.Errorf("skip report\n got %q\nwant %q", got, want)
	}
	for fp := range s.listed.keys {
		if _, both := s.local.keys[fp]; both {
			t.Errorf("key %s is in both sets", fp)
		}
	}
	if s.gen != 1 || !s.deadline.IsZero() || s.expired.Load() {
		t.Errorf("snapshot gen %d deadline %v expired %v; want 1, none, false", s.gen, s.deadline, s.expired.Load())
	}
}

// TestNewLookupRefuses checks the local admins config load would refuse
// beside auth.directory, and a list DecodeList could not have made, are
// refused rather than given to anyone.
func TestNewLookupRefuses(t *testing.T) {
	aLine, aFP := newTestKey(t)
	bLine, _ := newTestKey(t)
	good := map[string]config.UserConfig{"root": localAdmin(aLine)}
	scriptKey := localAdmin(aLine)
	scriptKey.Keys[0].Script = true
	badExpiry := localAdmin(aLine)
	badExpiry.Keys[0].Expires = "soon"
	adaKey := Key{Fingerprint: fpOf(t, keyAda), Label: "l"}
	noLabel := localAdmin(aLine)
	noLabel.Keys[0].Label = ""
	labelTwice := localAdmin(aLine, bLine)
	labelTwice.Keys[1].Label = labelTwice.Keys[0].Label
	adaBody := strings.TrimPrefix(fpOf(t, keyAda), "SHA256:")
	for _, tc := range []struct {
		name    string
		admins  map[string]config.UserConfig
		clients []string
		list    *List
		want    string
	}{
		{"a service client's name", good, []string{"ci", "root"}, nil, "also an auth.clients name"},
		{"an empty label", map[string]config.UserConfig{"root": noLabel}, nil, nil, "keys[0].label is required"},
		{"a label twice", map[string]config.UserConfig{"root": labelTwice}, nil, nil, `keys[1].label "k0" is used twice`},
		{"list: an MD5 fingerprint", good, nil, &List{Users: []User{{Name: "eve", Keys: []Key{{Fingerprint: "MD5:example", Label: "l"}}}}}, "canonical"},
		{"list: a lowercase prefix", good, nil, &List{Users: []User{{Name: "eve", Keys: []Key{{Fingerprint: "sha256:" + adaBody, Label: "l"}}}}}, "canonical"},
		{"list: a padded fingerprint", good, nil, &List{Users: []User{{Name: "eve", Keys: []Key{{Fingerprint: "SHA256:" + adaBody + "=", Label: "l"}}}}}, "canonical"},
		{"list: a short fingerprint", good, nil, &List{Users: []User{{Name: "eve", Keys: []Key{{Fingerprint: "SHA256:" + adaBody[:42], Label: "l"}}}}}, "canonical"},
		{"list: a counted fingerprint not canonical", good, nil, &List{Users: []User{{Name: "eve", Keys: []Key{adaKey}, Fingerprints: []string{"MD5:example"}}}}, "canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := newLookupOrNot(LookupOptions{LocalAdmins: tc.admins, ServiceClients: tc.clients}, tc.list)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewLookup = %v, %v; want an error containing %q", l, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		admins map[string]config.UserConfig
		list   *List
		want   string
	}{
		{"no local admin", nil, nil, "at least one local admin"},
		{"not an admin", map[string]config.UserConfig{"root": {Keys: localAdmin(aLine).Keys}}, nil, "admin: true"},
		{"a GitHub login", map[string]config.UserConfig{"root": {Admin: true, GitHub: "root", Keys: localAdmin(aLine).Keys}}, nil, "keys only"},
		{"a GitHub id", map[string]config.UserConfig{"root": {Admin: true, GitHubID: 7, Keys: localAdmin(aLine).Keys}}, nil, "keys only"},
		{"no key", map[string]config.UserConfig{"root": {Admin: true}}, nil, "needs a key"},
		{"invalid name", map[string]config.UserConfig{"Root": localAdmin(aLine)}, nil, "not a valid user name"},
		{"a script key", map[string]config.UserConfig{"root": scriptKey}, nil, "no script keys"},
		{"a bad key", map[string]config.UserConfig{"root": localAdmin("ssh-ed25519 AAAA")}, nil, `key "k0"`},
		{"a certificate", map[string]config.UserConfig{"root": localAdmin(testCertLine(t))}, nil, `key "k0"`},
		{"a bad expiry", map[string]config.UserConfig{"root": badExpiry}, nil, `key "k0"`},
		{"a key twice", map[string]config.UserConfig{"root": localAdmin(aLine), "toor": localAdmin(bLine, aLine)}, nil, aFP},
		{"list: a name twice", good, &List{Users: []User{{Name: "ada", Keys: []Key{adaKey}}, {Name: "ada", Keys: []Key{{Fingerprint: fpOf(t, keyBob)}}}}}, "listed twice"},
		{"list: a key twice", good, &List{Users: []User{{Name: "ada", Keys: []Key{adaKey}}, {Name: "bob", Keys: []Key{adaKey}}}}, "is listed under"},
		{"list: no key", good, &List{Users: []User{{Name: "ada"}}}, "has no key"},
		{"list: invalid name", good, &List{Users: []User{{Name: "", Keys: []Key{adaKey}}}}, "not a valid user name"},
		{"list: no fingerprint", good, &List{Users: []User{{Name: "ada", Keys: []Key{{Label: "l"}}}}}, "no fingerprint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := newLookupOrNot(LookupOptions{LocalAdmins: tc.admins}, tc.list)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewLookup = %v, %v; want an error containing %q", l, err, tc.want)
			}
		})
	}
}

// TestLookupUsersIsAFreshSlice checks Users returns a slice the caller owns.
func TestLookupUsersIsAFreshSlice(t *testing.T) {
	rootLine, _ := newTestKey(t)
	l := mustLookup(t, LookupOptions{LocalAdmins: map[string]config.UserConfig{"root": localAdmin(rootLine)}},
		decoded(t, listEntry("ada", false, listKey(keyAda, "l", ""))))
	first := l.Users()
	first[0] = users.User{Name: "mallory", Admin: true}
	if got := l.Users(); !reflect.DeepEqual(got, []users.User{{Name: "ada"}, {Name: "root", Admin: true}}) {
		t.Errorf("Users() = %+v", got)
	}
}
