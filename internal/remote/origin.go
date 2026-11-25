package remote

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/remote/state"
)

// originShed is r as the origin migration knows it.
func originShed(r *state.Record) backend.OriginShed {
	_, user := ownership(r)
	return backend.OriginShed{Name: r.Shed.Name, ID: r.Shed.ID, CreatedAt: r.Shed.CreatedAt, User: user}
}

// UnstampedSheds lists the records with no user origin
// (backend.OriginStamper).
func (b *Backend) UnstampedSheds(context.Context) ([]backend.OriginShed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []backend.OriginShed
	for _, r := range b.records {
		if r.UserOrigin == "" {
			out = append(out, originShed(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// StampUserOrigins gives every record with no user origin the origin decide
// returns for it (backend.OriginStamper). It runs once the first cloud list
// has rebuilt the records. A record counts as stamped only when its save was
// durable: otherwise the record in memory keeps no origin (so the shed is
// refused deliveries, failing closed), the file is put back as it was when
// the write landed without its flush, the failure is named in the error, and
// the next start stamps it from the recorded decision.
func (b *Backend) StampUserOrigins(ctx context.Context, decide func(backend.OriginShed) (string, bool)) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.records))
	for name := range b.records {
		names = append(names, name)
	}
	sort.Strings(names)
	var stamped []string
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return stamped, errors.Join(append(errs, err)...)
		}
		r := b.records[name]
		if r == nil || r.UserOrigin != "" {
			continue
		}
		origin, ok := decide(originShed(r))
		if !ok {
			continue
		}
		cp := r.Clone()
		cp.UserOrigin = origin
		if err := b.saveRecord(cp); err != nil {
			if errors.Is(err, state.ErrNotDurable) {
				// In place but not durable: put the record back without its
				// origin, so neither this process nor a restart trusts it.
				_ = b.saveRecord(r)
			}
			b.logf("remote: shed %s: where its user came from could not be recorded (%v); it gets no delivery until a start records it", name, err)
			errs = append(errs, fmt.Errorf("shed %s: %w", name, err))
			continue
		}
		*r = *cp
		stamped = append(stamped, name)
	}
	return stamped, errors.Join(errs...)
}
