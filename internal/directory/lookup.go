package directory

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	gossh "golang.org/x/crypto/ssh"
)

// A directory server's own rules on a list's entries, beside rule 7's: an
// entry the decoder admitted that this server does not answer for.
const (
	// SkipLocalAdminName: the entry's name is a local admin's. The local
	// admin is a separate, local-only identity and wins whole: nothing of
	// the entry (a key, the admin flag, an expiry) is added to it.
	SkipLocalAdminName SkipReason = "local-admin-name"
	// SkipLocalAdminKey: the entry lists a key a local admin holds: any key
	// the decoder counted, used or not (List.User.Fingerprints), or one in
	// its Keys. A key line the decoder refuses is never counted (rule 7's
	// counting), so it excludes nothing. The local admin keeps the key.
	SkipLocalAdminKey SkipReason = "local-admin-key"
	// SkipServiceClientName: the entry's name is an auth.clients name. The
	// service client is unaffected: it authenticates with its own
	// certificate, never through the user lookup.
	SkipServiceClientName SkipReason = "service-client-name"
)

// Skip is one entry of a list that the lookup does not answer for, and why:
// one rule 7 skipped (List.Skipped) or one this server's own rules skipped.
type Skip struct {
	// Name is the entry's name, "" when it had none as a string. The
	// service's text: quote it when printing.
	Name string
	// Reason is why the entry was skipped.
	Reason SkipReason
}

// LookupOptions are what a Lookup is built from: the server's own config on
// a directory server.
type LookupOptions struct {
	// LocalAdmins are auth.users beside auth.directory: admin-only,
	// key-only identities that hold whatever the list says. At least one.
	LocalAdmins map[string]config.UserConfig
	// ServiceClients are the names of auth.clients. A list entry with one
	// of these names is skipped.
	ServiceClients []string
	// MaxAuthTries is auth.ssh.max_auth_tries (0: the SSH server's default).
	MaxAuthTries int
}

// Lookup is the users.Lookup of a server whose users come from a directory
// (auth.directory). Its users are the local admins, from config, and the
// users of the list the directory serves, as far as this server's rules
// admit them (SkipLocalAdminName, SkipLocalAdminKey, SkipServiceClientName).
// There is no implicit user, so users.HasUserList is true: an unknown key is
// nobody's.
//
// Every reader answers from one snapshot, loaded once per call: a call never
// mixes two snapshots. A snapshot's users, keys and skip report never change
// once the lookup holds it.
type Lookup struct {
	maxAuthTries int
	// admins are the local admins, built once from config and shared by
	// every snapshot. Never changed.
	admins *userSet
	// clients holds the auth.clients names. Never changed.
	clients map[string]bool
	// snap is the snapshot every reader answers from.
	snap atomic.Pointer[snapshot]
}

var _ users.Lookup = (*Lookup)(nil)

// snapshot is one state of the lookup's users. Its users, keys and skip
// report never change once the lookup holds it.
type snapshot struct {
	// gen counts the snapshots the lookup has held; the first is 1.
	gen uint64
	// local are the local admins.
	local *userSet
	// listed are the list's users that this server admits; nil before the
	// lookup has a list. No name and no key is in both local and listed.
	listed *userSet
	// deadline and expired bound how long listed is honoured (the trust
	// time, a wall-clock deadline and a latch set once it has passed). A
	// snapshot built from a given list, as NewLookup builds, has neither:
	// a zero deadline and expired unset, and nothing reads them.
	deadline time.Time
	expired  atomic.Bool
	// skipped are the list's entries the lookup does not answer for: rule
	// 7's skips in List.Skipped's order (the answer's, as DecodeList makes
	// it), then this server's own, sorted by name.
	skipped []Skip
}

// userSet is a set of users and the keys that are theirs.
type userSet struct {
	// users by name.
	users map[string]users.User
	// keys by fingerprint (gossh.FingerprintSHA256).
	keys map[string]keyEntry
}

// keyEntry is one accepted key and whose it is.
type keyEntry struct {
	user string
	key  users.Key
}

// NewLookup builds the lookup from the local admins and list, a decoded
// directory answer (DecodeList), or nil for none: then only the local admins
// are users. It refuses local admins that config load would refuse beside
// auth.directory (none; one that is not an admin, is named like a service
// client, has a GitHub login, no key, a key with no label or a label used
// twice, a key that does not parse, a script key, or a key another holds)
// and a list that DecodeList could not have made (a name that is not a user
// name or is listed twice, a user with no key, a fingerprint that is not in
// the canonical gossh.FingerprintSHA256 form, a key listed twice).
func NewLookup(opts LookupOptions, list *List) (*Lookup, error) {
	clients := map[string]bool{}
	for _, name := range opts.ServiceClients {
		clients[name] = true
	}
	admins, err := localAdmins(opts.LocalAdmins, clients)
	if err != nil {
		return nil, err
	}
	l := &Lookup{maxAuthTries: opts.MaxAuthTries, admins: admins, clients: clients}
	s, err := l.build(list)
	if err != nil {
		return nil, err
	}
	s.gen = 1
	l.snap.Store(s)
	return l, nil
}

