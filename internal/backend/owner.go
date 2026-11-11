package backend

import (
	"context"
	"fmt"

	"github.com/charliek/shed/internal/config"
)

type requiredOwnerKey struct{}

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
