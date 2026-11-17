// Package users answers the server's identity questions: which user owns the
// SSH key a caller authenticated with, and who the users are.
//
// Both front doors ask it. The SSH server asks with the fingerprint of the key
// a connection authenticated with; the mtls API asks with a client
// certificate's Subject CN, which the bootstrap sets to that same fingerprint.
// The interface carries no SSH types so internal/api can depend on it without
// importing internal/sshd.
//
// A server with no user list (no auth.users) has one implicit user, spelled as
// the empty string, who is an admin and passes every check: every key the
// server accepts is theirs. A server with a user list has named users only:
// each key belongs to exactly one of them, and an unknown key is nobody's.
// Each key records where it came from (an inline key, the keys file, a GitHub
// login, a key listed by hand under a user, or a directory's list).
package users

import (
	"context"
	"time"
)

// ImplicitName is the implicit user's name: the empty string. It is the name a
// record, a principal and a binding carry on a server with no user list.
const ImplicitName = ""

// User is one person the server knows.
type User struct {
	// Name is the user's name; ImplicitName for the implicit user.
	Name string
	// Admin users may list, stop and delete sheds that are not theirs.
	Admin bool
	// Implicit marks the one user of a server with no user list. It passes
	// every check.
	Implicit bool
	// GitHub is the GitHub login whose published keys belong to this user,
	// or "" when the user has none. The implicit user has none: on a server
	// with no user list each GitHub key names its login in Key.Origin.
	GitHub string
}

// Implicit returns the implicit user: the one user of a server with no user
// list, an admin who passes every check.
func Implicit() User {
	return User{Name: ImplicitName, Admin: true, Implicit: true}
}

// Source says where an accepted key came from.
type Source string

const (
	// SourceInline is a key listed inline in auth.ssh.authorized_keys.
	SourceInline Source = "inline"
	// SourceFile is a key read from auth.ssh.authorized_keys_file.
	SourceFile Source = "file"
	// SourceGitHub is a key a GitHub login publishes.
	SourceGitHub Source = "github"
	// SourceAdHoc is a key listed by hand under a user in auth.users.
	SourceAdHoc Source = "adhoc"
	// SourceDirectory is a key the server's directory lists under a user
	// (auth.directory): an account service the server pulls its user list
	// from.
	SourceDirectory Source = "directory"
)

// Key is one accepted SSH public key and where it came from.
type Key struct {
	// Fingerprint is the key's gossh.FingerprintSHA256 form
	// ("SHA256:<43 base64 chars>"), the same string an issued client
	// certificate carries as its Subject CN.
	Fingerprint string
	// Source is where the key came from.
	Source Source
	// Origin names the place within Source: the GitHub login for
	// SourceGitHub, the keys file's path for SourceFile, "" for SourceInline
	// and SourceAdHoc.
	Origin string
	// Label is the key's label in auth.users (SourceAdHoc), or its comment
	// from its authorized_keys line, if any.
	Label string
	// Expires is when the key stops being accepted; nil means never. Any
	// time it points at is an expiry, the zero time included.
	Expires *time.Time
	// Script marks a key meant for automation rather than a person. It may
	// not take a credentials certificate, use the credential bus, or move a
	// credential source.
	Script bool
}

// Expired reports whether k has stopped being accepted at now: it has an
// expiry, and now is not before it.
func (k Key) Expired(now time.Time) bool {
	return k.Expires != nil && !now.Before(*k.Expires)
}

// Lookup is the user seam. Implementations must answer from live state (a key
// that leaves its source must stop resolving as soon as the implementation
// notices) and must be safe for concurrent use.
type Lookup interface {
	// UserForKey reports which user owns the key with this fingerprint, and
	// the key's provenance. ok is false for a key the server does not accept,
	// and for "" and any other spelling than the canonical
	// gossh.FingerprintSHA256 form.
	UserForKey(fingerprint string) (user User, key Key, ok bool)
	// User returns the user with this name. ImplicitName names the implicit
	// user on a server with no user list.
	User(name string) (User, bool)
	// Users lists every user, sorted by name.
	Users() []User
}

