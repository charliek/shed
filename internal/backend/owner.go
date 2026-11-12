package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/charliek/shed/internal/config"
)

type requiredOwnerKey struct{}

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

// WithRequiredCreatedAt returns ctx carrying the creation time the shed an
// operation acts on must have: its incarnation. The max_age reaper sets it
// (with the owner) from the listing that found the shed expired, so a stop or
// delete refuses a shed deleted and recreated under the same name, by anyone,
// since that listing: the new shed is young, and not the one that expired.
func WithRequiredCreatedAt(ctx context.Context, createdAt time.Time) context.Context {
	return context.WithValue(ctx, requiredCreatedAtKey{}, createdAt)
}

// CheckCreatedAt returns a shed-not-found error for name when ctx requires a
// creation time (WithRequiredCreatedAt) and createdAt is not it, and nil
// otherwise. A backend calls it with CheckOwner, under its per-shed lock,
// against the shed it loaded.
func CheckCreatedAt(ctx context.Context, name string, createdAt time.Time) error {
	want, ok := ctx.Value(requiredCreatedAtKey{}).(time.Time)
	if !ok || createdAt.Equal(want) {
		return nil
	}
	return fmt.Errorf("%w: %s (recreated since it was listed)", config.ErrShedNotFoundSentinel, name)
}
