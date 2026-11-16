package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
)

// UpgradeUser is a configured user as the start-up pass needs one.
type UpgradeUser struct {
	Name  string
	Admin bool
}

// UpgradeReport is what the start-up pass did and left.
type UpgradeReport struct {
	// StampedSheds and StampedSnapshots were given the one configured user.
	StampedSheds     []string
	StampedSnapshots []string
	// Unassigned are the sheds found with no user and no service client:
	// only an admin can view, stop and delete them, nobody can enter them,
	// until `shed assign` gives one to a user.
	Unassigned []string
	// Blocked are the Unassigned sheds that carry host directories or server
	// mounts: `shed assign` refuses them.
	Blocked []string
	// UnassignedSnapshots are the snapshots found with no user: an admin can
	// use and delete them; no command assigns one.
	UnassignedSnapshots []string
	// UnreadableSheds is how many shed records the backend's listing could
	// not read and left out (CountingLister); they are in no list above.
	UnreadableSheds int
	// Incomplete is set when the pass did not finish: a listing failed, a
	// stamp failed for a reason other than the shed or snapshot no longer
	// being one with no user, or ctx ended. The one-time upgrade step is
	// then not recorded as done (UsersUpgrade.MarkDone), and runs again at
	// the next start.
	Incomplete bool
}

// UpgradeLog receives the start-up pass's lines: Infof for what is always
// logged, Debugf for the names of unassigned sheds.
type UpgradeLog struct {
	Infof  func(format string, args ...any)
	Debugf func(format string, args ...any)
}

// CountingLister is implemented by a backend whose ListSheds leaves out a
// record it cannot read (Firecracker and VZ log and skip one). It lists as
// ListSheds does and also says how many records it left out, so the
// start-up report does not count them as having a user.
type CountingLister interface {
	ListShedsCounting(ctx context.Context) (sheds []config.Shed, unreadable int, err error)
}

