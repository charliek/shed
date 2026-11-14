// Package users answers the server's identity questions: which user owns the
// SSH key a caller authenticated with, and who the users are.
//
// Both front doors ask it. The SSH server asks with the fingerprint of the key
// a connection authenticated with; the mtls API asks with a client
// certificate's Subject CN, which the bootstrap sets to that same fingerprint.
// The interface carries no SSH types so internal/api can depend on it without
// importing internal/sshd.
//
// Today there is no user list. Every key the server accepts belongs to one
// implicit user, spelled as the empty string, who is an admin and passes every
// check. Each key still records where it came from (an inline key, the keys
// file or a GitHub login), so a later user list can give keys to named users
// without losing that.
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
	// SourceGitHub, the keys file's path for SourceFile, "" for SourceInline.
	Origin string
	// Label is the key's comment from its authorized_keys line, if any.
	Label string
	// Expires is when the key stops being accepted; zero means never.
	Expires time.Time
	// Script marks a key meant for automation rather than a person.
	Script bool
}

// Expired reports whether k has stopped being accepted at now: it has an
// expiry, and now is not before it.
func (k Key) Expired(now time.Time) bool {
	return !k.Expires.IsZero() && !now.Before(k.Expires)
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

// Refresher is a Lookup whose keys come partly from GitHub logins, which it
// can be told to fetch again at once (`shed users refresh`).
type Refresher interface {
	// Refresh fetches every GitHub login again, checking each against its
	// pinned account id, and reports what it found. It joins a fetch that
	// has not started yet, and otherwise starts one after any fetch already
	// running, so its result is never older than the call. It returns when
	// the fetch ends or ctx ends, whichever is first.
	Refresh(ctx context.Context) (RefreshReport, error)
}

// UnknownKeyRefetcher is a Lookup that can look for a key it does not know
// on GitHub, at most once a minute for the whole server. The SSH front door
// asks it, in enforce mode only, when an unknown key logs in as `_bootstrap`
// or as a name that is a valid shed name.
type UnknownKeyRefetcher interface {
	// RefetchForUnknownKey takes the server-wide slot (one a minute) before
	// doing anything else; when it gets it, it starts a fetch on behalf of
	// the unknown key with this fingerprint, waits up to five seconds for
	// it, and reports whether the key is known (and not expired)
	// afterwards. Every other call returns false at once, having started
	// nothing and waited for nothing: while the slot is used, for a key
	// dropped because its GitHub login no longer names its pinned account,
	// and outside enforce mode.
	RefetchForUnknownKey(ctx context.Context, fingerprint string) bool
}

// RefreshReport is what one fetch of the GitHub logins found.
type RefreshReport struct {
	// Logins has one row per configured GitHub login, in config order.
	Logins []LoginRefresh
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

// HasKey reports whether l resolves the fingerprint to a user. It is the one
// "is this key accepted?" check every front door asks. A nil Lookup accepts
// nothing.
func HasKey(l Lookup, fingerprint string) bool {
	if l == nil {
		return false
	}
	_, _, ok := l.UserForKey(fingerprint)
	return ok
}
