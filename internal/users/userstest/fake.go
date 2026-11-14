// Package userstest provides test doubles for users.Lookup.
package userstest

import (
	"sort"
	"sync"

	"github.com/charliek/shed/internal/users"
)

// Fake is a users.Lookup a test fills in by hand. It is safe for concurrent
// use, so a test can remove a key while requests are in flight. The zero value
// is not usable; build one with New or Implicit.
type Fake struct {
	mu    sync.RWMutex
	users map[string]users.User
	keys  map[string]fakeKey // fingerprint -> owner + provenance
}

type fakeKey struct {
	user string
	key  users.Key
}

var _ users.Lookup = (*Fake)(nil)

// New returns a Fake with no users and no keys.
func New() *Fake {
	return &Fake{users: map[string]users.User{}, keys: map[string]fakeKey{}}
}

// Implicit returns a Fake shaped like a server with no user list: the implicit
// user, owning a key for each of fingerprints (source inline).
func Implicit(fingerprints ...string) *Fake {
	f := New()
	f.AddUser(users.Implicit())
	for _, fp := range fingerprints {
		f.AddKey(users.ImplicitName, users.Key{Fingerprint: fp, Source: users.SourceInline})
	}
	return f
}

// AddUser adds or replaces a user.
func (f *Fake) AddUser(u users.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.Name] = u
}

// AddKey gives the key k (keyed by k.Fingerprint) to the user named user,
// replacing any earlier owner. It panics if that user has not been added, so a
// test cannot build a key that resolves to nobody by mistake.
func (f *Fake) AddKey(user string, k users.Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[user]; !ok {
		panic("userstest: AddKey for unknown user " + user)
	}
	f.keys[k.Fingerprint] = fakeKey{user: user, key: k}
}

// RemoveKey drops the key with this fingerprint, as a refresh that no longer
// finds it would.
func (f *Fake) RemoveKey(fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, fingerprint)
}

// UserForKey implements users.Lookup.
func (f *Fake) UserForKey(fingerprint string) (users.User, users.Key, bool) {
	if fingerprint == "" {
		return users.User{}, users.Key{}, false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.keys[fingerprint]
	if !ok {
		return users.User{}, users.Key{}, false
	}
	u, ok := f.users[e.user]
	if !ok {
		return users.User{}, users.Key{}, false
	}
	return u, e.key, true
}

// User implements users.Lookup.
func (f *Fake) User(name string) (users.User, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	u, ok := f.users[name]
	return u, ok
}

// Users implements users.Lookup.
func (f *Fake) Users() []users.User {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]users.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Matching is a users.Lookup on a server with no user list in which every
// fingerprint the predicate accepts is a key of the implicit user. It suits a
// test that swaps the accepted set wholesale mid-test; the predicate is called
// on every lookup and must be safe for concurrent use.
type Matching func(fingerprint string) bool

var _ users.Lookup = Matching(nil)

// UserForKey implements users.Lookup.
func (m Matching) UserForKey(fingerprint string) (users.User, users.Key, bool) {
	if fingerprint == "" || m == nil || !m(fingerprint) {
		return users.User{}, users.Key{}, false
	}
	return users.Implicit(), users.Key{Fingerprint: fingerprint, Source: users.SourceInline}, true
}

// User implements users.Lookup.
func (m Matching) User(name string) (users.User, bool) {
	if name == users.ImplicitName {
		return users.Implicit(), true
	}
	return users.User{}, false
}

// Users implements users.Lookup.
func (m Matching) Users() []users.User { return []users.User{users.Implicit()} }
