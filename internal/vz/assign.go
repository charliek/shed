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

// AssignUser gives a shed with no user and no service client to req.User (see
// backend.Backend.AssignUser). Under the per-shed lock it loads the record,
// checks it is the incarnation the caller looked up (checkIdentity) and that
// it may be assigned (backend.CheckAssignable, on the record as it is
// reported), saves the user durably (Metadata.SaveDurable), and only then moves a running shed's
// credential-bus route to the new user (CredentialManager.SetShedUser), still
// under the lock so a start or the resume walk cannot register the old user
// after it.
func (c *Client) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	if err := backend.ValidateAssignee(req); err != nil {
		return nil, err
	}
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}
	if err := checkIdentity(ctx, name, meta); err != nil {
		return nil, err
	}
	if err := backend.CheckAssignable(metadataToShed(meta, ""), req); err != nil {
		return nil, err
	}
	meta.User = req.User
	// Durable before it is acknowledged and before the bus route moves: an
	// assignment cannot be undone, so one lost to a power loss could later
	// be given to someone else. A failed sync fails the assignment and
	// leaves the route alone.
	if err := meta.SaveDurable(c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("assign %s: save metadata: %w", name, err)
	}
	if c.credMgr != nil && c.credMgr.SetShedUser(name, meta.CreatedAt, req.User) {
		log.Printf("[%s] now routes credential requests as user %q", name, req.User)
	}
	// Read back as what it now is: the user it was bound to (the guard's
	// WithRequiredUser) is the one this call has just replaced.
	return c.getShedLocked(backend.WithRequiredUser(ctx, req.User), name)
}

// StampSnapshotUser records user on a snapshot with none (see
// backend.SnapshotStamper), under the snapshot lock and checked against the
// snapshot the caller looked up. The schema version the snapshot was written
// with is kept: only the user changes.
func (c *Client) StampSnapshotUser(ctx context.Context, name, user string) (*config.Snapshot, error) {
	if err := backend.ValidateAssignee(config.AssignShedRequest{User: user}); err != nil {
		return nil, err
	}
	if err := config.ValidateSnapshotName(name); err != nil {
		return nil, err
	}
	defer c.acquireSnapshotLock(name)()

	snap, err := loadSnapshot(c.cfg.SnapshotsDir, name)
	if err != nil {
		return nil, err
	}
	if err := backend.CheckSnapshot(ctx, name, snap); err != nil {
		return nil, err
	}
	if snap.User != "" {
		return nil, fmt.Errorf("%w: snapshot %s already belongs to user %s", config.ErrShedAssignedSentinel, name, snap.User)
	}
	snap.User = user
	if err := writeSnapshotDurable(c.cfg.SnapshotsDir, snap); err != nil {
		return nil, fmt.Errorf("stamp snapshot %s: %w", name, err)
	}
	c.augmentSnapshot(snap)
	return snap, nil
}
