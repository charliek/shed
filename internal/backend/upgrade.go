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
	// UnreadableSnapshots is the same for snapshot records
	// (CountingSnapshotLister).
	UnreadableSnapshots int
	// Incomplete is set when the pass did not finish: a listing failed, a
	// stamp failed for a reason other than the shed or snapshot no longer
	// being one with no user, ctx ended, or (when it stamps) a listing left
	// out a record it could not read. The one-time upgrade step then stays
	// pending for the same user (UsersUpgrade.Finish).
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

// CountingSnapshotLister is CountingLister for snapshots: implemented by a
// backend whose ListSnapshots leaves out a record it cannot read (Firecracker
// and VZ log and skip one).
type CountingSnapshotLister interface {
	ListSnapshotsCounting(ctx context.Context) (snaps []config.Snapshot, unreadable int, err error)
}

// StampUnassigned is the start-up pass for sheds and snapshots made before
// the server had a user list. b is the raw backend (never a Guard); the pass
// acts as the server itself.
//
//   - With no user list (users empty) it does nothing at all.
//   - With firstStart and exactly one user (the caller passes the user of
//     UsersUpgrade.Begin's plan, StampFor, alone: on the first start with a
//     user list, or resuming a step begun for that same user), every shed
//     with no user and no service client (as the backend reports it: a
//     record reported config.UserUnresolved belongs to someone and is never
//     stamped) is given that user, bound to the incarnation the listing
//     found, and so is every snapshot with no user, on a backend that keeps
//     snapshots (SnapshotStamper). This cannot be undone, so each one is
//     logged. A shed that carries host directories or server mounts is
//     stamped only when that user is an admin (an admin may use them
//     anyway); otherwise it is left unassigned.
//   - Otherwise nothing is stamped: a shed left with no user then needs
//     `shed assign`. (So a server that had several users and sheds left
//     unassigned on purpose does not give them all to the one user left
//     when the others are removed.)
//
// It then logs how many sheds it found with no user and how many of those
// carry host resources (so `shed assign` refuses them), with their names at
// debug level, and how many records the backend could not read. A stamp that
// fails (a cloud label write, a record that cannot be saved) is logged and
// marks the pass Incomplete; every stamp is idempotent.
//
// When it stamps, a shed or snapshot record that the listing could not read
// also marks the pass Incomplete: that record may be one with no user, which
// this pass would have given the user, and which, once readable again,
// nothing else would (and `shed assign` refuses a shed that carries host
// resources, as most sheds made before users existed do). An Incomplete pass
// leaves the step pending for the same user (UsersUpgrade.Finish), and a
// later start resumes it only while that user is still the only configured
// user (UsersUpgrade.Begin): the recipient of a retry is the one recorded
// before the first stamp, never the configuration of the day.
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
		var snaps []config.Snapshot
		var err error
		if cl, ok := b.(CountingSnapshotLister); ok {
			snaps, rep.UnreadableSnapshots, err = cl.ListSnapshotsCounting(ctx)
		} else {
			snaps, err = b.ListSnapshots(ctx)
		}
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
	if stamp && (rep.UnreadableSheds > 0 || rep.UnreadableSnapshots > 0) {
		rep.Incomplete = true
		log.Infof("Users: %d shed record(s) and %d snapshot record(s) could not be read, and any of them may have no user; the one-time upgrade step is not finished and resumes at the next start, to give them to user %s once they can be read, while that user is still the only configured user",
			rep.UnreadableSheds, rep.UnreadableSnapshots, users[0].Name)
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
	if rep.UnreadableSnapshots > 0 {
		log.Infof("Users: %d snapshot record(s) could not be read and are not counted", rep.UnreadableSnapshots)
	}
	if len(rep.UnassignedSnapshots) > 0 {
		log.Infof("Users: found %d snapshot(s) with no user; only an admin can use or delete them", len(rep.UnassignedSnapshots))
		log.Debugf("Users: snapshots with no user: %s", strings.Join(rep.UnassignedSnapshots, ", "))
	}
	return rep
}

