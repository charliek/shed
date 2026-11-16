package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/charliek/shed/internal/config"
)

type requiredOwnerKey struct{}

type requiredUserKey struct{}

type requiredCreatedAtKey struct{}

// WithRequiredOwner returns ctx carrying the owner the shed an operation acts
// on must have. The API server sets it for a service client's request on a
// shed it owns, so a backend can check ownership against the shed it actually
// loads, under its own per-shed lock, rather than trusting a lookup made
// before the operation (a shed deleted and recreated under the same name by
// another caller in between is not the client's).
func WithRequiredOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, requiredOwnerKey{}, owner)
}

// CheckOwner returns a shed-not-found error for name when ctx requires an
// owner (WithRequiredOwner) and owner is not it, and nil otherwise. Not found,
// never forbidden: a client must not learn that someone else's shed exists.
func CheckOwner(ctx context.Context, name, owner string) error {
	want, ok := ctx.Value(requiredOwnerKey{}).(string)
	if !ok || owner == want {
		return nil
	}
	return fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
}

// requiredOwner returns the owner ctx requires (WithRequiredOwner), if any.
func requiredOwner(ctx context.Context) (string, bool) {
	o, ok := ctx.Value(requiredOwnerKey{}).(string)
	return o, ok
}

// WithRequiredUser returns ctx carrying the user the shed an operation acts
// on must be reported with: the user the access guard authorized the caller
// against. A backend checks it (CheckIdentity) under its per-shed lock, so a
// shed whose reported user changed between the guard's check and the action
// (on the remote backend, a reconcile that finds its user label in conflict
// and reports it config.UserUnresolved, making it admin-only) is not acted on
// for a caller who was authorized as its old user.
func WithRequiredUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, requiredUserKey{}, user)
}

// RequiredUser returns the user ctx requires of the shed an operation acts on
// (WithRequiredUser), if any.
func RequiredUser(ctx context.Context) (string, bool) {
	u, ok := ctx.Value(requiredUserKey{}).(string)
	return u, ok
}

// CheckUser returns a shed-not-found error for name when ctx requires a user
// (WithRequiredUser) and user (the shed's reported user, never the raw stored
// one) is not it, and nil otherwise: the answer an incarnation that changed
// gets.
func CheckUser(ctx context.Context, name, user string) error {
	want, ok := RequiredUser(ctx)
	if !ok || user == want {
		return nil
	}
	return fmt.Errorf("%w: %s (its owner changed since it was looked up)", config.ErrShedNotFoundSentinel, name)
}

// PinOwnership returns ctx requiring the reported owner (WithRequiredOwner)
// and user (WithRequiredUser) the access guard authorized a caller against,
// each unless ctx already requires one, which is never replaced (the reaper
// and a service client's request require the owner they looked up; the
// caller checks the shed it loaded against those).
func PinOwnership(ctx context.Context, owner, user string) context.Context {
	if _, ok := requiredOwner(ctx); !ok {
		ctx = WithRequiredOwner(ctx, owner)
	}
	if _, ok := RequiredUser(ctx); !ok {
		ctx = WithRequiredUser(ctx, user)
	}
	return ctx
}

// WithRequiredCreatedAt returns ctx carrying the creation time the shed an
// operation acts on must have: its incarnation. The max_age reaper sets it
// (with the owner) from the listing that found the shed expired, so a stop or
// delete refuses a shed deleted and recreated under the same name, by anyone,
// since that listing: the new shed is young, and not the one that expired.
func WithRequiredCreatedAt(ctx context.Context, createdAt time.Time) context.Context {
	return context.WithValue(ctx, requiredCreatedAtKey{}, createdAt)
}

// RequiredCreatedAt returns the creation time ctx requires of the shed an
// operation acts on (WithRequiredCreatedAt), if any.
func RequiredCreatedAt(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(requiredCreatedAtKey{}).(time.Time)
	return t, ok
}

// PinCreatedAt returns ctx requiring createdAt (WithRequiredCreatedAt) unless
// ctx already requires a creation time, which is never replaced: what follows
// acts on the shed that was looked up, never on one created again under its
// name.
func PinCreatedAt(ctx context.Context, createdAt time.Time) context.Context {
	if _, ok := RequiredCreatedAt(ctx); ok {
		return ctx
	}
	return WithRequiredCreatedAt(ctx, createdAt)
}

// CheckCreatedAt returns a shed-not-found error for name when ctx requires a
// creation time (WithRequiredCreatedAt) and createdAt is not it, and nil
// otherwise. A backend calls it with CheckOwner, under its per-shed lock,
// against the shed it loaded.
func CheckCreatedAt(ctx context.Context, name string, createdAt time.Time) error {
	want, ok := RequiredCreatedAt(ctx)
	if !ok || createdAt.Equal(want) {
		return nil
	}
	return fmt.Errorf("%w: %s (recreated since it was listed)", config.ErrShedNotFoundSentinel, name)
}

// CheckIdentity applies CheckOwner, CheckUser and then CheckCreatedAt to a
// shed whose reported service client is owner and reported user is user
// (config.ShedOwnership, never the raw stored fields) and whose creation time
// is createdAt: the incarnation and the ownership the access guard authorized
// the caller against. A backend calls it under its per-shed lock, against the
// shed it loaded. Each mismatch is answered as a shed that is not there.
func CheckIdentity(ctx context.Context, name, owner, user string, createdAt time.Time) error {
	if err := CheckOwner(ctx, name, owner); err != nil {
		return err
	}
	if err := CheckUser(ctx, name, user); err != nil {
		return err
	}
	return CheckCreatedAt(ctx, name, createdAt)
}
