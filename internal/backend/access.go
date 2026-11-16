package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
)

// This file holds the one access rule. The API server and the SSH server both
// call the backend through a Guard (guard.go), which applies it to the shed or
// snapshot it loads; nothing else decides who may reach a shed.

// PrincipalKind says what kind of caller a Principal is.
type PrincipalKind string

const (
	// PrincipalUser is a person: a user of the server, or the implicit user
	// of a server with no user list.
	PrincipalUser PrincipalKind = "user"
	// PrincipalClient is a service client (auth.clients).
	PrincipalClient PrincipalKind = "client"
	// PrincipalSystem is the server acting for itself (the reaper, the
	// egress re-push, start-up passes). It passes every check.
	PrincipalSystem PrincipalKind = "system"
)

// Principal is who a backend call is made for. Every call through a Guard
// needs one on its context (WithPrincipal); a call without one is refused.
type Principal struct {
	Kind PrincipalKind
	// User is the user's name for PrincipalUser; "" for the implicit user.
	User string
	// Admin users may list, stop and delete sheds that are not theirs, and
	// use what is shared on the host.
	Admin bool
	// Implicit marks the one user of a server with no user list. It passes
	// every check, so a server without users behaves as it always has.
	Implicit bool
	// Client is the service client's name for PrincipalClient.
	Client string
	// Script marks a named user acting with a key listed as a script key
	// (auth.users.<name>.keys[].script): automation, not a person. The
	// access rule treats it as its user; the credential bus, the credentials
	// certificate and the credential source refuse it.
	Script bool
}

// ImplicitPrincipal is the implicit user: the caller every key and every
// certificate maps to on a server with no user list.
func ImplicitPrincipal() Principal {
	return Principal{Kind: PrincipalUser, Admin: true, Implicit: true}
}

// SystemPrincipal is the server acting for itself.
func SystemPrincipal() Principal { return Principal{Kind: PrincipalSystem} }

// ClientPrincipal is the service client named name.
func ClientPrincipal(name string) Principal {
	return Principal{Kind: PrincipalClient, Client: name}
}

// UserPrincipal is the named user of a server with a user list.
func UserPrincipal(name string, admin bool) Principal {
	return Principal{Kind: PrincipalUser, User: name, Admin: admin}
}

// String names the principal for logs and error messages.
func (p Principal) String() string {
	switch {
	case p.Kind == PrincipalSystem:
		return "system"
	case p.Kind == PrincipalClient:
		return "client " + p.Client
	case p.Implicit:
		return "the implicit user"
	case p.Kind == PrincipalUser && p.Script:
		return "user " + p.User + " (script key)"
	case p.Kind == PrincipalUser:
		return "user " + p.User
	}
	return "nobody"
}

// valid reports whether p is a principal the rule knows how to judge. A
// malformed one (an unknown kind, a client without a name, a named user
// spelled as the implicit user) is refused like a missing one.
func (p Principal) valid() bool {
	switch p.Kind {
	case PrincipalSystem:
		return true
	case PrincipalClient:
		return p.Client != ""
	case PrincipalUser:
		if p.Implicit {
			return p.User == ""
		}
		return config.ValidUserName(p.User)
	}
	return false
}

// unrestricted reports whether p passes every check: the server itself, and
// the implicit user of a server with no user list.
func (p Principal) unrestricted() bool {
	return p.Kind == PrincipalSystem || (p.Kind == PrincipalUser && p.Implicit)
}

