package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

// Test hooks (nil in production): assignVerifiedHook runs in AssignUser after
// the instance is read and verified and before the shed_user label is
// claimed (tests: another server's assignment in that window);
// assignLabelledHook after the label is claimed and before the record is
// changed (tests: a delete, a restart or a failing record write there).
var (
	assignVerifiedHook func(name string)
	assignLabelledHook func(name string)
)

// AssignUser gives a shed with no user and no service client to req.User (see
// backend.Backend.AssignUser).
//
// The user is kept in two places, the instance's shed_user label and the
// local record, and the label is written first. The label is the copy that
// survives: a record with no user and no owner takes the user its instance
// is labelled with at the next read of the instance (reconcileUserLocked: the
// start-up pass, the periodic pass, every job), and a shed whose record is
// lost is adopted with it.
//
// The label is claimed (provider.Provider.ClaimLabel): written only while it
// is empty or already names req.User, checked by the cloud write itself, so
// two servers on one environment (or a stale one) cannot both give the shed
// to different users. A label that names someone else refuses the
// assignment as already assigned, with the record untouched. So:
//
//   - the label claim fails: the record is not changed and the error is
//     returned. The label may still have been written (a write whose answer
//     was lost); if so, the next read of the instance gives the record that
//     user, a new assignment to the same user goes through without writing
//     again, and one to another user is refused;
//   - the label is written and the record then cannot be saved (the server
//     dies, or the write fails): the record in memory, when there is one,
//     has the user, and the next start takes it from the label and stores
//     it. Nothing is ever recorded that the label does not also say.
//
// The record is changed only if it is still the incarnation that was looked
// up (by shed ID) and still unassigned. A running shed's bus route follows
// the new user before AssignUser returns: the change holds the route at
// once (busUserChangedLocked) and AssignUser then applies the user itself
// and lifts the hold (applyBusRoute), without waiting for the shed's
// bus-route worker.
func (b *Backend) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	if err := backend.ValidateAssignee(req); err != nil {
		return nil, err
	}
	b.assignMu.Lock()
	defer b.assignMu.Unlock()

	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if err := backend.CheckAssignable(b.shedOf(r), req); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	id, ref := r.Shed.ID, r.Ref
	b.mu.Unlock()
	if ref == "" {
		return nil, fmt.Errorf("shed %s has no cloud instance yet; assign it once its create has finished", name)
	}

	// The label first, on the instance verified to be this shed's.
	if _, err := b.verified(ctx, ref, id); err != nil {
		return nil, fmt.Errorf("assign %s: reading its instance: %w", name, mapRefusal(err))
	}
	if assignVerifiedHook != nil {
		assignVerifiedHook(name)
	}
	if err := b.prov.ClaimLabel(ctx, ref, provider.LabelUser, req.User); err != nil {
		if errors.Is(err, provider.ErrLabelTaken) {
			return nil, fmt.Errorf("%w: the instance of shed %s is labelled for another user: %w", config.ErrShedAssignedSentinel, name, err)
		}
		return nil, fmt.Errorf("assign %s: writing its %s label: %w", name, provider.LabelUser, mapRefusal(err))
	}
	if assignLabelledHook != nil {
		assignLabelledHook(name)
	}

	b.mu.Lock()
	r = b.currentLocked(name, id)
	if r == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s (deleted while it was being assigned)", config.ErrShedNotFoundSentinel, name)
	}
	moved := false
	if _, user := ownership(r); user != req.User {
		// A read of the instance may have taken the label's user already.
		if err := backend.CheckAssignable(b.shedOf(r), req); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		r.Shed.User = req.User
		b.saveLocked(r)
		b.busUserChangedLocked(r)
		moved = true
	}
	shed := b.shedOf(r)
	b.mu.Unlock()
	if moved {
		b.applyBusRoute(name)
	}
	b.logf("remote: shed %s assigned to user %s", name, req.User)
	return shed, nil
}
