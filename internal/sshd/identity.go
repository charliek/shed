package sshd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/users"
)

// connIdentityKey is the connection-context key of the key a connection
// authenticated with.
type connIdentityKey struct{}

// connIdentity is the key a connection authenticated with: the key whose
// signature was verified (newConnIdentity), the username that signature was
// made for, and the user the key resolved to at that moment.
type connIdentity struct {
	key         gossh.PublicKey
	fingerprint string
	// user is the username the connection authenticated as. It selects the
	// shed every channel addresses (shedNameFor); it never says who the
	// caller is, which comes from key.
	user string
	// bound is the user the key resolved to when its signature was verified
	// (the implicit user included). Every later channel, _bootstrap
	// included, requires the key to resolve to this same user still
	// (principalFor), so a key given to another user since opens nothing
	// more on this connection as either of them.
	bound users.Binding
}

// newConnIdentity is the identity of a connection whose signature by key, for
// the username user, x/crypto has just verified: the key, the username, and
// the user the key resolves to now. A key that resolves to nobody now (it
// left the lookup between the offer and the signature) gives an error, and
// the connection does not authenticate.
func (s *Server) newConnIdentity(key gossh.PublicKey, user string) (connIdentity, error) {
	fp := gossh.FingerprintSHA256(key)
	p, err := s.resolvePrincipal(fp)
	if err != nil {
		return connIdentity{}, err
	}
	return connIdentity{key: key, fingerprint: fp, user: user, bound: bindingOf(p)}, nil
}

// setConnIdentity records id as the identity of the connection on ctx. It is
// called from the VerifiedPublicKeyCallback, so only with a key whose
// signature x/crypto has verified, never with one the client only asked about
// (see handlePublicKey), and with the username of that signed request (the
// connection's metadata there), never the first-seen username ctx.User()
// keeps.
func setConnIdentity(ctx ssh.Context, id connIdentity) {
	ctx.SetValue(connIdentityKey{}, id)
}

// authenticatedUser is the username the connection on ctx authenticated as,
// or "" when it has none on record. Every channel routes by this, never by
// ctx.User(): gliderlabs records the username of the connection's first
// authentication request there, and a client may change usernames before it
// signs.
func authenticatedUser(ctx context.Context) string {
	id, _ := connIdentityFrom(ctx)
	return id.user
}

// connIdentityFrom returns the key the connection authenticated with.
func connIdentityFrom(ctx context.Context) (connIdentity, bool) {
	id, ok := ctx.Value(connIdentityKey{}).(connIdentity)
	return id, ok && id.key != nil
}

// errNoIdentity is a channel on a connection with no authenticated key on
// record, which handlePublicKey never lets through.
var errNoIdentity = errors.New("this connection has no authenticated key")

// principalFor resolves the principal of a channel from the key its
// connection authenticated with. It is asked again for every channel (a
// session, sftp, a forwarded port, a wake, _bootstrap), so a key that leaves
// the user lookup, or expires, opens no new channel; nor does a key that now
// belongs to another user than the one it resolved to when the connection
// authenticated (connIdentity.bound): the connection is that user's or
// nobody's. Channels already open stay open. The user's admin flag and the
// key's script flag are read as they are now.
func (s *Server) principalFor(ctx context.Context) (backend.Principal, error) {
	id, ok := connIdentityFrom(ctx)
	if !ok {
		return backend.Principal{}, errNoIdentity
	}
	return s.principalForIdentity(id)
}

// principalForIdentity is principalFor for the connection identity id.
func (s *Server) principalForIdentity(id connIdentity) (backend.Principal, error) {
	if id.key == nil {
		return backend.Principal{}, errNoIdentity
	}
	p, err := s.resolvePrincipal(id.fingerprint)
	if err != nil {
		return backend.Principal{}, err
	}
	if !id.bound.Bound || bindingOf(p) != id.bound {
		return backend.Principal{}, fmt.Errorf("key %s no longer belongs to the user this connection authenticated as", id.fingerprint)
	}
	return p, nil
}

// bindingOf is the user a principal resolved from a key is, as a binding.
func bindingOf(p backend.Principal) users.Binding {
	return users.Binding{Bound: true, Name: p.User, Implicit: p.Implicit}
}

// resolvePrincipal resolves the principal of the key with this fingerprint
// from the user lookup as it is now.
//
// A key the lookup resolves is its user's. A key it does not resolve is
// refused in enforce mode; in off and warn mode (which accept every key) it
// is the implicit user, as every key was before users existed, but only on a
// server whose lookup has the implicit user (no user list): on a server with
// users an unknown key is nobody's.
func (s *Server) resolvePrincipal(fingerprint string) (backend.Principal, error) {
	if l := s.keyAuth.Users; l != nil {
		if u, k, ok := l.UserForKey(fingerprint); ok {
			if k.Expired(time.Now()) {
				return backend.Principal{}, fmt.Errorf("key %s expired at %s", fingerprint, k.Expires.UTC().Format(time.RFC3339))
			}
			return principalOf(u, k), nil
		}
	}
	if s.keyAuth.enforced() {
		return backend.Principal{}, fmt.Errorf("key %s is no longer authorized", fingerprint)
	}
	if s.keyAuth.Users == nil {
		return backend.ImplicitPrincipal(), nil
	}
	if u, ok := s.keyAuth.Users.User(users.ImplicitName); ok && u.Implicit {
		return backend.ImplicitPrincipal(), nil
	}
	return backend.Principal{}, fmt.Errorf("key %s belongs to no user", fingerprint)
}

// principalOf is the principal of user u acting with key k: the user's admin
// flag as configured now, and the key's script flag.
func principalOf(u users.User, k users.Key) backend.Principal {
	if u.Implicit {
		return backend.ImplicitPrincipal()
	}
	p := backend.UserPrincipal(u.Name, u.Admin)
	p.Script = k.Script
	return p
}

// guard returns the access guard the server's backend calls go through.
func (s *Server) guard() *backend.Guard {
	return backend.Guarded(s.backend)
}

// forbiddenMessage is what a caller who may see a shed but not enter it is
// told, for the guard's refusal err: an admin and another user's shed, an
// admin and a shed with no user (backend.ErrUnassigned), or a user who is not
// an admin and a shed that carries host directories or server mounts
// (backend.ErrHostResources).
func forbiddenMessage(name string, err error) string {
	if errors.Is(err, backend.ErrHostResources) {
		return fmt.Sprintf("shed '%s' carries host directories or server mounts; only an admin can enter or start it (you may still stop or delete it)", name)
	}
	if errors.Is(err, backend.ErrUnassigned) {
		return fmt.Sprintf("shed '%s' has no user; nobody can enter it until an admin gives it to a user with 'shed assign %s <user>'", name, name)
	}
	return fmt.Sprintf("shed '%s' belongs to another user; an admin can list, stop and delete it but not enter it", name)
}
