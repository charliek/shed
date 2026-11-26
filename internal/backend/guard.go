package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/config"
)

// Guard is the Backend the API server and the SSH server are built with. It
// applies the access rule (access.go) to every call, for the principal on the
// call's context, and refuses a call that carries none.
//
// For an operation on a named shed it loads the shed through the backend it
// wraps, authorizes on the shed as the backend reports it (its reported owner
// and user, so a record that disagrees about whose it is reaches an admin
// only), and then calls the backend with the shed's incarnation (its creation
// time) and the ownership it authorized against (the reported owner and
// user) required on the context. The backend checks those again, under its
// per-shed lock, against the shed it actually acts on: a shed deleted and
// created again under the name, by anyone, between this lookup and the action
// is not acted on, and neither is one whose reported user changed in between
// (on the remote backend a reconcile can report it config.UserUnresolved,
// admin-only; on Firecracker and VZ a shed's user changes only by `shed
// assign`, of a shed with no user, so there the check refuses an operation
// authorized on an unassigned shed that was assigned in between). A creation time the caller already required (the reaper,
// a wait that follows a create) is never replaced: the shed loaded must have
// it. Snapshots are bound the same way, by creation time and user, under the
// snapshot lock.
//
// The wrapped backend is a named field, not an embedded one, so a method
// added to Backend does not compile until it is given a rule here. The
// optional interfaces (capabilities.go) see through a Guard: IsAsync and
// AsEgressController return guarded implementations of the capabilities that
// act, and the capability questions read the wrapped backend.
type Guard struct {
	inner Backend
	// beforeAct, when set, runs after a call has been authorized and before
	// the wrapped backend is called. It is a test seam: a test uses it to
	// replace the shed between the lookup and the action.
	beforeAct func(op, name string)

	// masked rate-limits the server log line for a lookup failure answered
	// as not found (maskLookup): the time each name was last logged.
	// maskedOldest is no later than the oldest time in masked, so a full
	// table is scanned for expired names only when one may have expired.
	// maskedSuppressed counts the failures not logged because the table was
	// full of names still inside their interval, and maskedSummaryAt is when
	// that count was last logged.
	maskedMu         sync.Mutex
	masked           map[string]time.Time
	maskedOldest     time.Time
	maskedSuppressed int
	maskedSummaryAt  time.Time
	// now and logf are the clock and the log maskLookup uses; tests replace
	// them.
	now  func() time.Time
	logf func(format string, args ...any)

	// audit is the server's audit log, where the lifecycle events are
	// written (nil: none). See SetAuditLog.
	audit atomic.Pointer[audit.Log]
}

// maskedLogInterval is how often a lookup failure answered as not found is
// logged for one name: at most once per interval, so a caller who keeps
// asking cannot flood the log.
const maskedLogInterval = time.Minute

// maskedLogMax bounds the names maskLookup remembers. A name is forgotten
// only once its interval has run out, so the per-name limit holds however
// many names are asked for: while the table is full of names still inside
// their interval, a failure for a new name is not logged but counted, and
// the count is logged at most once per maskedLogInterval.
const maskedLogMax = 1024

// Compile-time check that *Guard is a Backend.
var _ Backend = (*Guard)(nil)

// NewGuard returns a Guard around b.
func NewGuard(b Backend) *Guard { return &Guard{inner: b, now: time.Now, logf: log.Printf} }

// Guarded returns b as a Guard: b itself when it is one, a new Guard around
// it otherwise, and nil for nil (a server built without a backend, as some
// tests build one).
func Guarded(b Backend) *Guard {
	if b == nil {
		return nil
	}
	if g, ok := b.(*Guard); ok {
		return g
	}
	return NewGuard(b)
}

// SetAuditLog attaches the server's audit log. The guard then writes a
// lifecycle event (audit.ShedEvent) for every create, start, stop, delete,
// reset, assignment and provision that passed the access rule, however it
// ended: the shed as it was when the action was authorized (its name, id and
// whose it was; for a create or an assignment, as it was made), who acted,
// and whether it succeeded. A call the rule refused writes none. The guard
// is where these are written because every front door (the API, the SSH
// server's wake, the reaper) and every backend goes through it, and it holds
// both the caller and the shed it authorized. nil writes none.
func (g *Guard) SetAuditLog(l *audit.Log) { g.audit.Store(l) }

// actor is who p is as an audit event names them: the actor the front door
// put on ctx (audit.WithActor, which also names the caller's key and
// certificate) when it is the same principal, or p itself.
func actor(ctx context.Context, p Principal) audit.Identity {
	if id, ok := audit.ActorFrom(ctx); ok && actorIs(id, p) {
		return id
	}
	switch p.Kind {
	case PrincipalSystem:
		return audit.SystemIdentity()
	case PrincipalClient:
		return audit.ClientIdentity(p.Client, "")
	}
	return audit.PersonIdentity(p.Kind == PrincipalUser, p.User, p.Implicit, p.IsAdmin(), p.Script, "", "")
}

// actorIs reports whether id names the principal p.
func actorIs(id audit.Identity, p Principal) bool {
	switch p.Kind {
	case PrincipalSystem:
		return id.Kind == audit.KindSystem
	case PrincipalClient:
		return id.Kind == audit.KindClient && id.Name == p.Client
	case PrincipalUser:
		return id.Kind == audit.KindUser && id.Implicit == p.Implicit && id.User == p.User &&
			id.Script != nil && *id.Script == p.Script
	}
	return false
}

