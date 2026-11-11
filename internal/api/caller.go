package api

import (
	"context"

	"github.com/charliek/shed/internal/config"
)

// Caller kinds.
const (
	// CallerAdmin is every caller that is not a service client: open mode,
	// a bearer token, or a user's client certificate. Admins see and control
	// every shed, exactly as before service clients existed.
	CallerAdmin = "admin"
	// CallerClient is a service client (auth.clients) authenticated by its
	// service certificate. It reaches only the client route allowlist and
	// only the sheds it owns.
	CallerClient = "client"
)

// Caller is the authenticated identity of one API request, set in the request
// context by the auth middleware.
type Caller struct {
	// Kind is CallerAdmin or CallerClient.
	Kind string
	// Name is the client name for a service client, or the admin subject
	// (a token's subject or a user certificate's CN; empty in open mode).
	Name string
	// Serial is the client certificate's serial (lowercase hex), when the
	// request carried one.
	Serial string
	// Client is the service client's config (nil for an admin).
	Client *config.ServiceClientConfig
}

// IsClient reports whether the caller is a service client.
func (c Caller) IsClient() bool { return c.Kind == CallerClient }

type callerKey struct{}

// withCaller returns ctx carrying c.
func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// callerFrom returns the request's caller. A context with none (a handler
// reached without the auth middleware, as some unit tests do) is an admin:
// the middleware sets a client caller on every client request before any
// handler runs, so an absent caller is never a client's.
func callerFrom(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Caller{Kind: CallerAdmin}
}