// IsAdmin reports whether p may use what is shared on the host: an admin, the
// implicit user, or the server itself. A service client never is.
func (p Principal) IsAdmin() bool {
	return p.unrestricted() || (p.Kind == PrincipalUser && p.Admin)
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal on ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// ErrNoPrincipal is returned for a call through a Guard whose context carries
// no principal, or a malformed one. It is a server fault, never the caller's:
// every front door sets one before the backend is reached.
var ErrNoPrincipal = errors.New("no caller identity on the request")

// ErrHostResources marks the refusal of a user who is not an admin to enter
// or start a shed that carries what is shared on the host (AuthorizeShed). It
// is always wrapped together with config.ErrForbiddenSentinel.
var ErrHostResources = errors.New("the shed carries host directories or server mounts")

// Action is what a caller asks to do to a shed or a snapshot.
type Action int

const (
	// ActionView reads a shed's record (get, list, wait for it to settle).
	ActionView Action = iota + 1
	// ActionManage changes a shed's lifecycle without entering it: stop
	// and delete. For a snapshot: delete.
	ActionManage
	// ActionEnter reaches inside a shed: start, reset, exec, a shell, sftp,
	// a forwarded port, its sessions, its egress, a snapshot of it. For a
	// snapshot: create a shed from it.
	ActionEnter
	// ActionAdmin is for what belongs to the whole server: the image store,
	// prune, host directories.
	ActionAdmin
)

func (a Action) String() string {
	switch a {
	case ActionView:
		return "view"
	case ActionManage:
		return "manage"
	case ActionEnter:
		return "enter"
	case ActionAdmin:
		return "admin"
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Authorize is the access rule for a shed whose reported service client is
// owner and whose reported user is user (config.ShedOwnership; never the raw
// stored fields, so a record that disagrees about whose it is, reported as
// config.UserUnresolved, reaches an admin only).
//
//   - The server itself and the implicit user may do anything.
//   - A service client reaches only its own sheds, and every action on them
//     but ActionAdmin.
//   - A user reaches their own shed for everything but ActionAdmin.
//   - An admin may view, stop and delete any shed, and may enter a service
//     client's shed; an admin may not enter another user's shed, a shed with
//     no user, or an unresolved one (ErrForbiddenSentinel).
//   - ActionAdmin needs an admin.
//
// A caller who may not see the shed at all gets ErrShedNotFoundSentinel, so
// nobody learns which names other callers use. The error wraps one of
// config.ErrShedNotFoundSentinel, config.ErrForbiddenSentinel and
// ErrNoPrincipal.
func Authorize(p Principal, owner, user string, a Action) error {
	if !p.valid() {
		return ErrNoPrincipal
	}
	if p.unrestricted() {
		return nil
	}
	if p.Kind == PrincipalClient {
		if owner != p.Client {
			return config.ErrShedNotFoundSentinel
		}
		if a == ActionAdmin {
			return config.ErrForbiddenSentinel
		}
		return nil
	}
	// A named user from here on.
	if a == ActionAdmin {
		if p.Admin {
			return nil
		}
		return config.ErrForbiddenSentinel
	}
	if owner == "" && user == p.User {
		return nil
	}
	if !p.Admin {
		return config.ErrShedNotFoundSentinel
	}
	if owner != "" {
		// A service client's shed: an admin may do everything to it.
		return nil
	}
	if a == ActionEnter {
		return config.ErrForbiddenSentinel
	}
	return nil
}

// AuthorizeShed applies Authorize to a shed as it was reported, with an error
// that names it, and then the one rule that reads more of the shed than whose
// it is: a user who is not an admin may not enter (or start) a shed that
// carries what is shared on the host (config.Shed.HostResources: host
// directories or server mounts), even their own. Such a shed was made by
// someone who was an admin then; the rights it carries are an admin's, so a
// user demoted since keeps only view, stop and delete. A service client's
// sheds follow its own mount policy and are not affected.
func AuthorizeShed(p Principal, shed *config.Shed, a Action) error {
	err := Authorize(p, shed.Owner, shed.User, a)
	if err == nil && a == ActionEnter && p.Kind == PrincipalUser && !p.IsAdmin() {
		if what := shed.HostResources(); len(what) > 0 {
			return fmt.Errorf("%w: %w: shed %s carries %s; only an admin can enter or start it (you may still stop or delete it)",
				config.ErrForbiddenSentinel, ErrHostResources, shed.Name, strings.Join(what, " and "))
		}
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, config.ErrShedNotFoundSentinel):
		return fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, shed.Name)
	case errors.Is(err, config.ErrForbiddenSentinel) && a == ActionAdmin:
		return fmt.Errorf("%w: only an admin can do this", config.ErrForbiddenSentinel)
	case errors.Is(err, config.ErrForbiddenSentinel):
		return fmt.Errorf("%w: shed %s belongs to another user; an admin can list, stop and delete it but not enter it", config.ErrForbiddenSentinel, shed.Name)
	}
	return err
}

// AuthorizeSnapshot is the access rule for a snapshot whose user is user.
// Snapshots belong to their owner: the owner may do anything with one. An
// admin may view and delete any snapshot, and may create a shed from a
// snapshot with no user, never from another user's (that would be entering
// it). A service client reaches no snapshot. The error wraps
// config.ErrSnapshotNotFoundSentinel, config.ErrForbiddenSentinel or
// ErrNoPrincipal.
func AuthorizeSnapshot(p Principal, name, user string, a Action) error {
	if !p.valid() {
		return ErrNoPrincipal
	}
	if p.unrestricted() {
		return nil
	}
	notFound := fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
	if p.Kind == PrincipalClient {
		return notFound
	}
	if a == ActionAdmin {
		if p.Admin {
			return nil
		}
		return fmt.Errorf("%w: only an admin can do this", config.ErrForbiddenSentinel)
	}
	if user == p.User {
		return nil
	}
	if !p.Admin {
		return notFound
	}
	if a == ActionEnter && user != "" {
		return fmt.Errorf("%w: snapshot %s belongs to another user; an admin can list and delete it but not use it", config.ErrForbiddenSentinel, name)
	}
	return nil
}

// AuthorizeAdmin refuses a caller who may not use what is shared on the host
// (the image store, prune, host directories, server mounts): anyone but an
// admin, the implicit user and the server itself. what names the thing in
// the error.
func AuthorizeAdmin(p Principal, what string) error {
	if !p.valid() {
		return ErrNoPrincipal
	}
	if p.IsAdmin() {
		return nil
	}
	return fmt.Errorf("%w: admin only: %s", config.ErrForbiddenSentinel, what)
}

// requiredSnapshot is the incarnation of a snapshot an operation acts on.
type requiredSnapshot struct {
	CreatedAt time.Time
	User      string
}

type requiredSnapshotKey struct{}

// WithRequiredSnapshot returns ctx carrying the snapshot an operation on a
// snapshot (delete, or create-from-snapshot) must find: its creation time
// and its user, as the Guard saw and authorized them. The backend checks
// them with CheckSnapshot under its snapshot lock, so a snapshot deleted
// and taken again under the name in between (by anyone) is not acted on.
func WithRequiredSnapshot(ctx context.Context, createdAt time.Time, user string) context.Context {
	return context.WithValue(ctx, requiredSnapshotKey{}, requiredSnapshot{CreatedAt: createdAt, User: user})
}

// CheckSnapshot returns a snapshot-not-found error for name when ctx
// requires a snapshot (WithRequiredSnapshot) and snap is not that one, and
// nil otherwise.
func CheckSnapshot(ctx context.Context, name string, snap *config.Snapshot) error {
	want, ok := ctx.Value(requiredSnapshotKey{}).(requiredSnapshot)
	if !ok {
		return nil
	}
	if snap == nil || !snap.CreatedAt.Equal(want.CreatedAt) || snap.User != want.User {
		return fmt.Errorf("%w: %s (taken again since it was looked up)", config.ErrSnapshotNotFoundSentinel, name)
	}
	return nil
}