// withActor returns ctx carrying p's actor, so what the backend does with it
// (a delivery's own event) names the same caller.
func withActor(ctx context.Context, p Principal) context.Context {
	return audit.WithActor(ctx, actor(ctx, p))
}

// shedRef is shed as an event names it.
func shedRef(shed *config.Shed) audit.ShedRef {
	return audit.ShedRef{Shed: shed.Name, ShedID: shed.ID, ShedUser: shed.User, ShedOwner: shed.Owner}
}

// record writes one lifecycle event: kind, done by p (as ctx names them) to
// the shed ref, ending in err.
func (g *Guard) record(ctx context.Context, p Principal, kind string, ref audit.ShedRef, err error) {
	l := g.audit.Load()
	if l == nil {
		return
	}
	result := audit.ResultOK
	if err != nil {
		result = audit.ResultFailed
	}
	if werr := l.Record(audit.ShedEvent{Event: kind, Identity: actor(ctx, p), ShedRef: ref, Result: result}); werr != nil {
		g.logf("audit: %v", werr)
	}
}

// Inner returns the backend g wraps. Only code that acts as the server
// itself (start-up passes, the credential bus) should call it directly.
func (g *Guard) Inner() Backend { return g.inner }

// SetBeforeAct installs a function run after every authorization and before
// the wrapped backend is called, with the operation's name and the shed's or
// snapshot's name. For tests that force an interleaving; set it before use.
func (g *Guard) SetBeforeAct(fn func(op, name string)) { g.beforeAct = fn }

// unwrap returns the backend under every Guard around b, or b itself. A
// Guard around a Guard is unwrapped all the way down, so a capability lookup
// never stops at a Guard and mistakes the backend for a local one.
func unwrap(b Backend) Backend {
	for {
		g, ok := b.(*Guard)
		if !ok || g == nil {
			return b
		}
		b = g.inner
	}
}

func (g *Guard) acting(op, name string) {
	if g.beforeAct != nil {
		g.beforeAct(op, name)
	}
}

// principal returns the valid principal on ctx, or ErrNoPrincipal.
func principal(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok || !p.valid() {
		return Principal{}, ErrNoPrincipal
	}
	return p, nil
}

// maskLookup is what a caller gets when the lookup of the shed or snapshot
// named name (kind says which) failed with err, before the caller's right to
// see it was established: a record that cannot be read, is malformed, or has
// a version this build refuses. Not found stays not found. An admin, the
// implicit user and the server's own work get err as it is, as before. Every
// other caller (a user who is not an admin, a service client) gets the
// ordinary not-found answer (notFound, with the name), as for a shed or
// snapshot they may not see: the error's detail (it may name a path on the
// server, or a version) is not theirs to read, and its difference from not
// found would tell them the name is taken. The detail goes to the server
// log, at most once per name per maskedLogInterval.
func (g *Guard) maskLookup(p Principal, kind, name string, err, notFound error) error {
	if errors.Is(err, notFound) || p.IsAdmin() {
		return err
	}
	logNow, suppressed := g.logMasked(kind + " " + name)
	if logNow || suppressed > 0 {
		logf := g.logf
		if logf == nil {
			logf = log.Printf
		}
		if suppressed > 0 {
			logf("access guard: %d more lookup failure(s) answered as not found were not logged: more than %d names failed within %s", suppressed, maskedLogMax, maskedLogInterval)
		}
		if logNow {
			logf("access guard: looking up %s %q for %s failed; answered as not found: %v", kind, name, p, err)
		}
	}
	return fmt.Errorf("%w: %s", notFound, name)
}

// logMasked reports whether a masked lookup failure for key should be logged
// now (none was in the last maskedLogInterval), and records it if so. A name
// still inside its interval is never forgotten to make room: when the table
// is full, names whose interval has run out are dropped, and if none has, a
// new name is not logged and is counted instead. suppressed, when above zero,
// is that count, due to be logged now (at most once per maskedLogInterval);
// it is then reset.
func (g *Guard) logMasked(key string) (logNow bool, suppressed int) {
	now := time.Now
	if g.now != nil {
		now = g.now
	}
	t := now()
	g.maskedMu.Lock()
	defer g.maskedMu.Unlock()
	defer func() {
		if g.maskedSuppressed > 0 && t.Sub(g.maskedSummaryAt) >= maskedLogInterval {
			suppressed, g.maskedSuppressed = g.maskedSuppressed, 0
			g.maskedSummaryAt = t
		}
	}()
	last, known := g.masked[key]
	if known && t.Sub(last) < maskedLogInterval {
		return false, 0
	}
	if g.masked == nil {
		g.masked = map[string]time.Time{}
	}
	if !known && len(g.masked) >= maskedLogMax && t.Sub(g.maskedOldest) >= maskedLogInterval {
		oldest := t
		for k, at := range g.masked {
			switch {
			case t.Sub(at) >= maskedLogInterval:
				delete(g.masked, k)
			case at.Before(oldest):
				oldest = at
			}
		}
		g.maskedOldest = oldest
	}
	if !known && len(g.masked) >= maskedLogMax {
		g.maskedSuppressed++
		return false, 0
	}
	if len(g.masked) == 0 {
		g.maskedOldest = t
	}
	g.masked[key] = t
	return true, 0
}

