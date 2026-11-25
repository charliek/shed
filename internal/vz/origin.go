//go:build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// originShed is meta as the origin migration knows it.
func originShed(meta *Metadata) backend.OriginShed {
	_, user := config.ShedOwnership(meta.Owner, meta.User)
	return backend.OriginShed{Name: meta.Name, ID: meta.ID, CreatedAt: meta.CreatedAt, User: user}
}

// UnstampedSheds lists the sheds whose metadata has no user origin
// (backend.OriginStamper).
func (c *Client) UnstampedSheds(ctx context.Context) ([]backend.OriginShed, error) {
	names, _, err := listInstancesCounting(c.cfg.InstanceDir)
	if err != nil {
		return nil, err
	}
	var out []backend.OriginShed
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meta, err := LoadMetadata(c.cfg.InstanceDir, name)
		if err != nil {
			continue // unreadable: it gets no delivery either
		}
		if meta.UserOrigin == "" {
			out = append(out, originShed(meta))
		}
	}
	return out, nil
}

// StampUserOrigins gives every shed whose metadata has no user origin the
// origin decide returns for it (backend.OriginStamper), each under its
// lifecycle lock and saved durably.
func (c *Client) StampUserOrigins(ctx context.Context, decide func(backend.OriginShed) (string, bool)) ([]string, error) {
	names, _, err := listInstancesCounting(c.cfg.InstanceDir)
	if err != nil {
		return nil, err
	}
	var stamped []string
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return stamped, errors.Join(append(errs, err)...)
		}
		did, err := c.stampUserOrigin(name, decide)
		if err != nil {
			log.Printf("Warning: shed %q: where its user came from could not be recorded: %v; it gets no delivery until a start records it", name, err)
			errs = append(errs, fmt.Errorf("shed %s: %w", name, err))
			continue
		}
		if did {
			stamped = append(stamped, name)
		}
	}
	return stamped, errors.Join(errs...)
}

func (c *Client) stampUserOrigin(name string, decide func(backend.OriginShed) (string, bool)) (bool, error) {
	defer c.acquireCreateLock(name)()
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil || meta.UserOrigin != "" {
		return false, err
	}
	origin, ok := decide(originShed(meta))
	if !ok {
		return false, nil
	}
	meta.UserOrigin = origin
	if err := meta.SaveDurable(c.cfg.InstanceDir); err != nil {
		// Not durable, perhaps in place: put it back without its origin, so
		// neither this process nor a restart trusts it (it fails closed
		// until a start records it, from the recorded decision).
		meta.UserOrigin = ""
		_ = meta.Save(c.cfg.InstanceDir)
		return false, err
	}
	return true, nil
}

// UnstampedSheds is the client's (backend.OriginStamper).
func (b *VZBackend) UnstampedSheds(ctx context.Context) ([]backend.OriginShed, error) {
	return b.client.UnstampedSheds(ctx)
}

// StampUserOrigins is the client's (backend.OriginStamper).
func (b *VZBackend) StampUserOrigins(ctx context.Context, decide func(backend.OriginShed) (string, bool)) ([]string, error) {
	return b.client.StampUserOrigins(ctx, decide)
}
