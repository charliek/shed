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

import "time"

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