// bind loads the named shed, authorizes a on it, and returns ctx bound to the
// incarnation it loaded and to the ownership it authorized against: the
// shed's creation time and its reported owner and user (see Guard). A value
// the caller already required is never replaced; the shed loaded must match
// it.
func (g *Guard) bind(ctx context.Context, op, name string, a Action) (context.Context, *config.Shed, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, nil, err
	}
	shed, err := g.inner.GetShed(ctx, name)
	if err != nil {
		return nil, nil, g.maskLookup(p, "shed", name, err, config.ErrShedNotFoundSentinel)
	}
	if shed == nil {
		return nil, nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	if err := CheckIdentity(ctx, name, shed.Owner, shed.User, shed.CreatedAt); err != nil {
		return nil, nil, err
	}
	if err := AuthorizeShed(p, shed, a); err != nil {
		return nil, nil, err
	}
	ctx = PinOwnership(PinCreatedAt(ctx, shed.CreatedAt), shed.Owner, shed.User)
	g.acting(op, name)
	return ctx, shed, nil
}

// Check authorizes a on the named shed for the principal on ctx and returns
// the shed as loaded. The SSH server asks it before it waits for or wakes a
// shed, so a refused caller is told why at once.
func (g *Guard) Check(ctx context.Context, name string, a Action) (*config.Shed, error) {
	_, shed, err := g.bind(ctx, "Check", name, a)
	if err != nil {
		return nil, err
	}
	return g.shownTo(ctx, shed), nil
}

// recordCaps is what the report step (shownTo) reads from the wrapped
// backend: its capabilities, with no isolation kind when it cannot give a
// shed the isolated profile now (on remote, a firewall config that admits
// more than the server), so a record never claims an isolation the backend
// does not hold.
func (g *Guard) recordCaps() Capabilities {
	caps := CapabilitiesOf(g.inner)
	if !SupportsNetworkProfile(g.inner, config.NetworkProfileIsolated) {
		caps.Isolation = ""
	}
	return caps
}

// shownTo is the report step: every shed record a Guard returns goes through
// it, and it always returns a copy, so the backend's record is never changed.
//
// It sets the members computed when a record is reported, never stored:
// Isolation, from the backend's capabilities and the shed's network profile
// (IsolationOf: fail closed, and whatever the backend set is replaced), and
// Arch from the backend's GuestArch when the record carries none (a local
// backend; a remote record carries its own, or none).
//
// And it shows the record as the principal on ctx may see it. A caller who
// may not enter the shed (an admin looking at another user's shed) gets no
// repository URL and no landing directory at all. A clone URL can carry a
// token anywhere (its userinfo, its query, its path), and the landing
// directory is derived from the URL, so no redaction of either is safe: both
// are withheld. The shed's own user, the implicit user, the server and anyone
// else who may enter it see them.
func (g *Guard) shownTo(ctx context.Context, shed *config.Shed) *config.Shed {
	return g.shownWith(ctx, g.recordCaps(), shed)
}

// shownWith is shownTo with the capabilities already read (ListSheds reads
// them once for every row).
func (g *Guard) shownWith(ctx context.Context, caps Capabilities, shed *config.Shed) *config.Shed {
	if shed == nil {
		return nil
	}
	cp := *shed
	cp.Isolation = IsolationOf(caps, cp.NetworkProfile)
	if cp.Arch == "" {
		cp.Arch = caps.GuestArch
	}
	if cp.Repo != "" || cp.LandingDir != "" || cp.Delivery != nil {
		p, _ := PrincipalFrom(ctx)
		if Authorize(p, cp.Owner, cp.User, ActionEnter) != nil {
			// The delivery record goes the same way: its items, their
			// targets and the source's problems are the shed's user's,
			// and only the state is shown.
			cp.Repo, cp.LandingDir = "", ""
			cp.Delivery = cp.Delivery.StateOnly()
		}
	}
	return &cp
}

// showFor returns shownTo for ctx in the shape of a call that returns a shed
// and an error, so a guarded method can pass the wrapped call's results
// straight through it.
func (g *Guard) showFor(ctx context.Context) func(*config.Shed, error) (*config.Shed, error) {
	return func(shed *config.Shed, err error) (*config.Shed, error) {
		if err != nil {
			return shed, err
		}
		return g.shownTo(ctx, shed), nil
	}
}

// Type returns the wrapped backend's type. It needs no principal.
func (g *Guard) Type() Type { return g.inner.Type() }

// Close closes the wrapped backend. It needs no principal: only shed-server
// calls it, at exit.
func (g *Guard) Close() error { return g.inner.Close() }

// CreateShed stamps the shed's owner and user from the principal, never from
// the request (a service client sets owner, a user sets user, never both),
// and applies the rules for what is shared on the host: a user
// who is not an admin gets no host directories, no server mounts and none of
// the server's env_file variables. A shed made from a snapshot needs the
// right to use that snapshot, which is then bound like a shed.
func (g *Guard) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case PrincipalClient:
		req.Owner, req.User = p.Client, ""
	case PrincipalUser:
		req.Owner, req.User = "", p.User
		// A script key's create asks no source for its shed.
		req.ScriptKey = p.Script
		if !p.IsAdmin() {
			if what := hostResourceAsked(req); what != "" {
				return nil, AuthorizeAdmin(p, what)
			}
			req.NoServerEnv = true
		}
	}
	if req.FromSnapshot != "" {
		if ctx, _, err = g.bindSnapshot(ctx, p, req.FromSnapshot, ActionEnter); err != nil {
			return nil, err
		}
	}
	g.acting("CreateShed", req.Name)
	ctx = withActor(ctx, p)
	shed, err := g.inner.CreateShed(ctx, req)
	ref := audit.ShedRef{Shed: req.Name, ShedID: req.ID, ShedUser: req.User, ShedOwner: req.Owner}
	if err == nil && shed != nil {
		ref = shedRef(shed)
	}
	g.record(ctx, p, audit.EventShedCreate, ref, err)
	if err != nil && errors.Is(err, config.ErrShedAlreadyExistsSentinel) && !p.unrestricted() {
		// Names are unique per server, across users. A taken name is the
		// usual conflict, worded the same whoever holds it, so it tells the
		// caller nothing of whose the shed is or what it is like.
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}
	return g.showFor(ctx)(shed, err)
}