// UsersUpgradeFile is the marker, in the server's state directory, of the
// one-time upgrade step for a user list (UsersUpgrade): either the step is
// done, or it has begun for one named user and is not finished (pending).
const UsersUpgradeFile = "users-upgrade.json"

// usersUpgradeVersion is the marker's schema version. The pending form is an
// additive field under the same version: a marker from before it (no
// "pending") reads as done, and a binary from before it reads a pending
// marker as done too (it sees version 1), which never stamps anything.
const usersUpgradeVersion = 1

// usersUpgradeMarker is the marker file's content. With Pending unset the
// step is done (CompletedAt says when); with Pending set it has begun for
// that user and is not finished.
type usersUpgradeMarker struct {
	Version     int                  `json:"version"`
	CompletedAt time.Time            `json:"completed_at,omitzero"`
	Pending     *usersUpgradePending `json:"pending,omitempty"`
}

// usersUpgradePending is the decision of a step that has begun: the one user
// it gives sheds and snapshots with no user to, and whether that user was an
// admin then (so a shed carrying host resources may be stamped).
type usersUpgradePending struct {
	User      string    `json:"user"`
	Admin     bool      `json:"admin"`
	StartedAt time.Time `json:"started_at"`
}

// upgradeState is what the marker said when it was opened.
type upgradeState int

const (
	// upgradeNoList: no user list; the marker was removed.
	upgradeNoList upgradeState = iota
	// upgradeFirst: a user list and no marker: the first start with one.
	upgradeFirst
	// upgradePending: the step began for one user and did not finish.
	upgradePending
	// upgradeDone: the step is done, or the marker cannot be read.
	upgradeDone
)

// UsersUpgrade is the one-time upgrade step for adding a user list, as the
// marker in the state directory records it. On the first start with a user
// list (no marker), with exactly one configured user, the start-up pass gives
// sheds and snapshots with no user to that user (StampUnassigned). The step
// is recorded as begun for that user (a pending marker naming them) before
// anything is given to anyone, and as done once a pass finishes; a later
// start resumes a pending step only while the same user is still the one
// configured user, so a retry never changes the recipient. With other users
// the start is refused (UsersUpgradeChangedError) unless the operator
// abandons the step on purpose (AbandonUsersUpgradeFlag). Other one-time
// steps of adding a user list (the credential source bindings) follow the
// same decision (Begin's UpgradePlan).
type UsersUpgrade struct {
	path    string
	state   upgradeState
	pending UpgradeUser
	logf    func(format string, args ...any)
}

// AbandonUsersUpgradeFlag is the `shed-server serve` flag that abandons, on
// purpose and at one start, an unfinished one-time upgrade step whose users
// have changed since it began. Without such a step the flag is refused
// (UsersUpgradeAbandonError), so it cannot be left on a service's command
// line as a habit.
const AbandonUsersUpgradeFlag = "--abandon-user-upgrade"

// UsersUpgradeChangedError refuses a start: the one-time upgrade step began
// for Pending and did not finish, and the configured users are no longer
// Pending alone. Starting would end the step (what it has not reached keeps
// no user, and a shed that carries host resources can then never be
// assigned), so the server stops before anything is stamped, adopted or
// recorded, and says the two ways forward. Its commands hold nothing read
// from the config or the state directory: the names it gives are in its
// sentences only.
type UsersUpgradeChangedError struct {
	// Pending is the user the step began for.
	Pending UpgradeUser
	// Users are the configured users now (auth.users: beside auth.directory,
	// the local admins).
	Users []UpgradeUser
	// Marker is the marker's path.
	Marker string
}