// Refresher is a Lookup whose keys come partly from GitHub logins, or from a
// directory's list, which it can be told to fetch again at once (`shed users
// refresh`).
type Refresher interface {
	// Refresh fetches every GitHub login again, checking each against its
	// pinned account id (or pulls the directory's list again), and reports
	// what it found. It joins a fetch that has not started yet, and otherwise
	// starts one after any fetch already running, so its result is never
	// older than the call. It returns when the fetch ends or ctx ends,
	// whichever is first.
	Refresh(ctx context.Context) (RefreshReport, error)
}

// UnknownKeyRefetcher is a Lookup that can fetch its users again for a key
// it does not know (from GitHub logins, or a directory's list), at a bounded
// rate for the whole server. The SSH front door asks it, in enforce mode
// only, when an unknown key logs in as `_bootstrap` or as a name that is a
// valid shed name.
type UnknownKeyRefetcher interface {
	// RefetchForUnknownKey takes a server-wide slot before doing anything
	// else; when it gets it, it starts (or joins) a fetch on behalf of the
	// unknown key with this fingerprint, waits for it up to a bounded time,
	// and reports whether the key is known (and not expired) afterwards.
	// Every other call returns false at once, having started nothing and
	// waited for nothing. How often the slot may be taken, how long the
	// wait is, and when else a call is refused are each implementation's,
	// stated on it (ConfigLookup: one a minute, a five-second wait;
	// directory.Runtime: by the state of its last pull, one 20-second
	// deadline). ctx ending ends the wait, not the fetch.
	RefetchForUnknownKey(ctx context.Context, fingerprint string) bool
}

// BoundCredentialsOnly is a Lookup whose users change while the server runs
// (a directory's list, auth.directory), so that a name may stop being a user,
// or a key move to another user, at any moment. Two things follow on a
// server whose lookup implements it, and on no other:
//   - a credential that names no user (Binding.Bound false: a client
//     certificate issued before certificates named their user) is refused
//     outright rather than checked by its key alone, since its key alone
//     cannot say whose it was; its client enrols again;
//   - a credential-bus stream checks its caller's certificate again before
//     each message it sends, and ends when the certificate no longer
//     authenticates its user.
//
// The two lookups of a server with no user list or with auth.users do not
// implement it.
type BoundCredentialsOnly interface {
	Lookup
	// BoundCredentialsOnly marks the lookup; it does nothing.
	BoundCredentialsOnly()
}

// RefreshReport is what one fetch of the GitHub logins found, or, on a
// server whose users come from a directory, what one pull of its list found.
type RefreshReport struct {
	// Logins has one row per configured GitHub login, in config order.
	Logins []LoginRefresh
	// Directory is the pull's row on a server whose users come from a
	// directory (auth.directory); nil on every other server.
	Directory *DirectoryRefresh
}

// Directory refresh statuses (DirectoryRefresh.Status).
const (
	// DirectoryRefreshGood: the pull was good and its list is the one the
	// server answers for.
	DirectoryRefreshGood = "good"
	// DirectoryRefreshFailed: the pull failed (the service refused it, could
	// not be reached, answered with something the server refuses, or the
	// server is not enrolled). The list the server held before stays.
	DirectoryRefreshFailed = "failed"
)

// DirectoryRefresh is a directory pull's row in a RefreshReport.
type DirectoryRefresh struct {
	// Status is DirectoryRefreshGood or DirectoryRefreshFailed.
	Status string
	// Version is the version of the list the server answers for after the
	// pull ("" when it holds none): the pull's own when it was good.
	Version string
	// Users is how many of that list's users the server admits, and Skipped
	// how many of its entries it does not answer for (the local admins are
	// not counted).
	Users, Skipped int
	// Error says why the pull failed, when it did. Text from the service in
	// it is quoted.
	Error string
}