// hostResourceAsked names the first thing a create asks for that is shared on
// the host and so is admin-only, or "". A selection of server mounts must be
// explicit and empty: nil means every server mount.
func hostResourceAsked(req config.CreateShedRequest) string {
	switch {
	case req.LocalDir != "":
		return "local_dir"
	case len(req.AddDirs) > 0:
		return "add_dirs"
	case req.ResolvedMounts == nil || req.ResolvedMounts.Mode != config.MountModeSelected || len(req.ResolvedMounts.Names) > 0:
		return "mounts"
	}
	return ""
}

// GetShed returns the shed if the principal may view it.
func (g *Guard) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	_, shed, err := g.bind(ctx, "GetShed", name, ActionView)
	return g.showFor(ctx)(shed, err)
}

// ListSheds returns the sheds the principal may view, each as shownTo gives
// it. An unrestricted principal (the system, the implicit user) views every
// shed, and every row still goes through the report step.
func (g *Guard) ListSheds(ctx context.Context) ([]config.Shed, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	sheds, err := g.inner.ListSheds(ctx)
	all := p.unrestricted()
	if err != nil || (all && sheds == nil) {
		return sheds, err
	}
	caps := g.recordCaps()
	out := make([]config.Shed, 0, len(sheds))
	for i := range sheds {
		if all || Authorize(p, sheds[i].Owner, sheds[i].User, ActionView) == nil {
			out = append(out, *g.shownWith(ctx, caps, &sheds[i]))
		}
	}
	return out, nil
}

// DeleteShed needs the right to manage the shed.
func (g *Guard) DeleteShed(ctx context.Context, name string) error {
	bctx, shed, err := g.bind(ctx, "DeleteShed", name, ActionManage)
	if err != nil {
		return err
	}
	err = g.inner.DeleteShed(bctx, name)
	g.recordOn(ctx, audit.EventShedDelete, shed, err)
	return err
}

// recordOn writes a lifecycle event for the principal on ctx, on the shed as
// bind authorized it.
func (g *Guard) recordOn(ctx context.Context, kind string, shed *config.Shed, err error) {
	p, _ := PrincipalFrom(ctx)
	g.record(ctx, p, kind, shedRef(shed), err)
}

// StartShed needs the right to enter the shed.
func (g *Guard) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	bctx, shed, err := g.bind(ctx, "StartShed", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	started, err := g.inner.StartShed(bctx, name)
	g.recordOn(ctx, audit.EventShedStart, shed, err)
	return g.showFor(ctx)(started, err)
}

// StopShed needs the right to manage the shed.
func (g *Guard) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	bctx, shed, err := g.bind(ctx, "StopShed", name, ActionManage)
	if err != nil {
		return nil, err
	}
	stopped, err := g.inner.StopShed(bctx, name)
	g.recordOn(ctx, audit.EventShedStop, shed, err)
	return g.showFor(ctx)(stopped, err)
}

// AssignUser is admin-only. The admin is checked before the shed is looked
// up, so a caller who is not one learns nothing about the name. The shed is
// then bound like any (by name and incarnation, see Guard), so a shed deleted
// and created again under the name since this lookup is not assigned. Whose
// it is, its host resources and the target's name are decided by the backend
// under its lock (CheckAssignable); the admin's own right lasts only for this
// call (nothing records who assigned a shed). AllowHostResources is never
// taken from a caller: only the server's start-up pass, on the raw backend,
// sets it.
func (g *Guard) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := AuthorizeAdmin(p, "assigning a shed to a user"); err != nil {
		return nil, err
	}
	if err := ValidateAssignee(req); err != nil {
		return nil, err
	}
	req.AllowHostResources = false
	bctx, shed, err := g.bind(ctx, "AssignUser", name, ActionManage)
	if err != nil {
		return nil, err
	}
	assigned, err := g.inner.AssignUser(bctx, name, req)
	// An assignment is the new user's from here on: the event names the
	// shed as it was made (with its user), or as it was when it failed.
	ref := shedRef(shed)
	if err == nil && assigned != nil {
		ref = shedRef(assigned)
	}
	g.record(ctx, p, audit.EventShedAssign, ref, err)
	return g.showFor(ctx)(assigned, err)
}

// ResetShed needs the right to enter the shed.
func (g *Guard) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	bctx, shed, err := g.bind(ctx, "ResetShed", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	reset, err := g.inner.ResetShed(bctx, name)
	g.recordOn(ctx, audit.EventShedReset, shed, err)
	return g.showFor(ctx)(reset, err)
}