// localAdmins builds the local admins' set from auth.users beside
// auth.directory. Each is an admin with keys only; a key whose expiry has
// passed is kept (and refused by its callers from its instant on), as
// auth.users keeps one. clients are the auth.clients names, which no local
// admin may have.
func localAdmins(list map[string]config.UserConfig, clients map[string]bool) (*userSet, error) {
	if len(list) == 0 {
		return nil, errors.New("auth.directory needs at least one local admin in auth.users")
	}
	set := &userSet{users: make(map[string]users.User, len(list)), keys: map[string]keyEntry{}}
	names := make([]string, 0, len(list))
	for name := range list {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		uc := list[name]
		at := "auth.users." + name
		switch {
		case !config.ValidUserName(name):
			return nil, fmt.Errorf("auth.users: %q is not a valid user name", name)
		case clients[name]:
			return nil, fmt.Errorf("auth.users name %q is also an auth.clients name; a user and a service client must have different names", name)
		case !bool(uc.Admin):
			return nil, fmt.Errorf("%s: beside auth.directory every auth.users entry is a local admin (admin: true)", at)
		case uc.GitHub != "" || uc.GitHubID != 0:
			return nil, fmt.Errorf("%s: a local admin beside auth.directory has keys only, no GitHub login", at)
		case len(uc.Keys) == 0:
			return nil, fmt.Errorf("%s: a local admin needs a key", at)
		}
		set.users[name] = users.User{Name: name, Admin: true}
		labels := map[string]bool{}
		for i, kc := range uc.Keys {
			switch {
			case kc.Label == "":
				return nil, fmt.Errorf("%s.keys[%d].label is required", at, i)
			case labels[kc.Label]:
				return nil, fmt.Errorf("%s.keys[%d].label %q is used twice; labels are unique per user", at, i, kc.Label)
			}
			labels[kc.Label] = true
			if kc.Script {
				return nil, fmt.Errorf("%s key %q: a local admin beside auth.directory has no script keys", at, kc.Label)
			}
			pub, err := config.ParseUserKey(kc.Key)
			if err != nil {
				return nil, fmt.Errorf("%s key %q: %w", at, kc.Label, err)
			}
			expires, err := config.ParseKeyExpiry(kc.Expires)
			if err != nil {
				return nil, fmt.Errorf("%s key %q: %w", at, kc.Label, err)
			}
			fp := gossh.FingerprintSHA256(pub)
			if other, dup := set.keys[fp]; dup {
				return nil, fmt.Errorf("auth.users: key %s is listed under %s and %s; a key belongs to one user", fp, other.user, name)
			}
			set.keys[fp] = keyEntry{user: name, key: users.Key{
				Fingerprint: fp, Source: users.SourceAdHoc, Label: kc.Label, Expires: expires,
			}}
		}
	}
	return set, nil
}

// build makes a snapshot of the local admins and list (nil: no list yet),
// applying this server's rules to the list's users: an entry with a local
// admin's name, with a key a local admin holds (any key the decoder counted,
// used or not, or one in its Keys), or with an auth.clients name, is
// skipped, in that order of checks. These skips are reported sorted by name,
// whatever order the list's users are in. gen is the caller's to set.
func (l *Lookup) build(list *List) (*snapshot, error) {
	s := &snapshot{local: l.admins}
	if list == nil {
		return s, nil
	}
	if err := checkList(list); err != nil {
		return nil, err
	}
	for _, sk := range list.Skipped {
		s.skipped = append(s.skipped, Skip{Name: sk.Name, Reason: sk.Reason})
	}
	listed := &userSet{users: map[string]users.User{}, keys: map[string]keyEntry{}}
	var own []Skip
	for _, u := range list.Users {
		if reason := l.ruleOut(u); reason != "" {
			own = append(own, Skip{Name: u.Name, Reason: reason})
			continue
		}
		listed.users[u.Name] = users.User{Name: u.Name, Admin: u.Admin}
		for _, k := range u.Keys {
			listed.keys[k.Fingerprint] = keyEntry{user: u.Name, key: ownExpiry(users.Key{
				Fingerprint: k.Fingerprint, Source: users.SourceDirectory, Label: k.Label, Expires: k.Expires,
			})}
		}
	}
	// checkList refused a name listed twice, so names are unique here.
	sort.Slice(own, func(i, j int) bool { return own[i].Name < own[j].Name })
	s.skipped = append(s.skipped, own...)
	s.listed = listed
	return s, nil
}

