package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/lockmap"
)

// The delivery steps on a Firecracker or VZ shed, at create and by `shed
// provision`: the delivery runner over a host that takes the shed's
// lifecycle lock for each step (never across the wait for a source) and
// checks under it, before the step, that the shed is still the one the run
// began on (its id and creation time), still running, that no stop or delete
// is waiting for the lock (they go first), and that its delivery record is
// the one this run last saw (a reset in between ends the run). A step that
// finds any of that changed ends the run without saving, and an answer that
// arrived for it is dropped. One run per shed at a time: the backend's
// provisioning slot, held by a create for its whole delivery and by a
// provision from before its stream opens.

// LocalShed is a shed as a run reads it, under its lifecycle lock.
type LocalShed struct {
	// Shed is the shed as reported, its status reconciled with its VM.
	Shed *config.Shed
	// ID is the shed's permanent id.
	ID string
	// Record is the shed's delivery record as persisted (nil: none, a shed
	// from before delivery).
	Record *delivery.Record
	// ResetGen is the shed's reset generation as persisted: `shed reset`
	// bumps it. A run ends at a step that finds it changed, whatever the
	// record says (a reset of an all-owed record leaves the record as it
	// was).
	ResetGen uint64
	// Request is the shed as a delivery request names it.
	Request delivery.RequestShed
	// Origin is the shed's recorded user origin (user_origin): "local",
	// "directory", or "" for a shed from before it.
	Origin string
	// Repo is the repository URL as stored (the clone's).
	Repo string
	// CloneEnv is the environment the clone runs with besides the server's
	// own: the shed's egress proxy settings, rebuilt from its persisted
	// assignment (nil when egress is off for it).
	CloneEnv []string
	// Writer is the shed's agent, which the stages write through.
	Writer delivery.Agent
	// Clone is the clone step (vmutil.CloneStep with CloneEnv); withheld
	// keeps the server's env_file variables out of it.
	Clone func(ctx context.Context, withheld bool) delivery.Step
	// Hooks runs the shed's hooks as the install step
	// (vmutil.Provisioner.HooksStep, in its landing directory).
	Hooks func(ctx context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool)
	// Save persists rec into the metadata this was read from, durably.
	Save func(rec *delivery.Record) error
}

// LocalBackend is what a delivery run needs of a Firecracker or VZ backend.
type LocalBackend interface {
	// LockShed takes the shed's lifecycle lock; the caller runs the
	// returned unlock.
	LockShed(name string) (unlock func())
	// LoadShed reads the shed under its lock (held by the caller), checks it
	// against what ctx requires (backend.CheckIdentity), and reconciles its
	// status. With mintID, a shed that has no permanent id (one created
	// before delivery) is given one, saved before LoadShed returns.
	LoadShed(ctx context.Context, name string, mintID bool) (*LocalShed, error)
	// Halting reports whether a stop or a delete of the shed is waiting
	// for its lock: the run's next step gives way to it.
	Halting(name string) bool
}

// errRecordChanged ends a run whose shed's delivery record changed under it
// (a reset between two steps): its answer is for a disk that is gone.
var errRecordChanged = errors.New("the shed's delivery record changed during the run (it was reset)")

// LocalRun is one admitted delivery run on a local shed: a provision begun
// by BeginProvision, or a create's delivery (CreateShed's pending run). It
// holds the shed's provisioning slot until Release.
type LocalRun struct {
	lb      LocalBackend
	svc     *delivery.Service
	name    string
	reason  delivery.Reason
	first   *LocalShed
	release func()
	once    sync.Once

	// A create's: its request's flags, the baseline taken under its lock,
	// and the backend's own clone and hooks (with the create's repository
	// and server-env rule).
	scriptKey, fromSnapshot, noProvision bool
	base                                 *runBase
	// ticket is the run's admission (delivery.Service.Admit), taken with
	// the shed's provisioning slot: a stop or delete after it ends the run.
	ticket delivery.Ticket
	clone  func(ctx context.Context, ls *LocalShed) delivery.Step
	hooks  func(ctx context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool)
}

// runBase is what every step of a run is checked against: the shed's id,
// creation time and reset generation, and the record the run last saw.
type runBase struct {
	id        string
	createdAt time.Time
	gen       uint64
	rec       *delivery.Record
}

// BeginProvision admits `shed provision` on a running local shed: it takes
// the shed's one provisioning slot (config.ErrProvisionBusySentinel when a
// provision, or a create's delivery, holds it) and checks the shed is
// running, giving a shed from before delivery its permanent id. The slot is
// held until the run's Release; Run does the work.
func BeginProvision(ctx context.Context, lb LocalBackend, svc *delivery.Service, busy *lockmap.NamedMutexMap, name string) (*LocalRun, error) {
	release, ok := busy.TryAcquire(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrProvisionBusySentinel, name)
	}
	unlock := lb.LockShed(name)
	ls, err := lb.LoadShed(ctx, name, true)
	if err == nil && ls.Shed.Status != config.StatusRunning {
		err = fmt.Errorf("%w: %s is %s; start it first", config.ErrShedNotRunningSentinel, name, ls.Shed.Status)
	}
	if err == nil {
		// A shed whose user's name has changed origin since it was made is
		// refused before anything is asked (409 SHED_ORIGIN_CHANGED).
		_, user := config.ShedOwnership(ls.Shed.Owner, ls.Shed.User)
		err = svc.CheckOrigin(user, ls.Origin)
	}
	var ticket delivery.Ticket
	if err == nil {
		// The run's admission, under the lifecycle lock a stop or delete
		// takes before it ends the shed's runs.
		ticket = svc.Admit(name, ls.ID)
	}
	unlock()
	if err != nil {
		release()
		return nil, err
	}
	return &LocalRun{lb: lb, svc: svc, name: name, reason: delivery.ReasonProvision, first: ls, release: release, ticket: ticket}, nil
}

