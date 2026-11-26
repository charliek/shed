package remote

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/vmutil"
)

// Delivery on a cloud shed: the create job's tracked steps and `shed
// provision`, both through delivery.Run.
//
// The record's steps are saved after each, stage 1's outcome before any
// later step starts. A create resumed after a server restart (or a cloud
// read that failed) has lost its bundle and reads its record only (plan 040,
// section 3.3): it asks no source and probes nothing, and never rewrites a
// step already recorded. With stage 1 not recorded it runs nothing, and
// `shed provision` finishes the shed. With stage 1 recorded the clone runs
// if it is owed, stage 2 is skipped when stage 1 was not applicable and
// stays owed otherwise (and so does the install hook). A resumed job never
// retries a step recorded failed.

var _ backend.Provisioner = (*Backend)(nil)

// SetDelivery attaches the delivery service, called by shed-server before
// the start-up pass. nil leaves every delivery not applicable.
func (b *Backend) SetDelivery(s *delivery.Service) { b.deliverySvc = s }

// HoldResumes makes every create resumed by the start-up pass wait, before
// its delivery steps, until ReleaseResumes: shed-server calls it before
// Reconcile, and ReleaseResumes once the origin migration
// (backend.MigrateUserOrigins) has run, so a create from before user_origin
// is resumed with its shed's origin recorded. Unset, nothing waits.
func (b *Backend) HoldResumes() {
	b.resumeGate = make(chan struct{})
}

// ReleaseResumes lets the held resumed creates run (HoldResumes). It may be
// called more than once.
func (b *Backend) ReleaseResumes() {
	if b.resumeGate != nil {
		b.resumeOnce.Do(func() { close(b.resumeGate) })
	}
}

// requestShed is rec's shed as a delivery request names it.
func requestShed(rec *state.Record) delivery.RequestShed {
	return config.DeliveryRequestShed(rec.Shed.Name, rec.Shed.ID, delivery.BackendRemote, rec.Shed.Repo, "", rec.Shed.LandingDir)
}

// createSteps runs a create's tracked steps. The job the create request
// started runs them (j.deliver); a resumed one runs those its record allows
// (delivery.Run with Resumed). It returns errSuperseded when the job's
// context ended or the shed went.
func (b *Backend) createSteps(ctx context.Context, j *job, rec *state.Record, agent *vmutil.AgentClient, owned bool) error {
	if !j.deliver && b.resumeGate != nil {
		// A resumed create waits for the start-up's origin migration (a
		// record from before user_origin is stamped by it), so its steps
		// are judged with the shed's origin, never refused for lack of one
		// and the shed marked ready with its work skipped.
		select {
		case <-b.resumeGate:
		case <-ctx.Done():
			return errSuperseded
		}
		if rec = b.snapshot(j.name, j.id); rec == nil {
			return errSuperseded
		}
	}
	owner, user := ownership(rec)
	params := delivery.Params{
		User: user, Shed: requestShed(rec), Owner: owner, ServiceClient: owner != "", ScriptKey: rec.ScriptKey,
		Origin: rec.UserOrigin,
	}
	resumed := !j.deliver
	if resumed && stageOneOwed(rec) {
		// A record from before delivery: its old flags say what ran.
		if rec.Delivery == nil {
			steps := rec.LegacySteps()
			if steps == nil {
				steps = delivery.NewRecord().Steps
			}
			b.update(j.name, j.id, func(r *state.Record) {
				if r.Delivery == nil {
					d := &delivery.Record{Steps: steps}
					d.State = delivery.DeriveState(d)
					r.Delivery = d
				}
			})
		}
		b.logf("remote: create of shed %s resumed: its delivery was not decided before it stopped, so no step runs; `shed provision %s` finishes it", j.name, j.name)
		return nil
	}
	repo := j.repoURL
	if repo == "" {
		repo = rec.Shed.Repo
	}
	if j.hasActor {
		ctx = audit.WithActor(ctx, j.actor)
	}
	if resumed {
		b.logf("remote: create of shed %s resumed: its recorded delivery stands; the steps it allows run", j.name)
	}
	h := &remoteHost{
		b: b, name: j.name, id: j.id, agent: agent, rec: rec, owner: owner, user: user,
		last: rec.Delivery.Clone(), repo: repo, withheld: owned || rec.NoServerEnv,
	}
	run := &delivery.Run{
		Service:     b.deliverySvc,
		Reason:      delivery.ReasonCreate,
		Params:      params,
		HasRepo:     rec.Shed.Repo != "",
		NoProvision: rec.NoProvision,
		Resumed:     resumed,
		Ticket:      j.ticket,
		Prior:       rec.Delivery,
		Initial:     rec.LegacySteps(),
		Host:        h,
		Report:      backend.DeliveryProgress(ctx),
	}
	_, err := run.Execute(ctx)
	switch {
	case ctx.Err() != nil || errors.Is(err, errSuperseded):
		return errSuperseded
	case err != nil:
		b.logf("Warning: create of shed %s: the delivery steps ended early: %v", j.name, err)
	}
	return nil
}