func (e *UsersUpgradeChangedError) Error() string {
	return fmt.Sprintf(`the server does not start: the one-time upgrade step for the user list began for user %[1]s and did not finish, and the users in auth.users are now %[2]s (marker %[3]q).
Starting now would end the step: the sheds and snapshots it has not reached would keep no user, so only an admin could use them, and a shed among them that carries host directories or server mounts could never be given to a user, only recreated.
Either finish the step: put user %[1]s back as the only user in auth.users (beside auth.directory: as the only local admin) and start the server again as before (restart its service, or run it with its usual command line), until its log says the one-time upgrade step for the user list is done and recorded; change the users only then:
    shed-server serve --config /path/to/server.yaml
Or abandon it on purpose: stop the server's service and start the server once by hand, as the same system user and with the same --config, with %[4]s added; once it has started, stop it and start the service again, without the flag:
    shed-server serve --config /path/to/server.yaml %[4]s
Then the sheds and snapshots still with no user stay admin-only, and 'shed assign' gives such a shed to a user.`,
		e.Pending.Name, describeUsers(e.Users), e.Marker, AbandonUsersUpgradeFlag)
}

// UsersUpgradeAbandonError refuses a start with AbandonUsersUpgradeFlag when
// there is no unfinished step whose users changed: Reason says what the
// marker holds instead.
type UsersUpgradeAbandonError struct {
	Reason string
}

func (e *UsersUpgradeAbandonError) Error() string {
	return fmt.Sprintf("%s is refused: %s, so there is nothing to abandon; start the server without it (the flag is for one start, never for a service's command line)",
		AbandonUsersUpgradeFlag, e.Reason)
}

// CheckUsersUpgrade is the start-up check of the one-time upgrade step, made
// before the server opens or changes anything: it reads the marker in
// stateDir and never writes, removes or logs. users are the configured users
// as Begin takes them (nil with no user list); abandon is
// AbandonUsersUpgradeFlag. It returns
//
//   - a *UsersUpgradeChangedError when the step is pending for a user who is
//     no longer the only configured user and abandon is false;
//   - a *UsersUpgradeAbandonError when abandon is true and the step is not
//     pending for such a user (no user list, a first start, a done or
//     unreadable marker, or a pending step that resumes at this start);
//   - nil otherwise.
//
// Begin applies the same rule again, on the marker as OpenUsersUpgrade read
// it.
func CheckUsersUpgrade(stateDir string, users []UpgradeUser, abandon bool) error {
	u := &UsersUpgrade{path: filepath.Join(stateDir, UsersUpgradeFile), state: upgradeNoList}
	if len(users) > 0 {
		u.state, u.pending, _ = readUsersUpgrade(u.path)
	}
	return u.check(users, abandon)
}

// resumesFor reports whether a step pending for p resumes with users: p is
// still the only configured user (their admin flag may have changed: Begin
// narrows it). An empty users (no user list) is not a change of users: a
// start with no user list removes the marker, and the step is taken again
// from the start at the next start with one.
func resumesFor(p UpgradeUser, users []UpgradeUser) bool {
	return len(users) == 1 && users[0].Name == p.Name
}

// check is the rule of CheckUsersUpgrade on the marker u holds.
func (u *UsersUpgrade) check(users []UpgradeUser, abandon bool) error {
	changed := u.state == upgradePending && len(users) > 0 && !resumesFor(u.pending, users)
	switch {
	case changed && !abandon:
		return &UsersUpgradeChangedError{Pending: u.pending, Users: users, Marker: u.path}
	case changed || !abandon:
		return nil
	}
	var reason string
	switch u.state {
	case upgradeNoList:
		reason = "the server has no user list (a start with none removes the step's marker)"
	case upgradeFirst:
		reason = fmt.Sprintf("this is the first start with a user list (no marker at %q): the one-time upgrade step has not begun", u.path)
	case upgradePending:
		reason = fmt.Sprintf("the unfinished one-time upgrade step for user %s resumes at this start, since %s is still the only configured user", u.pending.Name, u.pending.Name)
	default:
		reason = fmt.Sprintf("the one-time upgrade step is done (marker %q; a marker that cannot be read counts as done)", u.path)
	}
	return &UsersUpgradeAbandonError{Reason: reason}
}

