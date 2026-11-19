package directory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
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
	// Now is the wall clock the trust time is measured on; nil is
	// time.Now. Every instant it gives is used without its monotonic
	// reading (Round(0)). For tests.
	Now func() time.Time
	// TrustTicks starts the ticker WatchTrust checks the trust time on and
	// returns its ticks and how to stop it; nil ticks every
	// TrustCheckEvery. For tests.
	TrustTicks func() (ticks <-chan time.Time, stop func())
}

// TrustCheckEvery is how often WatchTrust checks whether the trust time of
// the list the lookup holds has ended.
const TrustCheckEvery = 30 * time.Second

// Publication is what the publication hook (SetPublicationHook) is told of a
// state the lookup has just published.
type Publication struct {
	// Gen is the published state's generation.
	Gen uint64
	// TrustEnded is true when the published state answers for the local
	// admins only because the trust time of its list has ended.
	TrustEnded bool
	// Has reports whether name is a user of the published state: a local
	// admin, or a listed user while the list is trusted. It answers from
	// that state alone, never blocks, and may be called at any time.
	Has func(name string) bool
}

// State is the lookup's current state, as its readers see it now.
type State struct {
	// Gen is the state's generation; the first is 1, and each publication
	// adds one.
	Gen uint64
	// Seq is the pull sequence number of the list the state holds (the
	// last one published); 0 before the first.
	Seq uint64
	// Listed is true when the state holds a list.
	Listed bool
	// FetchedAt is when the list's answer arrived, and Deadline when its
	// trust time ends, both on the wall clock (no monotonic reading). Zero
	// before the first list.
	FetchedAt, Deadline time.Time
	// Expired is true when the trust time of the list has ended: the
	// lookup answers for the local admins only until a new list is
	// installed.
	Expired bool
	// Skipped is the list's skip report (a copy).
	Skipped []Skip
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
//
// The list changes only through one publication point (publish): a newly
// pulled list (Install), or the end of its trust time (WatchTrust). A list
// is trusted until its deadline, the wall-clock instant its answer arrived
// plus the trust time; from that instant on (now == deadline included) the
// lookup answers for the local admins only, and keeps doing so, even if the
// clock moves back, until a new list is installed. The local admins are
// never subject to the trust time.
type Lookup struct {
	maxAuthTries int
	// admins are the local admins, built once from config and shared by
	// every snapshot. Never changed.
	admins *userSet
	// clients holds the auth.clients names. Never changed.
	clients map[string]bool
	// clock is the wall clock (LookupOptions.Now); read through now.
	clock func() time.Time
	// trustTicks starts WatchTrust's ticker (LookupOptions.TrustTicks).
	trustTicks func() (<-chan time.Time, func())
	// snap is the snapshot every reader answers from.
	snap atomic.Pointer[snapshot]
	// pubMu orders the publications: publish holds it from its check of the
	// current snapshot through the publication hook. Readers never take it.
	pubMu sync.Mutex
	// hook is the publication hook, guarded by pubMu.
	hook func(Publication)
	// latched is told, without blocking, each time a reader sets a
	// snapshot's latch (trusted): the runtime's trust watch then removes the
	// kept copy at once rather than at its next tick. It holds one signal; a
	// signal sent while one waits is dropped (the one waiting covers it).
	latched chan struct{}
	// loaded, when set (by tests only), runs in every reader just after it
	// has loaded its snapshot.
	loaded func()
	// latchRead, when set (by tests only), runs in trusted after its first
	// read of the latch and before its clock reading.
	latchRead func()
}

var (
	_ users.Lookup               = (*Lookup)(nil)
	_ users.BoundCredentialsOnly = (*Lookup)(nil)
)

// snapshot is one state of the lookup's users. Its users, keys and skip
// report never change once the lookup holds it.
type snapshot struct {
	// gen counts the snapshots the lookup has held; the first is 1.
	gen uint64
	// seq is the pull sequence number of the list the snapshot holds (0:
	// none, or a fixed list given to newLookup).
	seq uint64
	// local are the local admins.
	local *userSet
	// listed are the list's users that this server admits; nil before the
	// lookup has a list. No name and no key is in both local and listed.
	listed *userSet
	// fetchedAt is when the list's answer arrived and deadline when its
	// trust time ends, both on the wall clock with no monotonic reading. A
	// fixed list given to newLookup (tests only) has neither (zero): it has
	// no trust time.
	fetchedAt, deadline time.Time
	// expired is the latch: set by the first reader that finds the deadline
	// passed (now >= deadline), and from then on the snapshot answers for
	// the local admins only, whatever the clock says later.
	expired atomic.Bool
	// ended marks a snapshot published as the end of the trust time,
	// expired from the start: one made for it (trustEndOf), or a list whose
	// trust time had already ended when it was installed (Install). No
	// second end is published after it.
	ended bool
	// from is, on a snapshot made for the end of the trust time, the
	// snapshot it was made from: publish refuses it unless that one is
	// still the current one. nil once published.
	from *snapshot
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

// NewLookup builds the lookup from the local admins, with no list: only the
// local admins are users until a list is installed (Install), and every list
// it ever holds comes through Install, with its trust time. It refuses local
// admins that config load would refuse beside auth.directory (none; one that
// is not an admin, is named like a service client, has a GitHub login, no
// key, a key with no label or a label used twice, a key that does not parse,
// a script key, or a key another holds).
func NewLookup(opts LookupOptions) (*Lookup, error) {
	return newLookup(opts, nil)
}

// newLookup is NewLookup holding list, a decoded directory answer
// (DecodeList), or nil for none, from the start. A list given here has no
// trust time and never expires: for the package's own tests of a fixed list.
// It also refuses a list that DecodeList could not have made (a name that is
// not a user name or is listed twice, a user with no key, a fingerprint that
// is not in the canonical gossh.FingerprintSHA256 form, a key listed twice).
func newLookup(opts LookupOptions, list *List) (*Lookup, error) {
	clients := map[string]bool{}
	for _, name := range opts.ServiceClients {
		clients[name] = true
	}
	admins, err := localAdmins(opts.LocalAdmins, clients)
	if err != nil {
		return nil, err
	}
	l := &Lookup{
		maxAuthTries: opts.MaxAuthTries, admins: admins, clients: clients,
		clock: opts.Now, trustTicks: opts.TrustTicks, latched: make(chan struct{}, 1),
	}
	if l.clock == nil {
		l.clock = time.Now
	}
	if l.trustTicks == nil {
		l.trustTicks = func() (<-chan time.Time, func()) {
			t := time.NewTicker(TrustCheckEvery)
			return t.C, t.Stop
		}
	}
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
	s := l.load()
	if e, ok := s.local.keys[fingerprint]; ok {
		return s.local.users[e.user], ownExpiry(e.key), true
	}
	if listed := l.trusted(s); listed != nil {
		if e, ok := listed.keys[fingerprint]; ok {
			return listed.users[e.user], ownExpiry(e.key), true
		}
	}
	return users.User{}, users.Key{}, false
}

// User returns the user with this name: a local admin, or a listed user
// while the list is trusted. There is no implicit user: users.ImplicitName
// is nobody.
func (l *Lookup) User(name string) (users.User, bool) {
	s := l.load()
	if u, ok := s.local.users[name]; ok {
		return u, true
	}
	if listed := l.trusted(s); listed != nil {
		if u, ok := listed.users[name]; ok {
			return u, true
		}
	}
	return users.User{}, false
}

// Users lists every user, the local admins and, while the list is trusted,
// the listed users, sorted by name.
func (l *Lookup) Users() []users.User {
	s := l.load()
	listed := l.trusted(s)
	n := len(s.local.users)
	if listed != nil {
		n += len(listed.users)
	}
	out := make([]users.User, 0, n)
	for _, u := range s.local.users {
		out = append(out, u)
	}
	if listed != nil {
		for _, u := range listed.users {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// BoundCredentialsOnly marks the lookup as one whose users change while the
// server runs (users.BoundCredentialsOnly): a client certificate that names
// no user is refused, and a credential-bus stream checks its caller again
// before each message.
func (l *Lookup) BoundCredentialsOnly() {}

// State reports the lookup's current state. Like every reader it finds, and
// latches, the end of the list's trust time.
func (l *Lookup) State() State {
	s := l.load()
	return stateOf(s, l.trusted(s))
}

// stateOf is the State of s, whose trusted listed users (trusted) are
// listed, or nil.
func stateOf(s *snapshot, listed *userSet) State {
	return State{
		Gen:       s.gen,
		Seq:       s.seq,
		Listed:    s.listed != nil,
		FetchedAt: s.fetchedAt,
		Deadline:  s.deadline,
		Expired:   s.listed != nil && listed == nil,
		Skipped:   append([]Skip(nil), s.skipped...),
	}
}

// view is the lookup's part of a status or a refresh row: its state, the
// users it answers for (with where each comes from, sorted by name) and how
// many of them are the list's, all from one snapshot, loaded once, with one
// check of its trust time. Like every reader it finds, and latches, the end
// of the list's trust time.
type view struct {
	State
	Users []StatusUser
	// Admitted is how many of Users are the list's (0 when no list is in
	// force: none held, or its trust time ended).
	Admitted int
}

// view reads the lookup's view (see the type).
func (l *Lookup) view() view {
	s := l.load()
	listed := l.trusted(s)
	v := view{State: stateOf(s, listed)}
	v.Users = make([]StatusUser, 0, len(s.local.users))
	for _, u := range s.local.users {
		v.Users = append(v.Users, StatusUser{Name: u.Name, Admin: u.Admin, Source: UserLocal})
	}
	if listed != nil {
		for _, u := range listed.users {
			v.Users = append(v.Users, StatusUser{Name: u.Name, Admin: u.Admin, Source: UserDirectory})
		}
		v.Admitted = len(listed.users)
	}
	sort.Slice(v.Users, func(i, j int) bool { return v.Users[i].Name < v.Users[j].Name })
	return v
}

// load is the snapshot a reader answers from, loaded once.
func (l *Lookup) load() *snapshot {
	s := l.snap.Load()
	if l.loaded != nil {
		l.loaded()
	}
	return s
}

// now is the wall clock's reading with no monotonic reading: every instant
// the trust time stores or compares is one of these. Go compares two times
// that both carry a monotonic reading on that reading, which does not count
// the time a host spent asleep; on the wall clock, a host that slept past
// the deadline stops trusting the list at once.
func (l *Lookup) now() time.Time { return l.clock().Round(0) }

// trusted is s's listed users while s's list is trusted, or nil: when s has
// no list, or the list's trust time has ended. A reader that finds the
// deadline passed (now >= deadline) sets s's latch, and from then on s
// answers nil whatever the clock says.
//
// The latch is read again after the clock reading, before the listed users
// are accepted: another reader may have set it between this one's first read
// and its clock reading, and the clock may have moved back since, so a
// reading before the deadline alone does not show the list is still trusted.
//
// The reader that sets the latch tells the runtime so (latched), without
// blocking and without taking a lock or touching the disk: the runtime's
// trust watch removes the kept copy at once.
func (l *Lookup) trusted(s *snapshot) *userSet {
	if s.listed == nil || s.expired.Load() {
		return nil
	}
	if s.deadline.IsZero() {
		return s.listed
	}
	if l.latchRead != nil {
		l.latchRead()
	}
	if !l.now().Before(s.deadline) {
		if s.expired.CompareAndSwap(false, true) {
			select {
			case l.latched <- struct{}{}:
			default:
			}
		}
		return nil
	}
	if s.expired.Load() {
		return nil
	}
	return s.listed
}

// noPull is the pull sequence number of a publication no pull made: the end
// of the trust time.
const noPull = 0

// publish is the lookup's one publication point. Under pubMu it refuses a
// stale next: one made by a pull (pullSeq) whose sequence number is not
// greater than the current snapshot's, a result of an older pull that ended
// late; or one made for the end of the trust time (pullSeq noPull) from a
// snapshot that is no longer the current one. Otherwise it numbers next
// (gen), makes it the snapshot readers load, and runs the publication hook
// before it lets pubMu go, so hooks run one at a time and in the order of
// the publications. All network work is done before publish, which does
// none. It reports whether next was published, and the snapshot it replaced
// (nil when it was not published).
func (l *Lookup) publish(next *snapshot, pullSeq uint64) (prev *snapshot, ok bool) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	cur := l.snap.Load()
	if pullSeq == noPull {
		if next.from != cur {
			return nil, false
		}
		next.seq = cur.seq
	} else {
		if pullSeq <= cur.seq {
			return nil, false
		}
		next.seq = pullSeq
	}
	next.from = nil
	next.gen = cur.gen + 1
	l.snap.Store(next)
	if l.hook != nil {
		l.hook(l.publication(next))
	}
	return cur, true
}

// publication is what the hook is told of s, just published.
func (l *Lookup) publication(s *snapshot) Publication {
	local, listed := s.local, l.trusted(s)
	return Publication{
		Gen:        s.gen,
		TrustEnded: s.listed != nil && listed == nil,
		Has: func(name string) bool {
			if _, ok := local.users[name]; ok {
				return true
			}
			if listed != nil {
				_, ok := listed.users[name]
				return ok
			}
			return false
		},
	}
}

// SetPublicationHook sets the function every publication runs with the
// state it published (nil: none), from then on. It runs inside the
// publication, under the lock that orders publications, so hooks run one at
// a time, in the order of the publications, after readers already see the
// new state; the next publication waits for it.
//
// The hook's contract: no network and no other blocking work, and no call
// into a Runtime over the lookup (a pull's publication runs under the
// runtime's lock, which Status and Refresh's row take). On a directory
// server it does one thing, close the sockets of the SSH connections of users
// the new state no longer has (sshd's CloseConnectionsExcept, which closes
// every such socket first and leaves its log lines to a goroutine of their
// own, so a blocked log writer holds no publication).
func (l *Lookup) SetPublicationHook(fn func(Publication)) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.hook = fn
}

// Install publishes list, a good pull's decoded answer (DecodeList), as the
// list the lookup answers for: the pull numbered seq (1 or more, increasing
// with each pull started), whose answer arrived at fetchedAt on the wall
// clock and is trusted for trust from then. It reports whether the list was
// published: false, with nothing changed, when a pull with a greater or equal
// number has been published already. The lookup's own rules apply to the
// list (Skip). It refuses a list DecodeList could not have made, no list, a
// trust time that is not positive, a zero fetchedAt, a fetchedAt after the
// lookup's own clock reading (no tolerance), and seq 0.
//
// A list whose trust time has already ended by that clock reading (a late
// result) is published as the end of the trust time at once: expired from
// the start, so the publication answers for the local admins only and is the
// one end published (the trust ticker publishes no second one). Logged once,
// loudly, unless the state it replaces had already ended.
func (l *Lookup) Install(list *List, fetchedAt time.Time, trust time.Duration, seq uint64) (bool, error) {
	switch {
	case list == nil:
		return false, errors.New("directory: no list to install")
	case trust <= 0:
		return false, fmt.Errorf("directory: trust time %s is not positive", trust)
	case fetchedAt.IsZero():
		return false, errors.New("directory: the list has no fetched-at instant")
	case seq == noPull:
		return false, errors.New("directory: a list is installed with a pull sequence number of 1 or more")
	}
	now := l.now()
	if at := fetchedAt.Round(0); at.After(now) {
		return false, fmt.Errorf("directory: the list's fetched-at instant %s is after this server's clock (%s)",
			at.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	}
	s, err := l.build(list)
	if err != nil {
		return false, err
	}
	s.fetchedAt = fetchedAt.Round(0)
	s.deadline = s.fetchedAt.Add(trust)
	if !now.Before(s.deadline) {
		s.ended = true
		s.expired.Store(true)
	}
	prev, ok := l.publish(s, seq)
	if ok && s.ended && !prev.ended {
		logTrustEnded(s)
	}
	return ok, nil
}

// WatchTrust checks, at every tick of its ticker (every TrustCheckEvery),
// whether the trust time of the list the lookup holds has ended, and when it
// has publishes the end (checkTrust), until ctx ends.
func (l *Lookup) WatchTrust(ctx context.Context) { l.watchTrust(ctx, nil, nil) }

// watchTrust is WatchTrust, running after (when set) after each tick's
// check, once the publication it may have made, and its hook, are over; and
// also, with no check of its own, each time latched is told that a reader
// set a snapshot's latch (nil: never). The ticker stays the backstop, and
// the end of the trust time is published at its ticks only.
func (l *Lookup) watchTrust(ctx context.Context, latched <-chan struct{}, after func()) {
	ticks, stop := l.trustTicks()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			l.checkTrust()
			if after != nil {
				after()
			}
		case <-latched:
			if after != nil {
				after()
			}
		}
	}
}

// checkTrust publishes the end of the trust time when the current snapshot
// holds a list whose deadline has passed (or whose latch is set) and that
// end is not published yet: a snapshot with the same users, expired from the
// start, so the publication hook closes the listed users' connections as it
// does a removed user's. Logged once, loudly. It reports whether it
// published.
func (l *Lookup) checkTrust() bool {
	s := l.snap.Load()
	if s.listed == nil || s.ended || l.trusted(s) != nil {
		return false
	}
	if _, ok := l.publish(trustEndOf(s), noPull); !ok {
		return false
	}
	logTrustEnded(s)
	return true
}

// logTrustEnded logs, loudly, the publication of the end of the trust time
// of s's list.
func logTrustEnded(s *snapshot) {
	log.Printf("directory: TRUST ENDED: the list whose answer arrived at %s was trusted until %s and no good pull has replaced it; only the local admins can log in until one does, and the listed users' SSH connections are closed",
		s.fetchedAt.UTC().Format(time.RFC3339), s.deadline.UTC().Format(time.RFC3339))
}

// withdraw publishes, as the result of pull seq, a state with no list: only
// the local admins are users until a list is installed again, and the
// publication hook closes the listed users' connections. It is what a pull
// does when it finds the server no longer enrolled, or its enrolment
// unusable. It reports whether it published (false when the lookup holds no
// list, or a pull numbered seq or later has been published).
func (l *Lookup) withdraw(seq uint64) bool {
	if l.snap.Load().listed == nil {
		return false
	}
	_, ok := l.publish(&snapshot{local: l.admins}, seq)
	return ok
}

// held is what the current snapshot holds of its list, whether or not it is
// still trusted: its admitted users (nil with no list) and every entry it
// does not answer for.
func (l *Lookup) held() (listed map[string]users.User, skipped []Skip) {
	s := l.snap.Load()
	if s.listed != nil {
		listed = s.listed.users
	}
	return listed, s.skipped
}

// UserSource says where a user of a directory server comes from.
type UserSource string

// The sources of a user.
const (
	// UserLocal is a local admin (auth.users beside auth.directory).
	UserLocal UserSource = "local"
	// UserDirectory is a user of the directory's list.
	UserDirectory UserSource = "directory"
)

// StatusUser is one user of the lookup as status shows it: no keys.
type StatusUser struct {
	Name   string
	Admin  bool
	Source UserSource
}

// trustEndOf is the snapshot that publishes the end of s's trust time: s's
// users, expired from the start, made from s (so publish refuses it once s
// is no longer the current snapshot).
func trustEndOf(s *snapshot) *snapshot {
	end := &snapshot{
		local: s.local, listed: s.listed, fetchedAt: s.fetchedAt, deadline: s.deadline,
		skipped: s.skipped, ended: true, from: s,
	}
	end.expired.Store(true)
	return end
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