// ListSessions needs the right to enter the shed.
func (g *Guard) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	bctx, _, err := g.bind(ctx, "ListSessions", shedName, ActionEnter)
	if err != nil {
		return nil, err
	}
	return g.inner.ListSessions(bctx, shedName)
}

// KillSession needs the right to enter the shed.
func (g *Guard) KillSession(ctx context.Context, shedName, sessionName string) error {
	bctx, _, err := g.bind(ctx, "KillSession", shedName, ActionEnter)
	if err != nil {
		return err
	}
	return g.inner.KillSession(bctx, shedName, sessionName)
}

// Exec needs the right to enter the shed.
func (g *Guard) Exec(ctx context.Context, shedName string, opts ExecOptions) error {
	bctx, _, err := g.bind(ctx, "Exec", shedName, ActionEnter)
	if err != nil {
		return err
	}
	return g.inner.Exec(bctx, shedName, opts)
}

// DialService needs the right to enter the shed.
func (g *Guard) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	bctx, _, err := g.bind(ctx, "DialService", shedName, ActionEnter)
	if err != nil {
		return nil, err
	}
	return g.inner.DialService(bctx, shedName, port)
}

// ListImages needs a principal: anyone may read the image list.
func (g *Guard) ListImages(ctx context.Context) ([]config.ImageInfo, error) {
	if _, err := principal(ctx); err != nil {
		return nil, err
	}
	return g.inner.ListImages(ctx)
}

// InspectImage needs a principal: anyone may read an image's details.
func (g *Guard) InspectImage(ctx context.Context, tagOrDigest string) (config.ImageInspectResponse, error) {
	if _, err := principal(ctx); err != nil {
		return config.ImageInspectResponse{}, err
	}
	return g.inner.InspectImage(ctx, tagOrDigest)
}

// admin authorizes an admin-only call.
func (g *Guard) admin(ctx context.Context, op, what string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if err := AuthorizeAdmin(p, what); err != nil {
		return err
	}
	g.acting(op, "")
	return nil
}

// TagImage is admin-only: it changes the shared image store.
func (g *Guard) TagImage(ctx context.Context, srcTagOrDigest, newTag string) error {
	if err := g.admin(ctx, "TagImage", "image tag"); err != nil {
		return err
	}
	return g.inner.TagImage(ctx, srcTagOrDigest, newTag)
}

// PullImage is admin-only: it changes the shared image store.
func (g *Guard) PullImage(ctx context.Context, dockerRef, tag, platform string, withLayers bool) (string, error) {
	if err := g.admin(ctx, "PullImage", "image pull"); err != nil {
		return "", err
	}
	return g.inner.PullImage(ctx, dockerRef, tag, platform, withLayers)
}

// PushImage is admin-only: it uses the server's registry access.
func (g *Guard) PushImage(ctx context.Context, tagOrDigest, dstRef string) error {
	if err := g.admin(ctx, "PushImage", "image push"); err != nil {
		return err
	}
	return g.inner.PushImage(ctx, tagOrDigest, dstRef)
}

// DeleteImage is admin-only: it changes the shared image store.
func (g *Guard) DeleteImage(ctx context.Context, name string) error {
	if err := g.admin(ctx, "DeleteImage", "image delete"); err != nil {
		return err
	}
	return g.inner.DeleteImage(ctx, name)
}

// PruneImages is admin-only: it changes the shared image store.
func (g *Guard) PruneImages(ctx context.Context, dryRun bool) ([]config.ImageInfo, error) {
	if err := g.admin(ctx, "PruneImages", "image prune"); err != nil {
		return nil, err
	}
	return g.inner.PruneImages(ctx, dryRun)
}

// Prune is admin-only: it removes what every user's sheds share.
func (g *Guard) Prune(ctx context.Context, opts PruneOptions) (config.PruneReport, error) {
	if err := g.admin(ctx, "Prune", "system prune"); err != nil {
		return config.PruneReport{}, err
	}
	return g.inner.Prune(ctx, opts)
}

// DiskUsage needs a principal. A caller who is not an admin sees only the
// shed and snapshot entries they may view, and no orphans; the totals are
// the server's. Whose an entry is comes from the entry itself, which the
// backend reads from the same record as its sizes: an entry is never matched
// by name against a later listing, in which a shed or snapshot deleted and
// made again under its name by someone else would pass for the old one.
func (g *Guard) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	p, err := principal(ctx)
	if err != nil {
		return config.DiskUsage{}, err
	}
	usage, err := g.inner.DiskUsage(ctx)
	if err != nil || p.IsAdmin() {
		return usage, err
	}
	keptSheds := make([]config.ShedDiskEntry, 0, len(usage.Sheds))
	for _, e := range usage.Sheds {
		if Authorize(p, e.Owner, e.User, ActionView) == nil {
			keptSheds = append(keptSheds, e)
		}
	}
	usage.Sheds = keptSheds
	if usage.Snapshots != nil {
		keptSnaps := make([]config.SnapshotDiskEntry, 0, len(usage.Snapshots))
		for _, e := range usage.Snapshots {
			if AuthorizeSnapshot(p, e.Name, e.User, ActionView) == nil {
				keptSnaps = append(keptSnaps, e)
			}
		}
		usage.Snapshots = keptSnaps
	}
	usage.Orphans = []config.FileEntry{}
	return usage, nil
}