// OpenUsersUpgrade reads the marker <stateDir>/users-upgrade.json:
//
//   - with no user list (userList false) a marker left by an earlier start,
//     done or pending, is removed, so a server that goes back to no user list
//     and later adds one takes the step again (the way out for an operator
//     who started with several users by mistake);
//   - with a user list and no marker (no directory entry at all) this is the
//     first start with a user list;
//   - with a user list and a pending marker, the step began for the user it
//     names and did not finish (Begin decides whether to resume it);
//   - with a user list and any other marker the step is done. A marker that
//     cannot be read, is not a regular file (a dangling symlink, a directory,
//     a special file), does not parse, has a schema version this build does
//     not know, or has a pending entry that is not a valid user name is taken
//     as "already done", never as "run again", and is logged.
//
// Every outcome is logged with logf, which Begin and Finish also use.
func OpenUsersUpgrade(stateDir string, userList bool, logf func(format string, args ...any)) *UsersUpgrade {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	u := &UsersUpgrade{path: filepath.Join(stateDir, UsersUpgradeFile), logf: logf}
	if !userList {
		u.state = upgradeNoList
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
	var line string
	u.state, u.pending, line = readUsersUpgrade(u.path)
	logf("%s", line)
	return u
}

// readUsersUpgrade reads the marker at path, for a server with a user list,
// without changing anything: the state it records (OpenUsersUpgrade says how
// each kind of marker is taken), the user of a pending step, and the line
// that says what was found.
func readUsersUpgrade(path string) (upgradeState, UpgradeUser, string) {
	// The directory entry itself decides "absent": an entry of any kind (a
	// symlink whose target is gone, a directory, a special file) is a
	// marker that is present, and is never read as a first start.
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return upgradeFirst, UpgradeUser{}, fmt.Sprintf("Users: first start with a user list (no marker at %s): running the one-time upgrade step", path)
	case err != nil:
		return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the user-list upgrade marker %s cannot be read (%v); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", path, err)
	case !fi.Mode().IsRegular():
		return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the user-list upgrade marker %s is not a regular file (%s); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", path, fi.Mode().Type())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the user-list upgrade marker %s cannot be read (%v); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", path, err)
	}
	var m usersUpgradeMarker
	switch err := json.Unmarshal(data, &m); {
	case err != nil:
		return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the user-list upgrade marker %s is damaged (%v); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", path, err)
	case m.Version != usersUpgradeVersion:
		return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the user-list upgrade marker %s has schema version %d, not %d; the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", path, m.Version, usersUpgradeVersion)
	case m.Pending != nil && (!config.ValidUserName(m.Pending.User) || !m.CompletedAt.IsZero()):
		return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the user-list upgrade marker %s is damaged (a pending step for %q); the one-time upgrade step is taken as done: nothing is given a user automatically, use 'shed assign'", path, m.Pending.User)
	case m.Pending != nil:
		return upgradePending, UpgradeUser{Name: m.Pending.User, Admin: m.Pending.Admin},
			fmt.Sprintf("Users: WARNING: the one-time upgrade step for the user list began at %s for user %s and did not finish (marker %s); it resumes only while %s is the only user in auth.users, and until it is done the server does not start with other users", m.Pending.StartedAt.Format(time.RFC3339), m.Pending.User, path, m.Pending.User)
	}
	return upgradeDone, UpgradeUser{}, fmt.Sprintf("Users: the one-time upgrade step for the user list ran at %s (marker %s); sheds with no user are not given one automatically, use 'shed assign'", m.CompletedAt.Format(time.RFC3339), path)
}

// FirstUserListStart reports whether this start found no marker with a user
// list: the server's first start with one (see OpenUsersUpgrade). It does not
// change when Begin or MarkDone writes the marker.
func (u *UsersUpgrade) FirstUserListStart() bool { return u.state == upgradeFirst }

