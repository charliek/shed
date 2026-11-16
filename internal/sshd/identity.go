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
// signature was verified (setConnIdentity).
type connIdentity struct {
	key         gossh.PublicKey
	fingerprint string
}

// setConnIdentity records key as the key the connection authenticated with.
// It is called from the VerifiedPublicKeyCallback, so only with a key whose
// signature x/crypto has verified, never with one the client only asked
// about (see handlePublicKey).
func setConnIdentity(ctx ssh.Context, key gossh.PublicKey) {
	ctx.SetValue(connIdentityKey{}, connIdentity{key: key, fingerprint: gossh.FingerprintSHA256(key)})
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
// the user lookup, or expires, opens no new channel. Channels already open
// stay open.
//
// A key the lookup resolves is its user's. A key it does not resolve is
// refused in enforce mode; in off and warn mode (which accept every key) it
// is the implicit user, as every key was before users existed, but only on a
// server whose lookup has the implicit user (no user list): on a server with
// users an unknown key is nobody's.
func (s *Server) principalFor(ctx context.Context) (backend.Principal, error) {
	id, ok := connIdentityFrom(ctx)
	if !ok {
		return backend.Principal{}, errNoIdentity
	}
	if l := s.keyAuth.Users; l != nil {
		if u, k, ok := l.UserForKey(id.fingerprint); ok {
			if k.Expired(time.Now()) {
				return backend.Principal{}, fmt.Errorf("key %s expired at %s", id.fingerprint, k.Expires.UTC().Format(time.RFC3339))
			}
			return principalOf(u, k), nil
		}
	}
	if s.keyAuth.enforced() {
		return backend.Principal{}, fmt.Errorf("key %s is no longer authorized", id.fingerprint)
	}
	if s.keyAuth.Users == nil {
		return backend.ImplicitPrincipal(), nil
	}
	if u, ok := s.keyAuth.Users.User(users.ImplicitName); ok && u.Implicit {
		return backend.ImplicitPrincipal(), nil
	}
	return backend.Principal{}, fmt.Errorf("key %s belongs to no user", id.fingerprint)
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