// Release gives the shed's provisioning slot back. It is safe to call more
// than once.
func (r *LocalRun) Release() {
	r.once.Do(func() {
		if r.release != nil {
			r.release()
		}
	})
}

// Run runs the delivery steps and returns the shed as it then stands. A
// provision's error ends it (the shed changed, stopped, went or was reset);
// see Execute's doc for what is kept.
func (r *LocalRun) Run(ctx context.Context) (*config.Shed, error) {
	ls := r.first
	_, user := config.ShedOwnership(ls.Shed.Owner, ls.Shed.User)
	base := runBase{id: ls.ID, createdAt: ls.Shed.CreatedAt, gen: ls.ResetGen, rec: ls.Record}
	if r.base != nil {
		base = *r.base
	}
	h := &localHost{
		lb: r.lb, name: r.name, id: base.id, createdAt: base.createdAt, gen: base.gen, last: base.rec.Clone(), cur: ls,
		clone: r.clone, hooks: r.hooks,
	}
	run := &delivery.Run{
		Service: r.svc,
		Reason:  r.reason,
		Params: delivery.Params{
			User: user, Shed: ls.Request, Owner: ls.Shed.Owner, ServiceClient: ls.Shed.Owner != "",
			ScriptKey: r.scriptKey, FromSnapshot: r.fromSnapshot, Origin: ls.Origin,
		},
		HasRepo:     ls.Repo != "",
		NoProvision: r.noProvision,
		Ticket:      r.ticket,
		Prior:       ls.Record,
		Host:        h,
		Report:      backend.DeliveryProgress(ctx),
	}
	_, err := run.Execute(ctx)
	if errors.Is(err, delivery.ErrShedPending) {
		err = fmt.Errorf("%w: %s", config.ErrProvisionBusySentinel, r.name)
	}
	unlock := r.lb.LockShed(r.name)
	defer unlock()
	now, lerr := r.lb.LoadShed(ctx, r.name, false)
	if lerr != nil {
		if err == nil {
			err = lerr
		}
		return nil, err
	}
	return now.Shed, err
}

// localHost is a run's delivery host on a local shed.
type localHost struct {
	lb        LocalBackend
	name, id  string
	createdAt time.Time
	gen       uint64
	// cur is the shed as the step in hand read it; last is the record this
	// run last saw persisted (loaded at its start, or saved by it).
	cur  *LocalShed
	last *delivery.Record
	// clone and hooks, when set, replace the shed's own (a create's).
	clone func(ctx context.Context, ls *LocalShed) delivery.Step
	hooks func(ctx context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool)
}

func (h *localHost) Step(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx.Err() != nil {
		// Its cause, so a run its shed's stop or delete ended reports
		// that (ErrShedEnded), not a bare cancellation.
		return context.Cause(ctx)
	}
	unlock := h.lb.LockShed(h.name)
	defer unlock()
	ls, err := h.lb.LoadShed(ctx, h.name, false)
	if err != nil {
		return err
	}
	switch {
	case ls.ID != h.id || !ls.Shed.CreatedAt.Equal(h.createdAt):
		return fmt.Errorf("%w: %s (deleted and created again during the delivery)", config.ErrShedNotFoundSentinel, h.name)
	case ls.Shed.Status != config.StatusRunning || h.lb.Halting(h.name):
		return fmt.Errorf("%w: %s stopped during the delivery", config.ErrShedNotRunningSentinel, h.name)
	case ls.ResetGen != h.gen || !delivery.SameRecord(ls.Record, h.last):
		log.Printf("delivery: shed %s: %v; the delivery ends here", h.name, errRecordChanged)
		return fmt.Errorf("%w: %s: %w", config.ErrShedNotRunningSentinel, h.name, errRecordChanged)
	}
	h.cur = ls
	return fn(ctx)
}

// Save persists rec, and remembers it as what this run last saw.
func (h *localHost) Save(_ context.Context, rec *delivery.Record) error {
	if err := h.cur.Save(rec); err != nil {
		return err
	}
	h.last = rec.Clone()
	return nil
}

func (h *localHost) Agent() delivery.Agent { return h.cur.Writer }

// Clone clones the shed's repository unless something stands at its project
// directory; it never removes anything (vmutil.CloneStep). At create the
// backend's own clone runs (the create's request, egress env and env rule);
// under provision the shed's, with its egress proxy env rebuilt, and the
// server's env_file variables only for an admin's own shed, as at create.
func (h *localHost) Clone(ctx context.Context, _ bool) delivery.Step {
	if h.clone != nil {
		return h.clone(ctx, h.cur)
	}
	p, _ := backend.PrincipalFrom(ctx)
	withheld := h.cur.Shed.Owner != "" || !p.IsAdmin()
	backend.Phase(ctx, "repo")
	backend.Status(ctx, "Cloning repository...")
	return h.cur.Clone(ctx, withheld)
}

func (h *localHost) Hooks(ctx context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool) {
	if h.hooks != nil {
		return h.hooks(ctx, install, startup)
	}
	return h.cur.Hooks(ctx, install, startup)
}