// PendingFor reports the user named by a pending marker found at open: a step
// that began for them and did not finish.
func (u *UsersUpgrade) PendingFor() (UpgradeUser, bool) {
	return u.pending, u.state == upgradePending
}

// UpgradePlan is what this start does in the one-time upgrade step, as Begin
// decided it.
type UpgradePlan struct {
	// StampFor is the user the start-up pass gives sheds and snapshots with
	// no user to (pass it to StampUnassigned as the only user, with
	// firstStart true), and to whom the credential source bindings of the
	// implicit user go; nil when nothing is given to anyone. Its Admin is
	// true only when the user is an admin now and was when the step began.
	StampFor *UpgradeUser
	// SeveralUsers is set on the first start with several users: the step
	// is taken (and recorded) without giving anything to anyone.
	SeveralUsers bool
	// Deferred is set on a first start with one user whose pending marker
	// could not be written: nothing is given to anyone at this start, and
	// the step stays to be taken (records it would act on are kept).
	Deferred bool
}

// Begin decides, from the marker read at open and the configured users, what
// this start does in the one-time step, and records that decision durably
// before the caller acts on it:
//
//   - no user list, or the step done: nothing;
//   - first start with several users: the step is recorded as done first,
//     before any listing (SeveralUsers);
//   - first start with exactly one user: a pending marker naming that user
//     is written before anything is given to anyone (StampFor); if it cannot
//     be written nothing is given this start (Deferred);
//   - a pending step whose user is still the one configured user: resumed
//     for that same user (StampFor);
//   - a pending step in any other case (a different single user, several
//     users, its user no longer configured): without abandon, refused (a
//     *UsersUpgradeChangedError; nothing is written); with abandon (the
//     operator's AbandonUsersUpgradeFlag), abandoned. Nothing is given to
//     anyone, the marker is written as done, and the sheds and snapshots
//     still with no user need 'shed assign'.
//
// abandon with no such pending step is refused (a *UsersUpgradeAbandonError;
// nothing is written). The rule is CheckUsersUpgrade's, which the server
// applies before it opens or changes anything; Begin applies it again.
func (u *UsersUpgrade) Begin(users []UpgradeUser, abandon bool, now time.Time) (UpgradePlan, error) {
	if err := u.check(users, abandon); err != nil {
		return UpgradePlan{}, err
	}
	return u.begin(users, now), nil
}

