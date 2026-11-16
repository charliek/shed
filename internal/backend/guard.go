package backend

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

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

// Inner returns the backend g wraps. Only code that acts as the server
// itself (start-up passes, the credential bus) should call it directly.
func (g *Guard) Inner() Backend { return g.inner }

// SetBeforeAct installs a function run after every authorization and before
// the wrapped backend is called, with the operation's name and the shed's or
// snapshot's name. For tests that force an interleaving; set it before use.
func (g *Guard) SetBeforeAct(fn func(op, name string)) { g.beforeAct = fn }

// unwrap returns the backend a Guard wraps, or b itself.
func unwrap(b Backend) Backend {
	if g, ok := b.(*Guard); ok {
		return g.inner
	}
	return b
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
	return shownTo(ctx, shed), nil
}

// shownTo returns shed as the principal on ctx may see it. Every shed record
// a Guard returns goes through it. A caller who may not enter the shed (an
// admin looking at another user's shed) gets a copy with no repository URL
// and no landing directory at all. A clone URL can carry a token anywhere (its
// userinfo, its query, its path), and the landing directory is derived from
// the URL, so no redaction of either is safe: both are withheld. The shed's
// own user, the implicit user, the server and anyone else who may enter it see
// the record unchanged. The backend's record is never changed.
func shownTo(ctx context.Context, shed *config.Shed) *config.Shed {
	if shed == nil || (shed.Repo == "" && shed.LandingDir == "") {
		return shed
	}
	p, _ := PrincipalFrom(ctx)
	if Authorize(p, shed.Owner, shed.User, ActionEnter) == nil {
		return shed
	}
	cp := *shed
	cp.Repo, cp.LandingDir = "", ""
	return &cp
}

// showFor returns shownTo for ctx in the shape of a call that returns a shed
// and an error, so a guarded method can pass the wrapped call's results
// straight through it.
func showFor(ctx context.Context) func(*config.Shed, error) (*config.Shed, error) {
	return func(shed *config.Shed, err error) (*config.Shed, error) {
		if err != nil {
			return shed, err
		}
		return shownTo(ctx, shed), nil
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
	shed, err := g.inner.CreateShed(ctx, req)
	if err != nil && errors.Is(err, config.ErrShedAlreadyExistsSentinel) && !p.unrestricted() {
		// Names are unique per server, across users. A taken name is the
		// usual conflict, worded the same whoever holds it, so it tells the
		// caller nothing of whose the shed is or what it is like.
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}
	return showFor(ctx)(shed, err)
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
	return showFor(ctx)(shed, err)
}

// ListSheds returns the sheds the principal may view, each as shownTo gives
// it.
func (g *Guard) ListSheds(ctx context.Context) ([]config.Shed, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	sheds, err := g.inner.ListSheds(ctx)
	if err != nil || p.unrestricted() {
		return sheds, err
	}
	out := make([]config.Shed, 0, len(sheds))
	for i := range sheds {
		if Authorize(p, sheds[i].Owner, sheds[i].User, ActionView) == nil {
			out = append(out, *shownTo(ctx, &sheds[i]))
		}
	}
	return out, nil
}

// DeleteShed needs the right to manage the shed.
func (g *Guard) DeleteShed(ctx context.Context, name string) error {
	bctx, _, err := g.bind(ctx, "DeleteShed", name, ActionManage)
	if err != nil {
		return err
	}
	return g.inner.DeleteShed(bctx, name)
}

// StartShed needs the right to enter the shed.
func (g *Guard) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	bctx, _, err := g.bind(ctx, "StartShed", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	return showFor(ctx)(g.inner.StartShed(bctx, name))
}

// StopShed needs the right to manage the shed.
func (g *Guard) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	bctx, _, err := g.bind(ctx, "StopShed", name, ActionManage)
	if err != nil {
		return nil, err
	}
	return showFor(ctx)(g.inner.StopShed(bctx, name))
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
	bctx, _, err := g.bind(ctx, "AssignUser", name, ActionManage)
	if err != nil {
		return nil, err
	}
	return showFor(ctx)(g.inner.AssignUser(bctx, name, req))
}

// ResetShed needs the right to enter the shed.
func (g *Guard) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	bctx, _, err := g.bind(ctx, "ResetShed", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	return showFor(ctx)(g.inner.ResetShed(bctx, name))
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
	return showFor(ctx)(a.inner.WaitSettled(bctx, name))
}

// DeleteAndWait deletes the named shed and waits until its VM is destroyed.
// The record is gone as soon as the delete is acknowledged, so the wait is
// not authorized again: it is authorized once, for the delete, and follows
// the incarnation the delete acted on, through its tombstone. A destroy that
// failed is reported (the wrapped backend's error), never as success.
func (a guardedAsync) DeleteAndWait(ctx, jobCtx context.Context, name string) error {
	bctx, shed, err := a.g.bind(ctx, "DeleteShed", name, ActionManage)
	if err != nil {
		return err
	}
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
	return showFor(ctx)(e.inner.SetShedEgress(bctx, name, profiles))
}

func (e guardedEgress) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	bctx, _, err := e.g.bind(ctx, "ClearShedEgress", name, ActionEnter)
	if err != nil {
		return nil, err
	}
	return showFor(ctx)(e.inner.ClearShedEgress(bctx, name))
}