// ruleOut is the first of this server's rules list user u fails, or "".
func (l *Lookup) ruleOut(u User) SkipReason {
	if _, ok := l.admins.users[u.Name]; ok {
		return SkipLocalAdminName
	}
	for _, fp := range u.Fingerprints {
		if _, ok := l.admins.keys[fp]; ok {
			return SkipLocalAdminKey
		}
	}
	for _, k := range u.Keys {
		if _, ok := l.admins.keys[k.Fingerprint]; ok {
			return SkipLocalAdminKey
		}
	}
	if l.clients[u.Name] {
		return SkipServiceClientName
	}
	return ""
}

// checkList refuses a list DecodeList could not have made, so that whatever
// the caller passes, a name is one user's, a key is one user's, and every
// fingerprint is in the canonical form users.Lookup answers for (the one
// gossh.FingerprintSHA256 makes of a key config.ParseUserKey accepted).
func checkList(list *List) error {
	names := map[string]bool{}
	keys := map[string]string{}
	for _, u := range list.Users {
		if !config.ValidUserName(u.Name) {
			return fmt.Errorf("list user %q is not a valid user name", u.Name)
		}
		if names[u.Name] {
			return fmt.Errorf("list user %q is listed twice", u.Name)
		}
		names[u.Name] = true
		if len(u.Keys) == 0 {
			return fmt.Errorf("list user %q has no key", u.Name)
		}
		for _, fp := range u.Fingerprints {
			if !canonicalFingerprint(fp) {
				return fmt.Errorf("list user %q has a fingerprint %q that is not in the canonical SHA256 form", u.Name, fp)
			}
		}
		for _, k := range u.Keys {
			if k.Fingerprint == "" {
				return fmt.Errorf("list user %q has a key with no fingerprint", u.Name)
			}
			if !canonicalFingerprint(k.Fingerprint) {
				return fmt.Errorf("list user %q has a key fingerprint %q that is not in the canonical SHA256 form", u.Name, k.Fingerprint)
			}
			if other, dup := keys[k.Fingerprint]; dup {
				return fmt.Errorf("list key %s is listed under %q and %q", k.Fingerprint, other, u.Name)
			}
			keys[k.Fingerprint] = u.Name
		}
	}
	return nil
}

// canonicalFingerprint reports whether fp is in gossh.FingerprintSHA256's
// form: "SHA256:" and the 32-byte digest in unpadded standard base64 (43
// characters, strictly encoded).
func canonicalFingerprint(fp string) bool {
	body, ok := strings.CutPrefix(fp, "SHA256:")
	if !ok || len(body) != 43 {
		return false
	}
	raw, err := base64.RawStdEncoding.Strict().DecodeString(body)
	return err == nil && len(raw) == sha256.Size
}

// Mode is the SSH key-auth mode: always enforce, since a directory server
// has a user list (auth.directory requires mtls).
func (l *Lookup) Mode() string { return config.SSHAuthEnforce }

// MaxAuthTries is auth.ssh.max_auth_tries, or 0 when unset (the SSH server
// then applies its own default).
func (l *Lookup) MaxAuthTries() int { return l.maxAuthTries }

// UserForKey reports which user owns the key with this fingerprint, and the
// key: a local admin's (users.SourceAdHoc) or a listed user's
// (users.SourceDirectory). A key with an expiry is reported with it, whether
// or not it has passed, as auth.users reports one: its callers refuse it
// from that instant on. No key is a script key. "" and any other spelling
// than the canonical gossh.FingerprintSHA256 form are nobody's.
func (l *Lookup) UserForKey(fingerprint string) (users.User, users.Key, bool) {
	if fingerprint == "" {
		return users.User{}, users.Key{}, false
	}
	s := l.snap.Load()
	if e, ok := s.local.keys[fingerprint]; ok {
		return s.local.users[e.user], ownExpiry(e.key), true
	}
	if s.listed != nil {
		if e, ok := s.listed.keys[fingerprint]; ok {
			return s.listed.users[e.user], ownExpiry(e.key), true
		}
	}
	return users.User{}, users.Key{}, false
}

// User returns the user with this name: a local admin, or a listed user.
// There is no implicit user: users.ImplicitName is nobody.
func (l *Lookup) User(name string) (users.User, bool) {
	s := l.snap.Load()
	if u, ok := s.local.users[name]; ok {
		return u, true
	}
	if s.listed != nil {
		if u, ok := s.listed.users[name]; ok {
			return u, true
		}
	}
	return users.User{}, false
}

// Users lists every user, the local admins and the listed users, sorted by
// name.
func (l *Lookup) Users() []users.User {
	s := l.snap.Load()
	n := len(s.local.users)
	if s.listed != nil {
		n += len(s.listed.users)
	}
	out := make([]users.User, 0, n)
	for _, u := range s.local.users {
		out = append(out, u)
	}
	if s.listed != nil {
		for _, u := range s.listed.users {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ownExpiry is k with its own copy of its expiry, so that neither the List a
// snapshot was built from nor a caller of UserForKey shares the snapshot's.
func ownExpiry(k users.Key) users.Key {
	if k.Expires != nil {
		t := *k.Expires
		k.Expires = &t
	}
	return k
}