// stageOneOwed reports whether rec's stage 1 is not recorded yet (a record
// from before delivery counts as owed: its stage 1 never ran).
func stageOneOwed(rec *state.Record) bool {
	return rec.Delivery == nil || rec.Delivery.Steps == nil || rec.Delivery.Steps.Stage1.State == delivery.StepOwed
}

// BeginProvision admits `shed provision` on a running cloud shed
// (backend.Provisioner): it takes the shed's provisioning slot and is
// refused while a job for the shed is in flight. The slot is held until the
// run's Release. Each step then takes the shed's step lock, which a stop or a
// delete waits for, and checks first that the shed is the same one (its id),
// with the owner and user it was authorized with, still running, with no
// job in flight, no stop or delete waiting, and the delivery record this run
// last saw.
func (b *Backend) BeginProvision(ctx context.Context, name string) (backend.ProvisionRun, error) {
	release, ok := b.provisioning.TryAcquire(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrProvisionBusySentinel, name)
	}
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	switch {
	case err != nil:
	case r.Job != "" || b.jobs[name] != nil:
		err = fmt.Errorf("%w: %s is %s; try again when it has settled", config.ErrProvisionBusySentinel, name, r.Shed.Status)
	case r.Shed.Status != config.StatusRunning:
		err = fmt.Errorf("%w: %s is %s; start it first", config.ErrShedNotRunningSentinel, name, r.Shed.Status)
	default:
		// A shed whose user's name has changed origin since it was made is
		// refused before anything is asked (409 SHED_ORIGIN_CHANGED).
		_, user := ownership(r)
		err = b.deliverySvc.CheckOrigin(user, r.UserOrigin)
	}
	var rec *state.Record
	var ticket delivery.Ticket
	if err == nil {
		rec = r.Clone()
		// The run's admission, under the lock a stop or delete takes to
		// end it.
		ticket = b.deliverySvc.Admit(name, rec.Shed.ID)
	}
	b.mu.Unlock()
	if err != nil {
		release()
		return nil, err
	}
	return &remoteRun{b: b, name: name, rec: rec, release: release, ticket: ticket}, nil
}

// ProvisionShed is BeginProvision, Run and Release in one call.
func (b *Backend) ProvisionShed(ctx context.Context, name string) (*config.Shed, error) {
	return backend.Provision(ctx, b, name)
}

// remoteRun is an admitted provision of a cloud shed.
type remoteRun struct {
	b       *Backend
	name    string
	rec     *state.Record
	release func()
	once    sync.Once
	// ticket is the run's admission, taken with the provisioning slot.
	ticket delivery.Ticket
}

func (r *remoteRun) Release() { r.once.Do(r.release) }

func (r *remoteRun) Run(ctx context.Context) (*config.Shed, error) {
	b, rec := r.b, r.rec
	agent, err := b.agentFor(rec)
	if err != nil {
		return nil, err
	}
	owner, user := ownership(rec)
	h := &remoteHost{
		b: b, name: r.name, id: rec.Shed.ID, agent: agent, rec: rec, owner: owner, user: user,
		last: rec.Delivery.Clone(), repo: rec.Shed.Repo, withheld: owner != "" || rec.NoServerEnv, provision: true,
	}
	run := &delivery.Run{
		Service: b.deliverySvc,
		Reason:  delivery.ReasonProvision,
		Params: delivery.Params{
			User: user, Shed: requestShed(rec), Owner: owner, ServiceClient: owner != "",
			Origin: rec.UserOrigin,
		},
		HasRepo:     rec.Shed.Repo != "",
		NoProvision: rec.NoProvision,
		Ticket:      r.ticket,
		Prior:       rec.Delivery,
		Initial:     rec.LegacySteps(),
		Host:        h,
		Report:      backend.DeliveryProgress(ctx),
	}
	if _, err := run.Execute(ctx); err != nil {
		if errors.Is(err, delivery.ErrShedPending) {
			return nil, fmt.Errorf("%w: %s", config.ErrProvisionBusySentinel, r.name)
		}
		return nil, err
	}
	return b.GetShed(ctx, r.name)
}

