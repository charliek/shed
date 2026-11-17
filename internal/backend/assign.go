package backend

import (
	"context"
	"fmt"
	"strings"

	"github.com/charliek/shed/internal/config"
)

// This file holds what gives a user to a shed made before its server had a
// user list: the rule a backend applies under its lock (CheckAssignable). The
// guarded entry point is Guard.AssignUser (guard.go); the server's start-up
// pass is StampUnassigned (upgrade.go).

// CheckAssignable returns nil when shed, as its backend reports it
// (config.ShedOwnership: never the raw stored fields), may be given to the
// user req.User, and the refusal otherwise:
//
//   - a shed with a service client (Owner), a user, or records that disagree
//     about whose it is (config.UserUnresolved) belongs to someone already:
//     config.ErrShedAssignedSentinel. A shed is never moved between users;
//   - a shed that carries host directories or server mounts
//     (config.Shed.HostResources) is refused with
//     config.ErrShedHostResourcesSentinel unless req.AllowHostResources:
//     its user would be handed the host's directories and the server's
//     credentials.
//
// A backend calls it under its per-shed lock, against the shed it is about to
// change.
func CheckAssignable(shed *config.Shed, req config.AssignShedRequest) error {
	switch {
	case shed.Owner != "":
		return fmt.Errorf("%w: shed %s belongs to a service client; it cannot be assigned to a user", config.ErrShedAssignedSentinel, shed.Name)
	case shed.User == config.UserUnresolved:
		return fmt.Errorf("%w: the records of shed %s disagree about whose it is; it cannot be assigned until they agree", config.ErrShedAssignedSentinel, shed.Name)
	case shed.User == req.User:
		return fmt.Errorf("%w: shed %s already belongs to user %s", config.ErrShedAssignedSentinel, shed.Name, shed.User)
	case shed.User != "":
		return fmt.Errorf("%w: shed %s already belongs to user %s; a shed cannot be moved between users", config.ErrShedAssignedSentinel, shed.Name, shed.User)
	}
	if req.AllowHostResources {
		return nil
	}
	if what := shed.HostResources(); len(what) > 0 {
		return fmt.Errorf("%w: shed %s carries %s, which assigning it would hand to user %s; recreate it without them (no --local-dir or --add-dir, and --mounts none) and assign the new shed instead",
			config.ErrShedHostResourcesSentinel, shed.Name, strings.Join(what, " and "), req.User)
	}
	return nil
}

// ValidateAssignee refuses an assignment whose req.User cannot name a user
// (the implicit user's "" included). A backend's AssignUser calls it first.
func ValidateAssignee(req config.AssignShedRequest) error {
	if !config.ValidUserName(req.User) {
		return fmt.Errorf("%w: %q is not a valid user name", config.ErrInvalidShedRequestSentinel, req.User)
	}
	return nil
}

// SnapshotStamper is implemented by a backend that keeps snapshots (Firecracker
// and VZ). It is not part of Backend and the Guard does not offer it: a
// snapshot cannot be assigned through the API in this cut. Only the server's
// start-up pass on a server with exactly one user calls it, on the raw
// backend.
type SnapshotStamper interface {
	// StampSnapshotUser records user as the user of the snapshot name, which
	// must have none, under the snapshot lock and checked against the
	// snapshot its context requires (WithRequiredSnapshot). A snapshot with
	// a user is refused with config.ErrShedAssignedSentinel.
	StampSnapshotUser(ctx context.Context, name, user string) (*config.Snapshot, error)
}