// begin is Begin once check has passed.
func (u *UsersUpgrade) begin(users []UpgradeUser, now time.Time) UpgradePlan {
	switch u.state {
	case upgradeFirst:
		if len(users) == 0 {
			return UpgradePlan{}
		}
		if len(users) > 1 {
			// With several users nothing is ever stamped, so the outcome is
			// known before any listing: recorded first, so a listing that
			// then fails (or a crash) cannot leave a later start, with fewer
			// users, to take it as the first and stamp what was left
			// unassigned on purpose.
			if err := u.writeDone(now); err != nil {
				u.logf("Users: the one-time upgrade step for the user list (several users: nothing is given a user automatically) could not be recorded; it runs again at the next start: %v", err)
			} else {
				u.logf("Users: the one-time upgrade step for the user list is recorded (several users: nothing is given a user automatically); from now on a shed with no user is given one only by 'shed assign'")
			}
			return UpgradePlan{SeveralUsers: true}
		}
		want := users[0]
		if err := u.write(usersUpgradeMarker{Version: usersUpgradeVersion, Pending: &usersUpgradePending{User: want.Name, Admin: want.Admin, StartedAt: now.UTC()}}); err != nil {
			u.logf("Users: the one-time upgrade step for the user list could not be recorded as begun for user %s, so nothing is given a user at this start; it runs again at the next start: %v", want.Name, err)
			return UpgradePlan{Deferred: true}
		}
		u.logf("Users: the one-time upgrade step for the user list has begun for user %s (the only configured user); until it finishes it resumes only while %s is the only configured user, and the server does not start with other users", want.Name, want.Name)
		return UpgradePlan{StampFor: &want}
	case upgradePending:
		p := u.pending
		if resumesFor(p, users) {
			resumed := UpgradeUser{Name: p.Name, Admin: p.Admin && users[0].Admin}
			if p.Admin != users[0].Admin {
				u.logf("Users: user %s was an admin when the one-time upgrade step began: %v; is now: %v. Sheds that carry host directories or server mounts are given to them only if both", p.Name, p.Admin, users[0].Admin)
			}
			u.logf("Users: resuming the unfinished one-time upgrade step for user %s, still the only configured user", p.Name)
			return UpgradePlan{StampFor: &resumed}
		}
		// The configuration no longer names p alone, and the operator chose
		// to abandon the step (check refused the start otherwise): resuming
		// would give what is left to someone the step was not begun for (or,
		// with several users, to nobody the operator chose). Nothing is
		// given.
		recorded := "it is recorded as done"
		if err := u.writeDone(now); err != nil {
			recorded = fmt.Sprintf("recording it as done failed (%v), so the same decision is taken again at the next start", err)
		}
		u.logf("Users: the unfinished one-time upgrade step for user %s was abandoned, because the configured users are now %s: nothing more is given a user automatically, and %s; the sheds and snapshots still with no user stay admin-only until 'shed assign <shed> <user>' gives one to a user",
			p.Name, describeUsers(users), recorded)
		return UpgradePlan{}
	}
	return UpgradePlan{}
}

// describeUsers names the configured users for a log line.
func describeUsers(users []UpgradeUser) string {
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Name
	}
	return strings.Join(names, ", ")
}

// Finish records the outcome of the start-up pass run under plan: a pass that
// stamped (plan.StampFor) and finished (rep not Incomplete) marks the step
// done; one that did not finish leaves it pending for the same user, to be
// resumed at a later start only while that user is still the only configured
// user. Any other plan has nothing to record.
func (u *UsersUpgrade) Finish(plan UpgradePlan, rep UpgradeReport, now time.Time) {
	if plan.StampFor == nil {
		return
	}
	if rep.Incomplete {
		u.logf("Users: WARNING: the one-time upgrade step for user %s did not finish; it stays pending and resumes at the next start only while %s is the only configured user, and until it is done the server does not start with other users", plan.StampFor.Name, plan.StampFor.Name)
		return
	}
	if err := u.writeDone(now); err != nil {
		u.logf("Users: WARNING: the one-time upgrade step for user %s finished, but its marker could not be written (it stays pending and resumes at the next start only while %s is the only configured user, and until it is done the server does not start with other users): %v", plan.StampFor.Name, plan.StampFor.Name, err)
		return
	}
	u.logf("Users: the one-time upgrade step for the user list is done and recorded; from now on a shed with no user is given one only by 'shed assign'")
}

// MarkDone records, durably, that the one-time upgrade step has run. It does
// nothing unless the marker found at open was absent (a first start) or
// pending: a done marker, or one this build cannot read, is never rewritten.
// On an error the step is not recorded as done.
func (u *UsersUpgrade) MarkDone(now time.Time) error {
	if u.state != upgradeFirst && u.state != upgradePending {
		return nil
	}
	return u.writeDone(now)
}

// writeDone writes the done marker.
func (u *UsersUpgrade) writeDone(now time.Time) error {
	return u.write(usersUpgradeMarker{Version: usersUpgradeVersion, CompletedAt: now.UTC()})
}

// write replaces the marker durably with m: written to a temporary file that
// is synced, renamed into place, and its directory synced.
func (u *UsersUpgrade) write(m usersUpgradeMarker) error {
	data, err := json.MarshalIndent(m, "", "  ")
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