// CreateSnapshot needs the right to enter the source shed, which is bound
// like any shed. The snapshot takes the user the source is reported with
// (the backend decides that, under its lock).
func (g *Guard) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	bctx, _, err := g.bind(ctx, "CreateSnapshot", req.SourceShed, ActionEnter)
	if err != nil {
		return nil, err
	}
	return g.inner.CreateSnapshot(bctx, req)
}

// ListSnapshots returns the snapshots the principal may view.
func (g *Guard) ListSnapshots(ctx context.Context) ([]config.Snapshot, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := g.inner.ListSnapshots(ctx)
	if err != nil || p.unrestricted() {
		return snaps, err
	}
	out := make([]config.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if AuthorizeSnapshot(p, s.Name, s.User, ActionView) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// GetSnapshot returns the snapshot if the principal may view it.
func (g *Guard) GetSnapshot(ctx context.Context, name string) (*config.Snapshot, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	_, snap, err := g.bindSnapshot(ctx, p, name, ActionView)
	return snap, err
}

// DeleteSnapshot needs the right to manage the snapshot, which is bound by
// its creation time and user.
func (g *Guard) DeleteSnapshot(ctx context.Context, name string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	bctx, _, err := g.bindSnapshot(ctx, p, name, ActionManage)
	if err != nil {
		return err
	}
	g.acting("DeleteSnapshot", name)
	return g.inner.DeleteSnapshot(bctx, name)
}

// bindSnapshot loads the named snapshot through the wrapped backend (a
// missing one is config.ErrSnapshotNotFoundSentinel), authorizes a on it for
// p, and returns ctx bound to the snapshot it loaded: its creation time and
// user (WithRequiredSnapshot).
func (g *Guard) bindSnapshot(ctx context.Context, p Principal, name string, a Action) (context.Context, *config.Snapshot, error) {
	snap, err := g.inner.GetSnapshot(ctx, name)
	if err != nil {
		return nil, nil, g.maskLookup(p, "snapshot", name, err, config.ErrSnapshotNotFoundSentinel)
	}
	if snap == nil {
		return nil, nil, fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
	}
	if err := AuthorizeSnapshot(p, name, snap.User, a); err != nil {
		return nil, nil, err
	}
	return WithRequiredSnapshot(ctx, snap.CreatedAt, snap.User), snap, nil
}

// guardedAsync is a Guard's AsyncLifecycle: the wrapped backend's, behind
// the access rule.
type guardedAsync struct {
	g     *Guard
	inner AsyncLifecycle
}

// WaitSettled needs the right to view the shed, and follows the incarnation
// it loaded (or the one the caller required).
func (a guardedAsync) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	bctx, _, err := a.g.bind(ctx, "WaitSettled", name, ActionView)
	if err != nil {
		return nil, err
	}
	return a.g.showFor(ctx)(a.inner.WaitSettled(bctx, name))
}

// DeleteAndWait deletes the named shed and waits until its VM is destroyed.
// The record is gone as soon as the delete is acknowledged, so the wait is
// not authorized again: it is authorized once, for the delete, and follows
// the incarnation the delete acted on, through its tombstone. A destroy that
// failed is reported (the wrapped backend's error), never as success.
func (a guardedAsync) DeleteAndWait(ctx, jobCtx context.Context, name string) (err error) {
	bctx, shed, err := a.g.bind(ctx, "DeleteShed", name, ActionManage)
	if err != nil {
		return err
	}
	defer func() { a.g.recordOn(ctx, audit.EventShedDelete, shed, err) }()
	if err := a.g.inner.DeleteShed(PinOwnership(PinCreatedAt(jobCtx, shed.CreatedAt), shed.Owner, shed.User), name); err != nil {
		return err
	}
	// The name is free from here on: anyone may create a shed under it
	// before the wait below looks. The wait is bound to the deleted
	// incarnation (bctx), so it follows that one's tombstone, never the
	// newcomer.
	a.g.acting("DeleteAndWait.wait", name)
	if _, err := a.inner.WaitSettled(bctx, name); err != nil && !errors.Is(err, config.ErrShedNotFoundSentinel) {
		return err
	}
	return nil
}

// AsyncDeleter is implemented by the AsyncLifecycle a Guard returns from
// IsAsync. A delete that waits for its VM to go must use it: once the record
// is gone, WaitSettled on the name finds nothing it may authorize.
type AsyncDeleter interface {
	DeleteAndWait(ctx, jobCtx context.Context, name string) error
}

// EgressController is the optional capability for live egress changes. The
// Firecracker and VZ backends implement it; a backend that does not gives a
// 501 at the API.
type EgressController interface {
	SetShedEgress(ctx context.Context, name string, profiles []string) (*config.Shed, error)
	ClearShedEgress(ctx context.Context, name string) (*config.Shed, error)
}

// AsEgressController returns b as an EgressController when it is one. Through
// a Guard it returns a guarded controller: both changes need the right to
// enter the shed.
func AsEgressController(b Backend) (EgressController, bool) {
	if g, ok := b.(*Guard); ok {
		ec, ok := AsEgressController(g.inner)
		if !ok {
			return nil, false
		}
		return guardedEgress{g: g, inner: ec}, true
	}
	ec, ok := b.(EgressController)
	return ec, ok
}

type guardedEgress struct {
	g     *Guard
	inner EgressController
}

func (e guardedEgress) SetShedEgress(ctx context.Context, name string, profiles []string) (*config.Shed, error) {
	bctx, _, err := e.g.bind(ctx, "SetShedEgress", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	return e.g.showFor(ctx)(e.inner.SetShedEgress(bctx, name, profiles))
}

func (e guardedEgress) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	bctx, _, err := e.g.bind(ctx, "ClearShedEgress", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	return e.g.showFor(ctx)(e.inner.ClearShedEgress(bctx, name))
}

// Provisioner is the optional capability behind `shed provision`: deliver a
// running shed's credentials afresh and finish its owed steps (stage 1, the
// clone, stage 2, the install hook, in that order). Every backend implements
// it; a backend that does not gives a 501 at the API.
//
// BeginProvision admits one provision: it takes the shed's one provisioning
// slot and checks the shed, before any work (so a caller can refuse with a
// plain status before it opens a stream). It returns
// config.ErrProvisionBusySentinel while a provision of the shed (or its
// create's delivery, or on the remote backend a job for it) is in flight,
// and config.ErrShedNotRunningSentinel for a shed that is not running. The
// slot is held until the run's Release, which the caller must call.
type Provisioner interface {
	BeginProvision(ctx context.Context, name string) (ProvisionRun, error)
}

// ProvisionRun is an admitted provision.
type ProvisionRun interface {
	// Run does the delivery and returns the shed as it then stands.
	Run(ctx context.Context) (*config.Shed, error)
	// Release gives the shed's provisioning slot back; it may be called
	// more than once.
	Release()
}

// Provision begins, runs and releases one provision of the named shed.
func Provision(ctx context.Context, pv Provisioner, name string) (*config.Shed, error) {
	run, err := pv.BeginProvision(ctx, name)
	if err != nil {
		return nil, err
	}
	defer run.Release()
	return run.Run(ctx)
}

// AsProvisioner returns b as a Provisioner when it is one. Through a Guard it
// returns a guarded one (AuthorizeProvision).
func AsProvisioner(b Backend) (Provisioner, bool) {
	if g, ok := b.(*Guard); ok {
		pv, ok := AsProvisioner(g.inner)
		if !ok {
			return nil, false
		}
		return guardedProvisioner{g: g, inner: pv}, true
	}
	pv, ok := b.(Provisioner)
	return pv, ok
}

type guardedProvisioner struct {
	g     *Guard
	inner Provisioner
}

// BeginProvision needs the right to enter the shed, and more: the caller
// must be the shed's own user (AuthorizeProvision). The run it returns
// reports the shed as the caller may see it.
func (pv guardedProvisioner) BeginProvision(ctx context.Context, name string) (ProvisionRun, error) {
	bctx, shed, err := pv.g.bind(ctx, "BeginProvision", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	p, _ := PrincipalFrom(ctx)
	if err := AuthorizeProvision(p, shed); err != nil {
		return nil, err
	}
	run, err := pv.inner.BeginProvision(bctx, name)
	ref := pv.g.admittedRef(bctx, name, shed)
	pv.g.record(ctx, p, audit.EventProvisionStart, ref, err)
	if err != nil {
		return nil, err
	}
	return guardedRun{g: pv.g, inner: run, createdAt: shed.CreatedAt, owner: shed.Owner, user: shed.User,
		p: p, actor: actor(ctx, p), ref: ref}, nil
}

// admittedRef is the shed's reference for a provision's events, read after
// admission: a Firecracker or VZ shed made before shed ids is given its id
// there, and the delivery's own event carries it, so the start and end
// events must too (else a filter by the new id misses them). The lookup is
// made only for a shed that had no id, on ctx bound to the incarnation
// authorized; if it fails, the shed as authorized stands.
func (g *Guard) admittedRef(ctx context.Context, name string, shed *config.Shed) audit.ShedRef {
	ref := shedRef(shed)
	if shed.ID != "" {
		return ref
	}
	if now, err := g.inner.GetShed(ctx, name); err == nil && now != nil && now.CreatedAt.Equal(shed.CreatedAt) {
		ref.ShedID = now.ID
	}
	return ref
}

// guardedRun is an admitted provision run behind the guard. Run's context
// (its cancellation and progress are the caller's) is bound to the shed the
// admission authorized, as bind binds every guarded call: its creation time
// and its reported owner and user. So nothing the run reads or acts on, its
// final lookup included, can be a shed deleted and created again under the
// name meanwhile, or one whose user changed.
type guardedRun struct {
	g           *Guard
	inner       ProvisionRun
	createdAt   time.Time
	owner, user string
	// p and actor are who was admitted, and ref the shed as it was then,
	// for the run's events.
	p     Principal
	actor audit.Identity
	ref   audit.ShedRef
}

// Run carries the admitted caller as its actor, so the delivery's event
// names them, and writes the provision's end event.
func (r guardedRun) Run(ctx context.Context) (*config.Shed, error) {
	ctx = audit.WithActor(ctx, r.actor)
	bctx := PinOwnership(PinCreatedAt(ctx, r.createdAt), r.owner, r.user)
	shed, err := r.inner.Run(bctx)
	r.g.record(ctx, r.p, audit.EventProvisionEnd, r.ref, err)
	return r.g.showFor(ctx)(shed, err)
}

func (r guardedRun) Release() { r.inner.Release() }

// AuthorizeProvision is the rule for `shed provision`, applied after the
// right to enter the shed (as the shed was reported): only the person whose
// shed it is may cause a delivery to it, because the delivery asks that
// person's own source. That is the shed's user, or the implicit user of a
// server with no user list for a shed with no user. A service client's shed
// is provisioned by nobody, an admin provisions no one else's shed (not even
// a service client's, which an admin may enter), a script key is automation
// and not a person, and the server never provisions on its own.
func AuthorizeProvision(p Principal, shed *config.Shed) error {
	switch {
	case p.Kind == PrincipalClient:
		return fmt.Errorf("%w: a service client cannot provision a shed", config.ErrForbiddenSentinel)
	case p.Kind != PrincipalUser:
		return fmt.Errorf("%w: only the shed's user can provision it", config.ErrForbiddenSentinel)
	case p.Script:
		return fmt.Errorf("%w: a script key cannot provision a shed; its user can, with a key of their own", config.ErrForbiddenSentinel)
	case shed.Owner != "":
		return fmt.Errorf("%w: shed %s belongs to a service client; nobody provisions it", config.ErrForbiddenSentinel, shed.Name)
	case shed.User != p.User:
		return fmt.Errorf("%w: only the shed's user can provision shed %s", config.ErrForbiddenSentinel, shed.Name)
	}
	return nil
}

// Extender is the optional capability behind `shed extend`: move a cloud
// shed's lease later by whole hours (1 to config.MaxExtendHours). The remote
// backend implements it; a backend that does not gives a 501 at the API.
//
// requestID, when not empty, is the compute contract's request id to send:
// the same id for the same extend, so a repeat moves the lease once. Through
// a Guard it is derived from the caller's own id (ExtendRequestID); empty
// means a new extend, for which the backend makes one.
type Extender interface {
	ExtendShed(ctx context.Context, name string, hours int, requestID string) (*config.Shed, error)
}

// ExtendRequestID is the compute contract's request id for an extend the
// caller p asked for with its own id callerID, on the shed of id shedID (or,
// for a shed with none, its name and creation time): a UUID derived from all
// three, so the same caller retrying the same extend sends the same id, and
// another shed or another caller never does.
func ExtendRequestID(shed *config.Shed, p Principal, callerID string) string {
	key := shed.ID
	if key == "" {
		key = shed.Name + "@" + shed.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte("shed-extend\x00" + key + "\x00" + string(p.Kind) + ":" + p.String() + "\x00" + strings.ToLower(callerID)))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x80 // version 8: a name-derived UUID
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// AsExtender returns b as an Extender when it is one. Through a Guard it
// returns a guarded one: a service client and a script key are refused
// before any lookup (AuthorizeExtend), then the shed is bound with
// ActionExtend (its own user, or an admin).
func AsExtender(b Backend) (Extender, bool) {
	if g, ok := b.(*Guard); ok {
		ex, ok := AsExtender(g.inner)
		if !ok {
			return nil, false
		}
		return guardedExtender{g: g, inner: ex}, true
	}
	ex, ok := b.(Extender)
	return ex, ok
}

type guardedExtender struct {
	g     *Guard
	inner Extender
}

// ExtendShed needs the right to extend the shed (ActionExtend), and writes a
// shed.extend event. callerID is the caller's own request id ("" for none),
// from which the id sent is derived (ExtendRequestID).
func (e guardedExtender) ExtendShed(ctx context.Context, name string, hours int, callerID string) (*config.Shed, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := AuthorizeExtend(p); err != nil {
		return nil, err
	}
	bctx, shed, err := e.g.bind(ctx, "ExtendShed", name, ActionExtend)
	if err != nil {
		return nil, err
	}
	rid := ""
	if callerID != "" {
		rid = ExtendRequestID(shed, p, callerID)
	}
	extended, err := e.inner.ExtendShed(withActor(bctx, p), name, hours, rid)
	e.g.recordOn(ctx, audit.EventShedExtend, shed, err)
	return e.g.showFor(ctx)(extended, err)
}

// CloudUsager is the optional capability behind GET /api/cloud/usage: the
// machines a person and the organization hold against the account
// service's limits. user is whose to count ("" for the organization only).
// The remote backend implements it; a backend that does not gives a 501.
type CloudUsager interface {
	CloudUsage(ctx context.Context, user string) (config.CloudUsage, error)
	// ServicePerson reports whether name is a person of the account
	// service (not a local admin): who may name another user's usage.
	ServicePerson(name string) bool
}

// AsCloudUsager returns b as a CloudUsager when it is one. Through a Guard it
// returns a guarded one, whose user is the ?user= the caller named, judged by
// AuthorizeUsage.
func AsCloudUsager(b Backend) (CloudUsager, bool) {
	if g, ok := b.(*Guard); ok {
		cu, ok := AsCloudUsager(g.inner)
		if !ok {
			return nil, false
		}
		return guardedUsage{inner: cu}, true
	}
	cu, ok := b.(CloudUsager)
	return cu, ok
}

type guardedUsage struct {
	inner CloudUsager
}

// CloudUsage counts whom AuthorizeUsage lets the caller see: asked is the
// user the caller named ("" for none).
// ServicePerson is the wrapped backend's answer.
func (u guardedUsage) ServicePerson(name string) bool { return u.inner.ServicePerson(name) }

func (u guardedUsage) CloudUsage(ctx context.Context, asked string) (config.CloudUsage, error) {
	p, err := principal(ctx)
	if err != nil {
		return config.CloudUsage{}, err
	}
	user, err := AuthorizeUsage(p, asked, u.inner.ServicePerson)
	if err != nil {
		return config.CloudUsage{}, err
	}
	return u.inner.CloudUsage(ctx, user)
}
