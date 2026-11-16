package api

import (
	"context"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// Caller kinds.
const (
	// CallerUser is every caller that is not a service client: a person, by
	// a user's client certificate, or the implicit user in open mode and on
	// a server with no user list (an admin who passes every check, exactly
	// as every caller was before users existed).
	CallerUser = "user"
	// CallerClient is a service client (auth.clients) authenticated by its
	// service certificate. It reaches only the client route allowlist and
	// only the sheds it owns.
	CallerClient = "client"
)

// Caller is the authenticated identity of one API request, set in the request
// context by the auth middleware together with the backend.Principal the
// access guard decides on (withCaller sets both).
type Caller struct {
	// Kind is CallerUser or CallerClient.
	Kind string
	// Name is the client name for a service client, or the user's
	// certificate subject (an SSH key fingerprint; empty in open mode).
	Name string
	// User is the user's name ("" for the implicit user). Users only.
	User string
	// Admin and Implicit describe the user (see users.User).
	Admin    bool
	Implicit bool
	// Script is set when the user's certificate is for a script key
	// (users.Key.Script): it may not use the credential bus.
	Script bool
	// KeyLabel is the label the user's key has in auth.users, when it has
	// one: shown (as the mover of a credential source), never matched. An
	// authorized_keys comment is not used: it often names a person's account
	// and host.
	KeyLabel string
	// Serial is the client certificate's serial (lowercase hex), when the
	// request carried one.
	Serial string
	// Client is the service client's config (nil for a user).
	Client *config.ServiceClientConfig
}

// IsClient reports whether the caller is a service client.
func (c Caller) IsClient() bool { return c.Kind == CallerClient }

// IsAdmin reports whether the caller may use what is shared on the host: an
// admin user or the implicit user. A service client never is.
func (c Caller) IsAdmin() bool {
	return c.Kind == CallerUser && (c.Admin || c.Implicit)
}

// principal is the backend.Principal the access guard decides on for c. A
// caller of no known kind maps to no principal, which the guard refuses.
func (c Caller) principal() (backend.Principal, bool) {
	switch {
	case c.Kind == CallerClient && c.Name != "":
		return backend.ClientPrincipal(c.Name), true
	case c.Kind == CallerUser && c.Implicit:
		return backend.ImplicitPrincipal(), true
	case c.Kind == CallerUser && c.User != "":
		p := backend.UserPrincipal(c.User, c.Admin)
		p.Script = c.Script
		return p, true
	}
	return backend.Principal{}, false
}

// implicitCaller is the caller of open mode: the implicit user, an admin.
func implicitCaller() Caller {
	return Caller{Kind: CallerUser, Admin: true, Implicit: true}
}

// userCaller is the caller a user's certificate (subject cn, the fingerprint
// of key k) stands for.
func userCaller(u users.User, k users.Key, cn, serial string) Caller {
	c := Caller{Kind: CallerUser, Name: cn, User: u.Name, Admin: u.Admin, Implicit: u.Implicit, Script: k.Script, Serial: serial}
	if k.Source == users.SourceAdHoc {
		c.KeyLabel = k.Label
	}
	return c
}

type callerKey struct{}

// withCaller returns ctx carrying c and the backend.Principal it stands for.
func withCaller(ctx context.Context, c Caller) context.Context {
	ctx = context.WithValue(ctx, callerKey{}, c)
	if p, ok := c.principal(); ok {
		ctx = backend.WithPrincipal(ctx, p)
	}
	return ctx
}

// callerFrom returns the request's caller. A context with none (a handler
// reached without the auth middleware) is the zero Caller: no kind, no
// principal, so the access guard refuses everything it asks for.
func callerFrom(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Caller{}
}

// systemContext returns ctx carrying the server's own principal, for work the
// server does for itself (the reaper, the egress re-push, a profile's
// reference check), which must see every shed whoever asked.
func systemContext(ctx context.Context) context.Context {
	return backend.WithPrincipal(ctx, backend.SystemPrincipal())
}