// remoteHost is a delivery host on a cloud shed, for a create job's steps
// or a provision's. Either way each step takes the shed's step lock (a stop
// or a delete waits for the step in hand, never longer) and checks the shed
// under it first.
type remoteHost struct {
	b        *Backend
	name, id string
	agent    *vmutil.AgentClient
	// rec is the record as the step in hand read it.
	rec *state.Record
	// owner and user are the shed's as the run was authorized (a provision)
	// or began (a create): a reconcile that changes them ends the run.
	owner, user string
	// last is the delivery record this run last saw persisted.
	last      *delivery.Record
	repo      string
	withheld  bool
	provision bool
}

// errDeliveryEnded ends a run whose shed stopped, is stopping or changing,
// changed hands, or had its record changed under it, between two steps. Its
// answer, if one came, is dropped.
var errDeliveryEnded = errors.New("the shed stopped, changed hands or is changing")

func (h *remoteHost) Step(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx.Err() != nil {
		// Its cause, so a run its shed's stop or delete ended reports
		// that (ErrShedEnded), not a bare cancellation.
		return context.Cause(ctx)
	}
	defer h.b.stepLocks.Acquire(h.name)()
	h.b.mu.Lock()
	r := h.b.currentLocked(h.name, h.id)
	if r == nil {
		h.b.mu.Unlock()
		if !h.provision {
			return errSuperseded
		}
		return fmt.Errorf("%w: %s (deleted during the provision)", config.ErrShedNotFoundSentinel, h.name)
	}
	owner, user := ownership(r)
	var why string
	switch {
	case owner != h.owner || user != h.user:
		why = "its owner or user changed"
	case h.b.halting.Raised(h.name):
		why = "a stop or delete is waiting"
	case !delivery.SameRecord(r.Delivery, h.last):
		why = "its delivery record changed"
	case h.provision && (r.Shed.Status != config.StatusRunning || r.Job != "" || h.b.jobs[h.name] != nil):
		why = "it is not running"
	case !h.provision && r.Queued == state.QueuedStop:
		why = "a stop is queued"
	}
	if why != "" {
		h.b.mu.Unlock()
		h.b.logf("remote: delivery to shed %s ends: %s", h.name, why)
		return fmt.Errorf("%w: %s: %w", config.ErrShedNotRunningSentinel, h.name, errDeliveryEnded)
	}
	h.rec = r.Clone()
	h.b.mu.Unlock()
	return fn(ctx)
}

// Save writes rec into the shed's record durably, and fails (ending the run)
// when the write did not put it in place, or put it in place without the
// flush that makes it durable.
func (h *remoteHost) Save(_ context.Context, rec *delivery.Record) error {
	err := h.b.updateDurable(h.name, h.id, func(r *state.Record) { r.Delivery = rec.Clone() })
	switch {
	case errors.Is(err, errSuperseded):
		return err
	case errors.Is(err, state.ErrNotDurable):
		// In place: this run saw it, but it is not durable; the run stops.
		h.last = rec.Clone()
		h.b.logf("remote: delivery to shed %s ends: its record could not be made durable", h.name)
		return err
	case err != nil:
		h.b.logf("remote: delivery to shed %s ends: its record could not be written", h.name)
		return err
	}
	h.last = rec.Clone()
	return nil
}
func (h *remoteHost) Agent() delivery.Agent { return h.b.writer(h.agent) }

// Clone is the clone step. At create a failed attempt's leftovers are
// removed before the next attempt; under provision nothing is ever removed.
func (h *remoteHost) Clone(ctx context.Context, provision bool) delivery.Step {
	backend.Phase(ctx, "repo")
	backend.Status(ctx, "Cloning repository...")
	probe := func(ctx context.Context) (vmutil.WorkTree, error) { return h.b.workTree(ctx, h.agent, h.rec.Shed.Repo) }
	return vmutil.CloneStepWith(ctx, probe, h.rec.Shed.Repo, func(ctx context.Context) error {
		return h.b.cloneRepo(ctx, h.agent, h.rec, h.repo, h.withheld, provision)
	})
}

func (h *remoteHost) Hooks(ctx context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool) {
	backend.Phase(ctx, "provision")
	return h.b.hooks(ctx, h.agent, h.rec, install, startup)
}

// waitStep waits for a delivery's step in hand on the shed to finish and
// holds the next one off; the caller runs the returned release when it has
// done its own change. The flag is raised before the wait, so the
// delivery's next step gives way instead of overtaking the waiter.
func (b *Backend) waitStep(name string) func() {
	lower := b.halting.Raise(name)
	unlock := b.stepLocks.Acquire(name)
	return func() {
		unlock()
		lower()
	}
}