// StampUnassigned is the start-up pass for sheds and snapshots made before
// the server had a user list. b is the raw backend (never a Guard); the pass
// acts as the server itself.
//
//   - With no user list (users empty) it does nothing at all.
//   - Only on the first start with a user list (firstStart: UsersUpgrade),
//     and only with exactly one configured user, every shed with no user and
//     no service client (as the backend reports it: a record reported
//     config.UserUnresolved belongs to someone and is never stamped) is given
//     that user, bound to the incarnation the listing found, and so is every
//     snapshot with no user, on a backend that keeps snapshots
//     (SnapshotStamper). This cannot be undone, so each one is logged. A
//     shed that carries host directories or server mounts is stamped only
//     when that user is an admin (an admin may use them anyway); otherwise
//     it is left unassigned.
//   - On any later start, and with several users, nothing is stamped: a
//     shed left with no user then needs `shed assign`. (So a server that
//     had several users and sheds left unassigned on purpose does not give
//     them all to the one user left when the others are removed.)
//
// It then logs how many sheds it found with no user and how many of those
// carry host resources (so `shed assign` refuses them), with their names at
// debug level, and how many records the backend could not read. A stamp that
// fails (a cloud label write, a record that cannot be saved) is logged and
// marks the pass Incomplete, so the step runs again at the next start; every
// stamp is idempotent.
func StampUnassigned(ctx context.Context, b Backend, users []UpgradeUser, firstStart bool, log UpgradeLog) UpgradeReport {
	var rep UpgradeReport
	if len(users) == 0 {
		return rep
	}
	if log.Infof == nil {
		log.Infof = func(string, ...any) {}
	}
	if log.Debugf == nil {
		log.Debugf = func(string, ...any) {}
	}
	ctx = WithPrincipal(ctx, SystemPrincipal())
	stamp := firstStart && len(users) == 1

	var sheds []config.Shed
	var err error
	if cl, ok := b.(CountingLister); ok {
		sheds, rep.UnreadableSheds, err = cl.ListShedsCounting(ctx)
	} else {
		sheds, err = b.ListSheds(ctx)
	}
	if err != nil {
		log.Infof("Users: cannot list sheds to find those with no user: %v", err)
		rep.Incomplete = true
		return rep
	}
	sort.Slice(sheds, func(i, j int) bool { return sheds[i].Name < sheds[j].Name })
	for i := range sheds {
		shed := &sheds[i]
		if shed.Owner != "" || shed.User != "" {
			continue
		}
		if stamp {
			u := users[0]
			req := config.AssignShedRequest{User: u.Name, AllowHostResources: u.Admin}
			if CheckAssignable(shed, req) == nil {
				_, err := b.AssignUser(WithRequiredCreatedAt(ctx, shed.CreatedAt), shed.Name, req)
				if err == nil {
					rep.StampedSheds = append(rep.StampedSheds, shed.Name)
					log.Infof("Users: shed %s had no user; it is now user %s's (the only configured user; this is not undone if users are added later)", shed.Name, u.Name)
					continue
				}
				if errors.Is(err, config.ErrShedAssignedSentinel) {
					// Since the listing it came to belong to someone (or its
					// records came to disagree): it is no longer a shed with
					// no user.
					log.Infof("Users: shed %s was found with no user but belongs to someone now; it was not given to user %s: %v", shed.Name, u.Name, err)
					continue
				}
				if !errors.Is(err, config.ErrShedNotFoundSentinel) {
					rep.Incomplete = true
				}
				log.Infof("Users: shed %s has no user and could not be given to user %s: %v", shed.Name, u.Name, err)
			}
		}
		rep.Unassigned = append(rep.Unassigned, shed.Name)
		if len(shed.HostResources()) > 0 {
			rep.Blocked = append(rep.Blocked, shed.Name)
		}
	}

	if stamper, ok := b.(SnapshotStamper); ok {
		snaps, err := b.ListSnapshots(ctx)
		if err != nil {
			log.Infof("Users: cannot list snapshots to find those with no user: %v", err)
			rep.Incomplete = true
		}
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].Name < snaps[j].Name })
		for _, snap := range snaps {
			if snap.User != "" {
				continue
			}
			if stamp {
				u := users[0]
				_, err := stamper.StampSnapshotUser(WithRequiredSnapshot(ctx, snap.CreatedAt, ""), snap.Name, u.Name)
				if err == nil {
					rep.StampedSnapshots = append(rep.StampedSnapshots, snap.Name)
					log.Infof("Users: snapshot %s had no user; it is now user %s's (the only configured user; this is not undone if users are added later)", snap.Name, u.Name)
					continue
				}
				if errors.Is(err, config.ErrShedAssignedSentinel) {
					log.Infof("Users: snapshot %s was found with no user but belongs to someone now; it was not given to user %s: %v", snap.Name, u.Name, err)
					continue
				}
				if !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
					rep.Incomplete = true
				}
				log.Infof("Users: snapshot %s has no user and could not be given to user %s: %v", snap.Name, u.Name, err)
			}
			rep.UnassignedSnapshots = append(rep.UnassignedSnapshots, snap.Name)
		}
	}
	if ctx.Err() != nil {
		rep.Incomplete = true
	}

	unreadable := ""
	if rep.UnreadableSheds > 0 {
		unreadable = fmt.Sprintf("; %d shed record(s) could not be read and are not counted", rep.UnreadableSheds)
	}
	switch {
	case len(rep.Unassigned) == 0:
		log.Infof("Users: found no shed without a user or a service client%s", unreadable)
	default:
		log.Infof("Users: found %d shed(s) with no user (%d of them carry host directories or server mounts and cannot be assigned until recreated without them)%s; an admin can view, stop and delete them, nobody can enter them, and 'shed assign <shed> <user>' gives one to a user",
			len(rep.Unassigned), len(rep.Blocked), unreadable)
		log.Debugf("Users: sheds with no user: %s", strings.Join(rep.Unassigned, ", "))
		if len(rep.Blocked) > 0 {
			log.Debugf("Users: sheds with no user that carry host resources: %s", strings.Join(rep.Blocked, ", "))
		}
	}
	if len(rep.UnassignedSnapshots) > 0 {
		log.Infof("Users: found %d snapshot(s) with no user; only an admin can use or delete them", len(rep.UnassignedSnapshots))
		log.Debugf("Users: snapshots with no user: %s", strings.Join(rep.UnassignedSnapshots, ", "))
	}
	return rep
}

// UsersUpgradeFile is the marker, in the server's state directory, that the
// one-time upgrade step for a user list has run (UsersUpgrade).
const UsersUpgradeFile = "users-upgrade.json"

// usersUpgradeVersion is the marker's schema version.
const usersUpgradeVersion = 1