// Login refresh statuses (LoginRefresh.Status).
const (
	// RefreshOK: the login named its pinned account; its keys are current.
	RefreshOK = "ok"
	// RefreshPinned: the login had no pin and is now pinned to the account
	// it named.
	RefreshPinned = "pinned"
	// RefreshUnchanged: a check made for an unknown key found the login's
	// keys as they were; the account was not asked about.
	RefreshUnchanged = "unchanged"
	// RefreshUnreachable: GitHub could not be asked (network, an error, a
	// rate limit) or answered inconsistently. The last known keys stay and
	// nothing new is accepted.
	RefreshUnreachable = "unreachable"
	// RefreshMismatch: the login names another account than its pin, or
	// none. Its keys are dropped until it names the pinned account again.
	RefreshMismatch = "mismatch"
	// RefreshGone: a login with no pin names no account. It has no keys.
	RefreshGone = "gone"
	// RefreshPinWriteFailed: the fetch was good but the pin store could not
	// be written, so nothing new is accepted (keys GitHub stopped listing
	// still leave).
	RefreshPinWriteFailed = "pin-write-failed"
	// RefreshInvalidLogin: the configured login is not a valid GitHub login
	// and is never fetched.
	RefreshInvalidLogin = "invalid-login"
)

// LoginRefresh is one GitHub login's row in a RefreshReport.
type LoginRefresh struct {
	// User is the name of the user the login's keys belong to.
	User string
	// Login is the GitHub login as configured.
	Login string
	// Status is one of the Refresh* statuses.
	Status string
	// PinnedID is the account id the login is pinned to; 0 for none.
	PinnedID int64
	// SeenID is, on a mismatch, the account the login names instead (0 when
	// it names none).
	SeenID int64
	// Keys is how many of the login's keys are accepted after the fetch.
	Keys int
	// Added and Removed are the fingerprints the fetch added and removed.
	Added, Removed []string
	// Error says why the login is not ok, when it is not.
	Error string
}

// HasKey reports whether l resolves the fingerprint to a user and the key has
// not expired. It is the one "is this key accepted?" check every front door
// asks. A nil Lookup accepts nothing.
func HasKey(l Lookup, fingerprint string) bool {
	if l == nil {
		return false
	}
	_, k, ok := l.UserForKey(fingerprint)
	return ok && !k.Expired(time.Now())
}

// Binding is the user a credential was issued to, or a connection
// authenticated as: the user its key resolved to at that moment. A key can
// change hands later (an operator gives it to another user, or a GitHub
// refresh finds it under another user's login), and a credential bound to
// one user must not then act as the other, so whoever checks the credential
// asks ResolveBound, not UserForKey.
type Binding struct {
	// Bound is false for a credential that names no user: a client
	// certificate issued before certificates named one. It is checked by
	// its key alone, as it always was, until it expires.
	Bound bool
	// Name is the user's name; ImplicitName for the implicit user.
	Name string
	// Implicit marks the implicit user. The implicit user is a user for
	// this purpose: a credential bound to it never matches a named user,
	// nor the reverse, whatever the names.
	Implicit bool
}

// BindingOf is the binding to user u.
func BindingOf(u User) Binding { return Binding{Bound: true, Name: u.Name, Implicit: u.Implicit} }

// Matches reports whether u is the user b binds to. An unbound b matches any
// user.
func (b Binding) Matches(u User) bool {
	return !b.Bound || (b.Implicit == u.Implicit && b.Name == u.Name)
}

// ResolveBound is UserForKey for a credential bound to b: the key with this
// fingerprint must resolve to a user, must not have expired at now, and must
// still belong to the user b binds to. ok is false otherwise, and with a nil
// Lookup. Admin status and the key's script mark come from the lookup as it
// is now, never from the binding.
func ResolveBound(l Lookup, fingerprint string, b Binding, now time.Time) (User, Key, bool) {
	if l == nil {
		return User{}, Key{}, false
	}
	u, k, ok := l.UserForKey(fingerprint)
	if !ok || k.Expired(now) || !b.Matches(u) {
		return User{}, Key{}, false
	}
	return u, k, true
}

// HasUserList reports whether l has named users: a server whose lookup does
// not answer for the implicit user. On such a server an unknown key is
// nobody's, in every SSH mode, and never an admin. A nil Lookup has none.
func HasUserList(l Lookup) bool {
	if l == nil {
		return false
	}
	u, ok := l.User(ImplicitName)
	return !ok || !u.Implicit
}