// usersUpgradeMarker is the marker file's content.
type usersUpgradeMarker struct {
	Version     int       `json:"version"`
	CompletedAt time.Time `json:"completed_at"`
}

// UsersUpgrade is the decision, made once per start, whether this is the
// server's first start with a user list: the one start on which the start-up
// pass may give sheds and snapshots with no user to the one configured user
// (StampUnassigned). Other one-time steps of adding a user list read the
// same decision (FirstUserListStart).
type UsersUpgrade struct {
	path  string
	first bool
}

// OpenUsersUpgrade reads the marker <stateDir>/users-upgrade.json and decides
// whether this is the first start with a user list:
//
//   - with no user list (userList false) it is not, and a marker left by an
//     earlier start is removed, so a server that goes back to no user list
//     and later adds one takes the step again (the way out for an operator
//     who started with several users by mistake);
//   - with a user list and no marker (no directory entry at all) it is;
//   - with a user list and a marker it is not. A marker that cannot be read,
//     is not a regular file (a dangling symlink, a directory, a special
//     file), does not parse, or has a schema version this build does not
//     know is taken as "already done", never as "run again", and is logged.
//
// Every outcome is logged with logf.
func OpenUsersUpgrade(stateDir string, userList bool, logf func(format string, args ...any)) *UsersUpgrade {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	u := &UsersUpgrade{path: filepath.Join(stateDir, UsersUpgradeFile)}
	if !userList {
		switch err := os.Remove(u.path); {
		case err == nil:
			if err := syncDir(stateDir); err != nil {
				logf("Users: removed the user-list upgrade marker %s, but syncing its directory failed: %v", u.path, err)
			}
			logf("Users: no user list: removed the user-list upgrade marker %s; the next start with a user list runs the one-time upgrade step again", u.path)
		case !errors.Is(err, os.ErrNotExist):
			logf("Users: no user list, but the user-list upgrade marker %s could not be removed: %v", u.path, err)
		}
		return u
	}
	// The directory entry itself decides "absent": an entry of any kind (a
	// symlink whose target is gone, a directory, a special file) is a
	// marker that is present, and is never read as a first start.
	fi, err := os.Lstat(u.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		u.first = true
		logf("Users: first start with a user list (no marker at %s): running the one-time upgrade step", u.path)
		return u
	case err != nil:
		logf("Users: the user-list upgrade marker %s cannot be read (%v); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", u.path, err)
		return u
	case !fi.Mode().IsRegular():
		logf("Users: the user-list upgrade marker %s is not a regular file (%s); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", u.path, fi.Mode().Type())
		return u
	}
	data, err := os.ReadFile(u.path)
	if err != nil {
		logf("Users: the user-list upgrade marker %s cannot be read (%v); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", u.path, err)
		return u
	}
	var m usersUpgradeMarker
	switch err := json.Unmarshal(data, &m); {
	case err != nil:
		logf("Users: the user-list upgrade marker %s is damaged (%v); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", u.path, err)
	case m.Version != usersUpgradeVersion:
		logf("Users: the user-list upgrade marker %s has schema version %d, not %d; the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", u.path, m.Version, usersUpgradeVersion)
	default:
		logf("Users: the one-time upgrade step for the user list ran at %s (marker %s); sheds with no user are not given one automatically, use 'shed assign'", m.CompletedAt.Format(time.RFC3339), u.path)
	}
	return u
}

// FirstUserListStart reports whether this start is the server's first with a
// user list (see OpenUsersUpgrade). It does not change when MarkDone writes
// the marker.
func (u *UsersUpgrade) FirstUserListStart() bool { return u.first }

// MarkDone records, durably, that the one-time upgrade step has run: the
// marker is written to a temporary file that is synced, renamed into place,
// and its directory synced. On an error the step is not recorded and runs
// again at the next start. It does nothing unless this is the first start
// with a user list.
func (u *UsersUpgrade) MarkDone(now time.Time) error {
	if !u.first {
		return nil
	}
	data, err := json.MarshalIndent(usersUpgradeMarker{Version: usersUpgradeVersion, CompletedAt: now.UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(u.path, append(data, '\n'))
}

// writeFileDurable writes data to path, mode 0600, so that once it returns nil
// the content survives a crash: a temporary file in the same directory is
// written, synced and renamed over path, and the directory is synced.
func writeFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return syncDir(dir)
}

// syncDir syncs a directory, so a rename or removal in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
