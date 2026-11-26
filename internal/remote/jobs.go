package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/vmutil"
)

const (
	jobDelete = "delete"

	// maxBusyRetries bounds how often a job waits out another transition
	// and calls again.
	maxBusyRetries = 6
	// findInsertAttempts and findInsertInterval bound how long a delete
	// looks for an instance whose insert was in flight when the delete was
	// acknowledged.
	findInsertAttempts = 6
	findInsertInterval = 5 * time.Second

	// cloneAttempts is how many times a create tries the --repo clone.
	cloneAttempts = 3
)

// errSuperseded ends a job whose shed was deleted, or whose server is
// shutting down, without touching the record.
var errSuperseded = errors.New("job superseded")

// errUnresolved ends a job step that decided nothing, because the cloud did
// not say what the shed's instance is doing (see Backend.keep). The record
// keeps its job, and the step is run again later.
var errUnresolved = errors.New("the job is kept and tried again")

// job is background work for one shed. A lifecycle job may run more than one
// step: a stop queued behind a start runs in the same job.
type job struct {
	kind string
	name string
	id   string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// accepted is closed when a create has passed its provider phase.
	accepted     chan struct{}
	acceptedOnce sync.Once
	// repoURL is the clone URL as the create request gave it, credentials
	// included. It is never stored; a create resumed after a restart
	// clones the stored, stripped URL.
	repoURL string
	// deliver is set on the job a create request started: its delivery
	// steps run and its source is asked. Like repoURL it is never stored:
	// a create resumed after a restart has lost its bundle and asks no
	// source on its own. Its unfinished steps stay owed when a source would
	// be asked; when none would be (createSteps), they run.
	deliver bool
	// actor is who made the create (the access guard's actor on the
	// request), for the delivery's audit event; hasActor says there was
	// one. Never stored, like deliver.
	actor    audit.Identity
	hasActor bool
	// ticket is the create's delivery admission (delivery.Service.Admit),
	// taken when the create request admitted the job. Never stored.
	ticket delivery.Ticket

	// fastWindow and fastErr are guarded by Backend.mu. While fastWindow is
	// true the create handler has not answered yet, and a failure is its to
	// report.
	fastWindow bool
	fastErr    error
	// nameDeadline is when a create request's wait for its name ends, set
	// with fastWindow; a resumed create has none and waits nameWait.
	nameDeadline time.Time
	// firstRun marks the job a create request started (not one resumed
	// after a restart or by a pass): its sends end by nameDeadline, and a
	// create it never delivered is refused (see failCreate's rule).
	firstRun bool
	// naming, guarded by Backend.mu, is open while the create decides what
	// holds its name (nameHeld): the create handler waits for it to close.
	naming chan struct{}

	// marker, prior and gone belong to a delete job. gone is set by the
	// job's own goroutine when the instance no longer exists.
	marker *state.Marker
	prior  *job
	gone   bool

	emu    sync.Mutex
	events []backend.ProgressEvent
	notify chan struct{}
}

func (j *job) markAccepted() { j.acceptedOnce.Do(func() { close(j.accepted) }) }

// record appends a progress event and wakes the waiters.
func (j *job) record(ev backend.ProgressEvent) {
	j.emu.Lock()
	j.events = append(j.events, ev)
	close(j.notify)
	j.notify = make(chan struct{})
	j.emu.Unlock()
}

// changed returns a channel closed at the next progress event.
func (j *job) changed() <-chan struct{} {
	j.emu.Lock()
	defer j.emu.Unlock()
	return j.notify
}

// newJobLocked registers a lifecycle job for a shed. The caller launches it
// after releasing Backend.mu.
func (b *Backend) newJobLocked(kind, name, id string) *job {
	ctx, cancel := context.WithCancel(b.rootCtx)
	j := &job{
		kind:     kind,
		name:     name,
		id:       id,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		accepted: make(chan struct{}),
		notify:   make(chan struct{}),
	}
	b.jobs[name] = j
	return j
}

// launch runs a lifecycle job.
func (b *Backend) launch(j *job) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.runJob(j)
	}()
}

// finishLocked wakes WaitIdle.
func (b *Backend) finishLocked() {
	close(b.idle)
	b.idle = make(chan struct{})
}

// runJob runs the job's step, then any step queued behind it.
func (b *Backend) runJob(j *job) {
	defer func() {
		j.cancel()
		b.mu.Lock()
		if b.jobs[j.name] == j {
			delete(b.jobs, j.name)
		}
		b.finishLocked()
		b.mu.Unlock()
		j.markAccepted()
		close(j.done)
	}()

	kind := j.kind
	for kind != "" {
		timer := backend.NewPhaseTimer(fmt.Sprintf("%s name=%s backend=%s", kind, j.name, backend.TypeRemote), nil)
		ctx := backend.ContextWithProgress(j.ctx, backend.TeeProgress(timer.Track, j.record))
		var err error
		switch kind {
		case state.JobCreate:
			err = b.runCreate(ctx, j)
		case state.JobStart:
			err = b.runStart(ctx, j)
		case state.JobStop:
			err = b.runStop(ctx, j)
		case state.JobProbe:
			err = b.runProbe(ctx, j)
		}
		if errors.Is(err, errSuperseded) || j.ctx.Err() != nil {
			b.logf("remote: %s of shed %s ended early: %v", kind, j.name, firstErr(err, j.ctx.Err()))
			return
		}
		if errors.Is(err, errUnresolved) {
			// keep has recorded and logged why. Whatever is queued behind the
			// job stays queued: it runs when the job itself has.
			return
		}
		b.logf("timing: %s", timer.Finish(err))
		kind = b.nextStep(j)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// nextStep turns a queued action into the job's next step, if the shed is in
// the status the action needs.
func (b *Backend) nextStep(j *job) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Only the job the shed is registered to may take the queued action. A
	// job that published "running" and has not left yet can already have a
	// successor (a stop registers its own job), and an action queued since
	// then is the successor's: taking it here would throw it away.
	if b.jobs[j.name] != j {
		return ""
	}
	r := b.currentLocked(j.name, j.id)
	if r == nil {
		return ""
	}
	// With no next step the job gives the shed up in this critical section,
	// so an action queued from now on finds no job and starts its own
	// (healLocked) instead of waiting on one that will never look again.
	next := ""
	defer func() {
		if next == "" {
			delete(b.jobs, j.name)
		}
	}()
	queued := r.Queued
	r.Queued = ""
	switch {
	case queued == state.QueuedStop && (r.Shed.Status == config.StatusRunning || r.Shed.Status == config.StatusStarting):
		// "starting" here is a shed whose probe just failed: its VM runs
		// and its agent cannot be reached. A stop that was asked for
		// still stops the VM; nothing else short of a delete would.
		r.Shed.Status = config.StatusStopping
		r.Shed.StatusReason = ""
		r.Job = state.JobStop
		next = state.JobStop
	case queued == state.QueuedStart && r.Shed.Status == config.StatusStopped:
		// The stop that just ended released its run slot and this start
		// takes it back, under the same lock.
		if err := b.checkStartLocked(); err != nil {
			r.Shed.StatusReason = config.StatusReasonLimit
			break
		}
		r.Shed.Status = config.StatusStarting
		r.Shed.StatusReason = ""
		r.Job = state.JobStart
		next = state.JobStart
	}
	if queued != "" {
		b.saveLocked(r)
	}
	return next
}

// --- provider helpers ---

// verify reads the machine before a call that changes it: a read that fails
// is an *unreadError, so a job can tell "the provider did not answer" from
// an answer.
func (b *Backend) verify(ctx context.Context, ref provider.Ref) error {
	_, err := b.verified(ctx, ref)
	return err
}

// verified is verify, also returning the machine it read.
func (b *Backend) verified(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	inst, err := b.prov.Get(ctx, ref)
	if err != nil {
		if unread(err) {
			return provider.Instance{}, &unreadError{err}
		}
		return provider.Instance{}, err
	}
	return inst, nil
}

// unread reports whether err, from a read of one machine, means the read
// itself failed, so that nothing is known about the machine. "It is not
// there" is an answer, not a failure.
func unread(err error) bool {
	return err != nil && !errors.Is(err, provider.ErrNotFound)
}

// unreadError is verify's error when its read of the machine failed. A job
// may give up a mutation for it, and must never stop or destroy anything
// because of it.
type unreadError struct{ err error }

func (e *unreadError) Error() string { return e.err.Error() }
func (e *unreadError) Unwrap() error { return e.err }

func isUnread(err error) bool {
	var u *unreadError
	return errors.As(err, &u)
}

// retryUnknown runs a call that changes something with one request id (rid,
// or a new one when it is empty), and sends it again with the same id when
// its reply was lost (provider.ErrUnavailable: the outcome is unknown), up
// to unknownRetries times, so it takes effect once.
//
// Once an attempt's outcome is unknown, a later attempt's error proves
// nothing about it (a retry refused at the token or scope, never delivered,
// or judged again after the first was accepted): the error returned is then
// the earlier, unknown one. Only a later success settles it.
func (b *Backend) retryUnknown(ctx context.Context, rid string, call func(requestID string) error) error {
	if rid == "" {
		rid = provider.NewRequestID()
	}
	var unknown error
	result := func(err error) error {
		if err != nil && unknown != nil {
			return unknown
		}
		return err
	}
	for attempt := 0; ; attempt++ {
		err := call(rid)
		if err != nil && unknown == nil && unknownOutcome(err) {
			unknown = err
		}
		if !errors.Is(err, provider.ErrUnavailable) || attempt >= unknownRetries || ctx.Err() != nil {
			return result(err)
		}
		select {
		case <-time.After(b.opts.UnknownRetryPause):
		case <-ctx.Done():
			return result(err)
		}
	}
}

// notDelivered reports whether a call provably never reached the service
// (provider.ErrNotDelivered): it changed nothing.
func notDelivered(err error) bool {
	return errors.Is(err, provider.ErrNotDelivered)
}

// keep ends a create or start step that cannot decide what to do: the
// provider did not say what the machine is doing (a read failed, or a reply
// never came). Giving the job up as failed is not an option. A
// failed create is cleaned up by destroying its VM and a failed start by
// stopping it, and the VM may be a finished shed, or one that runs. So
// nothing is changed, in the cloud or in the guest, and the record keeps its
// status ("starting"), its job and what is queued behind it, with reason
// cloud_unreadable for anyone who lists it.
//
// How it ends: the periodic pass runs the job again (Sweep; one read of the
// instance per pass), and so do a start call and the next server start. Once
// the cloud answers, the job carries on from what it finds. A delete works
// as for any shed and ends it at once, VM included. Until then the shed
// holds its name and its slot under the limits, because its machine may be
// running and billed; the machine's lease ends it in any case.
func (b *Backend) keep(j *job, kind string, cause error) error {
	return b.keepWith(j, kind, cause, config.StatusReasonCloudUnreadable)
}

// keepWith is keep with another reason on the record: firewall for a later
// run's firewall refusal, unreachable for a guest that did not answer. The
// job and its status stay, and the periodic pass runs it again.
func (b *Backend) keepWith(j *job, kind string, cause error, reason string) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	b.mu.Lock()
	r := b.currentLocked(j.name, j.id)
	if r == nil {
		b.mu.Unlock()
		return errSuperseded
	}
	first := r.Shed.StatusReason != reason
	if first {
		r.Shed.StatusReason = reason
		b.saveLocked(r)
	}
	b.mu.Unlock()
	if first {
		// Once, not at every pass: the reason on the record says the rest.
		b.logf("remote: %s of shed %s is kept as it is, with nothing changed, and looked at again at every periodic pass: %v", kind, j.name, cause)
	}
	return fmt.Errorf("%w: %w", errUnresolved, cause)
}

// answered takes reason cloud_unreadable off a kept create or start once the
// read it was kept for has been answered and the job goes on. From there it
// is an ordinary job in flight, for as long as an insert, a boot and the
// guest's set-up take, and the reason would tell anyone who lists the shed
// that the server is still waiting for the cloud. A job kept a second time
// gets the reason, and its log line, again.
func (b *Backend) answered(j *job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r := b.currentLocked(j.name, j.id); r != nil && r.Shed.StatusReason == config.StatusReasonCloudUnreadable {
		r.Shed.StatusReason = ""
		b.saveLocked(r)
	}
}

// mutate runs one accepting provider call (start, stop or destroy) after a
// read of the machine, with request id rid (stored with the job before the
// call, so a job resumed after a restart sends the same one; "" for a new
// one). On provider.ErrBusy it waits for the transition in progress to end
// and calls again with the same id (a refusal is judged again); a lost
// reply is sent again with the same id (retryUnknown).
func (b *Backend) mutate(ctx context.Context, ref provider.Ref, rid string, call func(requestID string) (provider.Pending, error)) (provider.Pending, error) {
	return b.mutateSeeing(ctx, ref, rid, nil, call)
}

// mutateSeeing is mutate, handing seen (when set) each machine its reads
// find.
func (b *Backend) mutateSeeing(ctx context.Context, ref provider.Ref, rid string, seen func(provider.Instance), call func(requestID string) (provider.Pending, error)) (provider.Pending, error) {
	if rid == "" {
		rid = provider.NewRequestID()
	}
	for attempt := 0; ; attempt++ {
		inst, err := b.verified(ctx, ref)
		if err != nil {
			return provider.Pending{}, err
		}
		if seen != nil {
			seen(inst)
		}
		var p provider.Pending
		err = b.retryUnknown(ctx, rid, func(rid string) error {
			var cerr error
			p, cerr = call(rid)
			return cerr
		})
		if !errors.Is(err, provider.ErrBusy) || attempt >= maxBusyRetries {
			return p, err
		}
		if _, werr := b.prov.Wait(ctx, provider.Settle(ref)); werr != nil {
			return provider.Pending{}, werr
		}
	}
}

// requestID is the request id a job's call sends: the one the record holds
// for that call on its machine (a retry, or the job resumed after a
// restart), or a new one, written durably to the record before the call is
// sent. An error (errSuperseded when the shed is gone, or the write's,
// state.ErrNotDurable included) means the call must not be sent: a send whose
// id is not on disk could be sent again under another id after a restart.
func (b *Backend) requestID(j *job, call string) (string, error) {
	if rec := b.snapshot(j.name, j.id); rec == nil {
		return "", errSuperseded
	} else if m := rec.Mutation; m != nil && m.Call == call && m.Ref == rec.Ref && m.RequestID != "" {
		return m.RequestID, nil
	}
	rid := provider.NewRequestID()
	var prev *state.Mutation
	err := b.updateDurable(j.name, j.id, func(r *state.Record) {
		prev = r.Mutation
		r.Mutation = &state.Mutation{Call: call, Ref: r.Ref, RequestID: rid}
	})
	if errors.Is(err, state.ErrNotDurable) {
		// In place but not flushed: a power cut could lose it, and the
		// call be sent again under another id. Nothing is sent; the id is
		// taken back, so the next run writes a new one.
		b.update(j.name, j.id, func(r *state.Record) { r.Mutation = prev })
	}
	if err != nil {
		return "", err
	}
	return rid, nil
}

// unknownOutcome reports whether a call that changes something ended with
// its outcome unknown: no reply came to a request that was written
// (provider.ErrUnavailable), the reply broke the contract
// (provider.ErrInvalidReply), or the read before it failed after reaching
// the service. Such a call may have taken effect: nothing is cleaned up for
// it. A request that was never delivered (provider.ErrNotDelivered) is not
// one: it is a refusal that changed nothing, unless a send of the same call
// before it may have been delivered (sentBefore).
func unknownOutcome(err error) bool {
	if errors.Is(err, errNameDeleting) || errors.Is(err, errNameUnknown) || notDelivered(err) {
		// Decided after an EXISTS, which made nothing; or never delivered,
		// which changed nothing.
		return false
	}
	return errors.Is(err, provider.ErrUnavailable) || errors.Is(err, provider.ErrInvalidReply) || isUnread(err)
}

// --- create ---

func (b *Backend) runCreate(ctx context.Context, j *job) error {
	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	if rec.Spec == nil {
		return b.failCreate(j, errors.New("the record holds no create spec"), config.StatusReasonCreateFailed)
	}

	backend.Phase(ctx, "provider")
	backend.Status(ctx, "Creating the cloud machine...")
	// sentBefore: an earlier run stored this create's request id before it
	// sent it, so that send may have been delivered. fresh: this is the
	// create request's own first run and nothing was sent before it, the
	// only case where a create never delivered, or refused at the token,
	// proves nothing was made.
	sentBefore := rec.Mutation != nil && rec.Mutation.Call == state.JobCreate && rec.Mutation.RequestID == rec.Spec.RequestID
	fresh := j.firstRun && !sentBefore && rec.Ref == ""

	// A create resumed after a restart, or by a later pass, first resolves
	// what its earlier send did, before any admission check: the machine it
	// was given (Ref), or, when no answer came, the machine of its shed id
	// (the same thing: provider.Ref is the shed id). It follows the Pending
	// it stored once a read has shown the machine is there.
	var pend provider.Pending
	if ref := rec.Ref; ref != "" || sentBefore {
		if ref == "" {
			ref = provider.Ref(j.id)
		}
		inst, err := b.prov.Get(ctx, ref)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			// Gone, or never made: the create is sent again, as a new
			// request (the stored request id's answer may be a gone
			// machine's), and the provider makes a new machine for the shed
			// id. Nothing of a gone machine carries over: its pin, its
			// facts, what was delivered to its guest and the link to it
			// belong to another guest.
			b.closeLink(j.id)
			// A new request id that is not flushed (state.ErrNotDurable)
			// sends nothing either: the next run, finding the machine
			// gone, makes another and writes it again.
			if err := b.updateDurable(j.name, j.id, resetForNewMachine); err != nil {
				if errors.Is(err, errSuperseded) {
					return errSuperseded
				}
				return b.keep(j, state.JobCreate, fmt.Errorf("recording the create's new request id: %w", err))
			}
			if rec = b.snapshot(j.name, j.id); rec == nil {
				return errSuperseded
			}
		case err != nil:
			// The read failed, so nothing is known: the machine may be a
			// create half done, or a shed the server that died had
			// finished (labelled ready) and that the list did not show.
			return b.keep(j, state.JobCreate, fmt.Errorf("reading machine %s: %w", ref, err))
		case inst.ShedState() == provider.ShedStateReady:
			return b.resumeCommitted(ctx, j, inst)
		default:
			pend = provider.Pending{Ref: inst.Ref, Want: provider.StateRunning}
			if rec.Pending != nil && rec.Pending.Ref == inst.Ref {
				pend = *rec.Pending
			}
			if rec.Ref == "" {
				// The earlier send made it: the record takes it now.
				if !b.update(j.name, j.id, func(r *state.Record) { r.Ref = inst.Ref; b.noteMachineLocked(r, inst) }) {
					return errSuperseded
				}
			}
		}
	}
	// The provider has said what there is (a machine on its way, or none),
	// so a create that was kept is one in flight again.
	b.answered(j)
	if pend.Ref == "" {
		// The request was let in past the firewall check, but that may be
		// long ago: a create resumed after a restart was let in by a server
		// that is gone. Nothing is created while the check fails. Only the
		// create request's own first run is refused for it; a later run
		// keeps the shed for the next pass.
		if err := b.firewallGate(ctx, rec.Shed.NetworkProfile); err != nil {
			if fresh {
				return b.refuseCreate(j, err, config.StatusReasonFirewall)
			}
			return b.keepCreate(j, err, config.StatusReasonFirewall)
		}
		// The first run answers inside the fast-fail window: its waits and
		// its sends end by the window's end (nameDeadline), so a create
		// never delivered, or a name still held, is the request's answer
		// (503, 409) and never a 201 followed by a failure. A send that was
		// written and got no reply by then is an unknown outcome: kept.
		deadline := j.nameDeadline
		if deadline.IsZero() {
			deadline = time.Now().Add(b.nameWait())
		}
		cctx := ctx
		if j.firstRun {
			var cancel context.CancelFunc
			cctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		// A shed of this name deleted a moment ago holds the name at the
		// service until its machine is gone: wait for its delete, until the
		// deadline. A name still held then is 409 COMPUTE_DELETING, never
		// SHED_ALREADY_EXISTS, so that no client adopts a shed that is
		// going away.
		if err := b.waitNameFree(cctx, j.name, j.id, deadline); err != nil {
			if ctx.Err() != nil {
				return errSuperseded
			}
			if errors.Is(err, errNameDeleting) && !sentBefore {
				return b.refuseCreate(j, err, config.StatusReasonCreateFailed)
			}
			return b.keep(j, state.JobCreate, err)
		}
		// The stored create, verbatim, request id included: the provider
		// answers a repeat with the first answer, and a resumed create
		// with the same body as the machine it made. The record notes,
		// durably and before the first send, that it is being sent: a later
		// run that cannot reach the service does not know whether an
		// earlier send made the machine. A note that cannot be written
		// sends nothing.
		if !sentBefore {
			var prev *state.Mutation
			err := b.updateDurable(j.name, j.id, func(r *state.Record) {
				prev = r.Mutation
				r.Mutation = &state.Mutation{Call: state.JobCreate, RequestID: r.Spec.RequestID}
			})
			if errors.Is(err, state.ErrNotDurable) {
				// In place but not flushed: the note is taken back and
				// nothing is sent.
				b.update(j.name, j.id, func(r *state.Record) { r.Mutation = prev })
			}
			switch {
			case errors.Is(err, errSuperseded):
				return errSuperseded
			case err != nil:
				if fresh {
					return b.refuseCreate(j, fmt.Errorf("%w: recording the create before sending it: %w", errNotRecorded, err), config.StatusReasonCreateFailed)
				}
				return b.keep(j, state.JobCreate, fmt.Errorf("recording the create before sending it: %w", err))
			}
		}
		var inst provider.Instance
		var err error
		for {
			err = b.retryUnknown(cctx, rec.Spec.RequestID, func(string) error {
				var cerr error
				pend, inst, cerr = b.prov.Create(cctx, *rec.Spec)
				return cerr
			})
			if !errors.Is(err, provider.ErrExists) {
				break
			}
			// The name is held by a machine of another shed id: a live one
			// is SHED_ALREADY_EXISTS; one being deleted is waited for, and
			// still held at the deadline is COMPUTE_DELETING.
			retry, nerr := b.nameHeld(cctx, j, deadline, err)
			if ctx.Err() != nil {
				return errSuperseded
			}
			if !retry {
				err = nerr
				break
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return errSuperseded
			}
			switch {
			case judgedCreate(err):
				// The service judged this create and refused it.
				return b.refuseCreate(j, err, config.StatusReasonCreateFailed)
			case fresh && notSent(err):
				// Never delivered, or refused at the token, on every send of
				// the first run: nothing was made (503 COMPUTE_UNAVAILABLE,
				// 403 COMPUTE_DENIED).
				return b.refuseCreate(j, err, config.StatusReasonCreateFailed)
			}
			// It may have been accepted: kept, never cleaned up, and sent
			// again (the same request id) by the next pass.
			return b.keep(j, state.JobCreate, fmt.Errorf("creating the machine: %w", err))
		}
		if !b.update(j.name, j.id, func(r *state.Record) {
			r.Ref = pend.Ref
			r.Pending = &pend
			// The create's answer at once: the machine's facts are on the
			// first starting record.
			b.noteMachineLocked(r, inst)
		}) {
			// Deleted while the create was being accepted: hand the
			// delete job the machine it could not have known.
			b.setMarkerRef(j.id, pend.Ref)
			return errSuperseded
		}
	}
	inst, err := b.prov.Wait(ctx, pend)
	if err != nil {
		if afterAccept(err) {
			// Where the machine is is not known: the create is kept and
			// waits on its stored Pending again at the next pass.
			return b.keep(j, state.JobCreate, fmt.Errorf("waiting for machine %s: %w", pend.Ref, err))
		}
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	b.noteInstance(j, inst)
	j.markAccepted()

	info, err := b.waitAgent(ctx, j)
	if err != nil {
		if ctx.Err() != nil {
			return errSuperseded
		}
		if reason := linkReason(err, ""); reason != "" {
			// A host key other than the attested one, or access this server
			// may not use: no retry changes it.
			return b.failCreate(j, err, reason)
		}
		// The guest did not answer in time: a later pass tries again.
		return b.keepCreate(j, err, config.StatusReasonUnreachable)
	}
	if !guest.ProtocolCompatible(info.Protocol) || !guest.LayoutSupported(info.Layout) {
		err := fmt.Errorf("the guest agent reports protocol %d and layout %d; this server needs protocol %d to %d and layout %d to %d",
			info.Protocol, info.Layout, guest.MinAgentProtocol, guest.AgentProtocol, guest.MinLayout, guest.Layout)
		return b.failCreate(j, err, config.StatusReasonLayout)
	}

	rec = b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return b.keepCreate(j, err, config.StatusReasonUnreachable)
	}
	owned := rec.Shed.Owner != ""
	b.joinBus(j.name, j.id, func(id vmutil.BusIdentity) {
		b.credMgr.SetupCredentials(ctx, agent, j.name, nil, nil, id)
	})

	// The tracked steps: stage 1, the clone, stage 2, the install hook,
	// each saved in the record as it is decided.
	if err := b.createSteps(ctx, j, rec, agent, owned); err != nil {
		return err
	}

	backend.Phase(ctx, "ready")
	if err := b.verify(ctx, rec.Ref); err != nil {
		if afterAccept(err) || errors.Is(err, provider.ErrNotFound) {
			// Everything but the label is done and the machine cannot be
			// read just now, or is gone. The create is kept, not failed;
			// run again, it skips the steps recorded as done, or makes the
			// machine again.
			return b.keep(j, state.JobCreate, fmt.Errorf("reading machine %s before labelling it ready: %w", rec.Ref, err))
		}
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	labels := map[string]*string{
		provider.LabelState:  provider.Str(string(provider.ShedStateReady)),
		provider.LabelLayout: provider.Str(provider.LayoutLabel(info.Layout)),
	}
	var labelled provider.Instance
	rid, err := b.requestID(j, "labels:ready")
	if err != nil {
		if errors.Is(err, errSuperseded) {
			return errSuperseded
		}
		return b.keep(j, state.JobCreate, fmt.Errorf("recording the ready label's request id: %w", err))
	}
	if err := b.retryUnknown(ctx, rid, func(rid string) error {
		var lerr error
		labelled, lerr = b.prov.SetLabels(ctx, rec.Ref, rid, labels, nil)
		return lerr
	}); err != nil {
		if afterAccept(err) {
			return b.keep(j, state.JobCreate, fmt.Errorf("labelling machine %s ready: %w", rec.Ref, err))
		}
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusRunning
		r.Shed.StatusReason = ""
		r.Job = ""
		r.Pending, r.Mutation = nil, nil
		r.Ready = true
		r.Layout = info.Layout
		b.noteMachineLocked(r, labelled)
	}) {
		return errSuperseded
	}
	backend.Status(ctx, "Shed is running")
	return nil
}

// commitCreatedLocked repairs the record of a create that the provider
// shows as finished. A create's last two writes are the shed_state=ready
// label and the local Ready flag, in that order; a server that died between
// them left a finished shed with a record that still says "create in
// flight". The label is the commit point: such a shed is never created again
// and never cleaned up as a failed create, whatever its machine is doing
// now. The record takes the status an adopted shed would get.
//
// The delivery record is left as the create saved it: a step it did not
// record done or skipped stays owed, and nothing is marked done that the
// server did not see.
func (b *Backend) commitCreatedLocked(r *state.Record, inst provider.Instance) {
	r.Ready = true
	r.Pending, r.Mutation = nil, nil
	if r.Ref == "" {
		r.Ref = inst.Ref
	}
	if l := inst.Layout(); l != 0 {
		r.Layout = l
	}
	b.noteMachineLocked(r, inst)
	switch inst.State {
	case provider.StateStopped:
		b.recordStoppedLocked(r, inst)
		return
	case provider.StateStopping:
		r.Shed.Status = config.StatusStopping
		r.Shed.StatusReason = ""
		r.Job = state.JobStop
	default:
		// Running, or on its way: "starting" until a probe succeeds.
		r.Shed.Status = config.StatusStarting
		r.Shed.StatusReason = ""
		r.Job = state.JobProbe
	}
	b.saveLocked(r)
}

// resumeCommitted carries a resumed create job on as the job a finished shed
// needs: a probe when its instance runs, a stop when it is stopping.
func (b *Backend) resumeCommitted(ctx context.Context, j *job, inst provider.Instance) error {
	b.mu.Lock()
	r := b.currentLocked(j.name, j.id)
	if r == nil {
		b.mu.Unlock()
		return errSuperseded
	}
	b.reconcileUserLocked(r, inst, b.logNote)
	b.commitCreatedLocked(r, inst) // saves r
	status := r.Shed.Status
	b.mu.Unlock()
	b.logf("remote: create of shed %s had finished in the cloud (%s=%s); recording it %s", j.name, provider.LabelState, provider.ShedStateReady, status)
	j.markAccepted()
	switch status {
	case config.StatusStarting:
		return b.runProbe(ctx, j)
	case config.StatusStopping:
		return b.runStop(ctx, j)
	}
	return nil
}

// The create's clean-up rule (lead ruling), decided here and in
// refuseCreate and keepCreate:
//
// A create is cleaned up (its record dropped, or a marker written that
// destroys its machine) ONLY when the service definitively refused THAT
// create, or when a person deleted the shed (DeleteShed). A definitive
// refusal is a judgement refusal from mapping.json at the create call
// itself (EXISTS, a policy or limit refusal, quota, capacity), or a create
// never delivered on every attempt of the first run with no request id
// stored by an earlier run (ErrNotDelivered, or ErrDenied at the first
// send). Once any attempt may have been delivered or was accepted, every
// later failure keeps the record: a firewall or admission check on a later
// run, an authorization refusal on a retry or a read, a read or poll that
// never reached the service, a boot or probe timeout. Those keep it
// starting with a reason (keepCreate: cloud_unreadable, firewall,
// unreachable) for the next pass to run the create again; a failure a
// retry cannot change (a host key other than the attested one, access this
// server may not use, a guest it cannot drive, an operation the service
// failed) leaves it in error with the reason (failCreate). Neither writes a
// marker: the machine stays until a person deletes the shed, and its lease
// bounds the cost.

// refuseCreate ends a create the service definitively refused (see the rule
// above): inside the fast-fail window the refusal is the create's answer
// and no record stays; after it the shed is left in error with the reason.
// The refused create made no machine, so nothing is destroyed.
func (b *Backend) refuseCreate(j *job, cause error, reason string) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	if r := provider.Reason(cause); r != "" {
		reason = r
	}
	b.mu.Lock()
	rec := b.currentLocked(j.name, j.id)
	if rec == nil {
		b.mu.Unlock()
		return errSuperseded
	}
	if j.fastWindow {
		j.fastErr = cause
		b.dropLocked(j.name)
	} else {
		b.endCreateLocked(rec, reason)
	}
	b.mu.Unlock()
	b.logf("remote: create of shed %s was refused (%s): %v", j.name, reason, cause)
	b.credMgr.StopListener(j.name)
	b.closeLink(j.id)
	return cause
}

// keepCreate keeps a create whose outcome is not known, or whose failure a
// later pass may get past (see the rule above), starting with reason: the
// next pass runs it again, from what the service then says.
func (b *Backend) keepCreate(j *job, cause error, reason string) error {
	b.closeLink(j.id)
	return b.keepWith(j, state.JobCreate, cause, reason)
}

// failCreate leaves a create that may have made a machine, and that no
// retry will finish, in status error with the reason (see the rule above).
// Nothing is cleaned up: the machine stays until a person deletes the shed.
func (b *Backend) failCreate(j *job, cause error, reason string) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	if r := provider.Reason(cause); r != "" {
		reason = r
	}
	b.mu.Lock()
	rec := b.currentLocked(j.name, j.id)
	if rec == nil {
		b.mu.Unlock()
		return errSuperseded
	}
	b.endCreateLocked(rec, reason)
	b.mu.Unlock()
	b.logf("remote: create of shed %s failed (%s); its machine, if any, stays until the shed is deleted: %v", j.name, reason, cause)
	b.credMgr.StopListener(j.name)
	b.closeLink(j.id)
	return cause
}

// endCreateLocked records a create that ended without a running shed.
func (b *Backend) endCreateLocked(rec *state.Record, reason string) {
	rec.Shed.Status = config.StatusError
	rec.Shed.StatusReason = reason
	rec.Job = ""
	rec.Queued = ""
	rec.Pending, rec.Mutation = nil, nil
	b.saveLocked(rec)
}

// judgedCreate reports whether a create call's error is the service's
// judgement of that create (mapping.json): checked after the request id's
// kept answer and the shed id's machine, so the create made nothing.
func judgedCreate(err error) bool {
	for _, k := range []error{provider.ErrQuota, provider.ErrCapacity, provider.ErrComputeLimit, provider.ErrPolicy,
		provider.ErrInvalid, provider.ErrLimit, provider.ErrExists, errNameDeleting, errNameUnknown} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}

// notSent reports whether a create call's error says the request was never
// acted on, which proves nothing was made only for a first send: never
// delivered, or refused at the token or scope before any judgement.
func notSent(err error) bool {
	return notDelivered(err) || errors.Is(err, provider.ErrDenied)
}

// afterAccept reports whether an error on a create that may have been
// delivered, or was accepted, keeps the create for a later pass (see the
// rule above): an unknown outcome, a read or poll that never reached the
// service, or an authorization refusal of the read.
func afterAccept(err error) bool {
	return unknownOutcome(err) || notSent(err)
}

// errNotRecorded is a create the server could not record as being sent:
// nothing was sent (500 BACKEND_ERROR).
var errNotRecorded = errors.New("the create could not be recorded")

// errNameDeleting marks a create refused because its name is still held
// by a machine being deleted (nameDeleting): a definite refusal that made
// nothing.
var errNameDeleting = errors.New("the name is held by a machine being deleted")

// nameDeleting is the create's answer when its name is still held by a
// machine being deleted at the end of the wait: 409 COMPUTE_DELETING with
// no reason.
func nameDeleting(name string, cause error) error {
	msg := fmt.Sprintf("the name %s is held by a shed whose machine is still being deleted; try again shortly", name)
	if cause != nil {
		msg += fmt.Sprintf(" (%v)", cause)
	}
	return fmt.Errorf("%w: %w", errNameDeleting, &config.ComputeRefusal{Status: http.StatusConflict, Code: config.ErrComputeDeleting, Err: errors.New(msg)})
}

// nameWait is how long a create waits for its name to be free: the
// fast-fail window, less a margin so that the create's answer is the
// refusal and not a 201.
func (b *Backend) nameWait() time.Duration {
	return b.opts.FastFailWindow * 9 / 10
}

// nameHeld decides a create the service answered EXISTS (exists): the name
// is held by a machine of another shed id. While it runs the create handler
// waits for it (job.naming), so the create has one answer: a live holder is
// SHED_ALREADY_EXISTS; one being deleted is waited for until deadline and
// still there is 409 COMPUTE_DELETING; a holder that is gone has the create
// sent again (retry). A holder that cannot be learned by the deadline (the
// list fails) is 503 COMPUTE_UNAVAILABLE: EXISTS made nothing, so the
// refusal leaves nothing. Every list is bounded by the deadline, and the
// decision is never later than it.
func (b *Backend) nameHeld(ctx context.Context, j *job, deadline time.Time, exists error) (retry bool, err error) {
	// The mark is lifted only when the create goes on (retry): a refusal
	// is handed to the handler by failCreate, and the job ends, which the
	// handler waits for as well.
	b.setNaming(j, true)
	defer func() {
		if retry {
			b.setNaming(j, false)
		}
	}()
	// Decided by the deadline, the window's end on a create request's
	// first run: never later, so the refusal is the request's answer.
	end := deadline
	deleting := false
	for {
		lctx, cancel := context.WithDeadline(ctx, end)
		holder, known, lerr := b.nameHolder(lctx, j.name, j.id)
		cancel()
		switch {
		case j.ctx.Err() != nil:
			// The job ended (a delete, a shutdown). ctx itself ends at the
			// deadline, which is a decision below, not an end.
			return false, j.ctx.Err()
		case known && holder == nil:
			return true, nil
		case known && holder.State != provider.StateDeleting && holder.ShedState() != provider.ShedStateDeleting:
			return false, exists
		case known:
			deleting = true
		}
		if !time.Now().Before(end) {
			if deleting {
				return false, nameDeleting(j.name, exists)
			}
			return false, fmt.Errorf("%w: %w", errNameUnknown, computeUnavailable(fmt.Errorf("the name %s is held at the account service and what holds it could not be read: %w", j.name, lerr)))
		}
		select {
		case <-j.ctx.Done():
			return false, j.ctx.Err()
		case <-time.After(min(b.findInterval(), time.Until(end))):
		}
	}
}

// errNameUnknown marks a create refused because what holds its name could
// not be learned: a definite refusal (EXISTS made nothing).
var errNameUnknown = errors.New("what holds the name is not known")

// nameHolder lists the machines and returns the one of another shed id that
// holds name, nil when none does; known is false when the list failed.
func (b *Backend) nameHolder(ctx context.Context, name, id string) (holder *provider.Instance, known bool, err error) {
	l, err := b.prov.List(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range l.Instances {
		if inst := l.Instances[i]; inst.Name == name && inst.Ref != provider.Ref(id) {
			return &inst, true, nil
		}
	}
	return nil, true, nil
}

// setNaming marks the create job as deciding what holds its name (nameHeld);
// the create handler waits for the decision past the fast-fail window.
func (b *Backend) setNaming(j *job, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on {
		j.naming = make(chan struct{})
		return
	}
	if j.naming != nil {
		close(j.naming)
		j.naming = nil
	}
}

// waitNameFree waits until no delete of another shed of this name is in
// flight: the name is held at the service by the deleted shed's machine
// until it is gone. At the deadline a name still held is nameDeleting.
func (b *Backend) waitNameFree(ctx context.Context, name, id string, deadline time.Time) error {
	// While it waits the create is deciding its name: the create handler
	// waits for the decision (job.naming), so a refusal made right at the
	// deadline is still the request's answer. The mark is lifted when the
	// name is free; a refusal leaves it to the job's end.
	marked := false
	for {
		b.mu.Lock()
		var d *job
		for _, dj := range b.deletes {
			if dj.name == name && dj.id != id {
				d = dj
				break
			}
		}
		self := b.jobs[name]
		b.mu.Unlock()
		if d == nil {
			if marked {
				b.setNaming(self, false)
			}
			return nil
		}
		if !marked && self != nil && self.id == id {
			b.setNaming(self, true)
			marked = true
		}
		wait := time.NewTimer(time.Until(deadline))
		select {
		case <-d.done:
			wait.Stop()
		case <-wait.C:
			return nameDeleting(name, nil)
		case <-ctx.Done():
			wait.Stop()
			if time.Now().Before(deadline) {
				return ctx.Err()
			}
			return nameDeleting(name, nil)
		}
	}
}

// noteInstance copies what the provider reported about a started machine
// into the record, and brings the record's user into line with the
// machine's owner (reconcileUserLocked), as every read of a machine does.
func (b *Backend) noteInstance(j *job, inst provider.Instance) {
	b.update(j.name, j.id, func(r *state.Record) {
		r.Pending = nil
		if inst.Ref != "" {
			r.Ref = inst.Ref
		}
		b.noteMachineLocked(r, inst)
		b.reconcileUserLocked(r, inst, b.logNote)
	})
}

// noteMachineLocked keeps the machine the provider answered in the record:
// its facts for the shed's cloud view, and its access for a reconnect. A
// zero machine (an answer that carried none) changes nothing.
func (b *Backend) noteMachineLocked(r *state.Record, inst provider.Instance) {
	if inst.Ref == "" {
		return
	}
	m := inst.Clone()
	r.Machine = &m
	if inst.ImageResolved != "" {
		r.Shed.Image = inst.ImageResolved
	}
}

// resetForNewMachine readies the record of an unfinished create whose
// machine is gone for a new machine of the same shed id: a new request id
// for the create, and nothing of the gone machine (its facts, its pinned
// host key, its layout, the request ids and operation of its calls, an owner
// conflict read from it, and what was delivered to its guest, which another
// guest does not have).
func resetForNewMachine(r *state.Record) {
	r.Spec.RequestID = provider.NewRequestID()
	r.Machine = nil
	r.HostKeyPin = ""
	r.Layout = 0
	r.Pending, r.Mutation, r.Extend = nil, nil, nil
	r.UserConflict = ""
	r.Ready = false
	r.Cloned, r.Provisioned = false, false
	r.Delivery = delivery.NewRecord()
}

// cloneRepo clones the shed's repository through the agent. Each attempt runs
// under its own deadline, and a failed attempt is retried a bounded number of
// times: a guest's first clone can fail or hang while its network settles.
// Git never prompts (GIT_TERMINAL_PROMPT=0), so a repository that needs
// credentials the URL lacks fails at once instead of waiting on a terminal.
// repo may carry credentials; it is never logged.
//
// Under provision nothing is ever removed: a failed attempt's leftovers are
// not cleared, and when something stands at the project directory after a
// failed attempt the clone stops there instead of trying again.
func (b *Backend) cloneRepo(ctx context.Context, agent *vmutil.AgentClient, rec *state.Record, repo string, owned, provision bool) error {
	shown := config.SanitizeRepoURL(rec.Shed.Repo)
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, b.opts.CloneTimeout)
		err := b.clone(actx, agent, b.opts.Server, repo, owned, []string{"GIT_TERMINAL_PROMPT=0"})
		timedOut := actx.Err() != nil && ctx.Err() == nil
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if timedOut {
			err = fmt.Errorf("no result within %s: %w", b.opts.CloneTimeout, err)
		}
		if attempt >= cloneAttempts {
			return fmt.Errorf("after %d attempts: %w", attempt, err)
		}
		// The error may quote the guest: only its class is logged.
		b.logf("remote: clone of %s into shed %s failed (attempt %d of %d, %s); retrying in %s",
			shown, rec.Shed.Name, attempt, cloneAttempts, vmutil.CloneErrorClass(err), b.opts.CloneRetryPause)
		if provision {
			if wt, perr := b.workTree(ctx, agent, rec.Shed.Repo); perr != nil || wt != vmutil.WorkTreeAbsent {
				return fmt.Errorf("after %d attempts, with the project directory not empty (left as it is): %w", attempt, err)
			}
		} else {
			b.rmPartial(ctx, agent, repo)
		}
		backend.Status(ctx, fmt.Sprintf("Clone failed; retrying (attempt %d of %d)...", attempt+1, cloneAttempts))
		select {
		case <-time.After(b.opts.CloneRetryPause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// removePartialClone removes what a failed clone left behind, so the next
// attempt does not find a directory in its way. Best-effort. A create's own
// clone only: never under provision.
func (b *Backend) removePartialClone(ctx context.Context, agent *vmutil.AgentClient, repo string) {
	dir, err := config.RepoDirName(repo)
	if err != nil || strings.HasPrefix(dir, "-") {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_ = agent.Exec(rctx, backend.ExecOptions{
		Cmd:        []string{"rm", "-rf", "--", dir},
		Stdout:     vmutil.NopWriteCloser(io.Discard),
		Stderr:     vmutil.NopWriteCloser(io.Discard),
		WorkingDir: config.HomePath,
	})
}

// provisionerFor runs rec's hooks through agent in its landing directory.
func provisionerFor(agent *vmutil.AgentClient, rec *state.Record) *vmutil.Provisioner {
	workDir := rec.Shed.LandingDir
	if workDir == "" {
		workDir = config.HomePath
	}
	provisioner := vmutil.NewProvisioner(agent, rec.Shed.Name)
	provisioner.SetWorkDir(workDir)
	// The project directory is on the VM's own disk: no mount to wait for,
	// so a repository with no .shed/provision.yaml costs no retries.
	provisioner.SetNoMountLag()
	provisioner.SetOutput(os.Stdout, os.Stderr)
	return provisioner
}

// provision runs the shed's startup hook through the agent (runInstall is
// false at every start), the same way the local backends do. Best-effort:
// failures are logged.
func (b *Backend) provision(ctx context.Context, agent *vmutil.AgentClient, rec *state.Record, runInstall bool) {
	provisioner := provisionerFor(agent, rec)
	cfg, err := provisioner.LoadConfig(ctx)
	if err != nil {
		b.logf("Warning: failed to load provisioning config for %s: %v", rec.Shed.Name, err)
		return
	}
	if err := provisioner.RunProvisioning(ctx, cfg, runInstall); err != nil {
		b.logf("Warning: provisioning of %s failed: %v", rec.Shed.Name, err)
	}
}

// --- start ---

func (b *Backend) runStart(ctx context.Context, j *job) error {
	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	backend.Phase(ctx, "provider")
	backend.Status(ctx, "Starting the cloud machine...")
	// A start resumed after a restart whose call the service had accepted
	// waits on the operation it stored, and sends nothing again: a new
	// start would be judged again, and could be refused for a start that
	// already ran.
	var pend provider.Pending
	if p := rec.Pending; p != nil && p.Ref == rec.Ref && p.Want == provider.StateRunning {
		pend = *p
	} else {
		// The request was let in past the firewall check, but the job may
		// run much later: resumed after a restart, or queued behind a stop.
		// No start is sent to a stopped VM while the check fails. A VM that
		// is already on its way up or running (a resumed start the cloud
		// had accepted) is followed, not judged again: the check never
		// stops a VM, and the record must not call a running one stopped.
		if gerr := b.firewallGate(ctx, rec.Shed.NetworkProfile); gerr != nil {
			if ctx.Err() != nil {
				return errSuperseded
			}
			inst, err := b.prov.Get(ctx, rec.Ref)
			if unread(err) {
				// Whether the VM is up is not known. A failed start stops
				// the VM (failStart), and this one may be running, so the
				// start is neither failed nor refused: it is kept and looked
				// at again.
				return b.keep(j, state.JobStart, fmt.Errorf("the firewall check fails (%w) and instance %s could not be read: %w", gerr, rec.Ref, err))
			}
			if err != nil {
				return b.failStart(j, err, "")
			}
			// A VM that is not up gets no start while the check fails, so
			// the shed goes back to "stopped", which is true, and holds no
			// run slot.
			if inst.State != provider.StateStarting && inst.State != provider.StateRunning {
				return b.refuseStart(j, gerr)
			}
		}
		// The request id is stored before the call: a retry, or the job
		// resumed after a restart, sends the same one. One already stored
		// means an earlier run may have sent the start.
		sentBefore := rec.Mutation != nil && rec.Mutation.Call == state.JobStart && rec.Mutation.Ref == rec.Ref
		rid, err := b.requestID(j, state.JobStart)
		if err != nil {
			if errors.Is(err, errSuperseded) {
				return errSuperseded
			}
			return b.keep(j, state.JobStart, fmt.Errorf("recording the start's request id: %w", err))
		}
		pend, err = b.mutate(ctx, rec.Ref, rid, func(rid string) (provider.Pending, error) {
			return b.prov.Start(ctx, rec.Ref, rid)
		})
		if unknownOutcome(err) || (sentBefore && notDelivered(err)) {
			// The read that precedes the start call failed, or the start's
			// reply never came: whether a start was accepted is not known.
			// The machine may be up all the same, so it is not stopped on
			// the strength of that.
			return b.keep(j, state.JobStart, fmt.Errorf("starting machine %s: %w", rec.Ref, err))
		}
		if err != nil {
			return b.failStart(j, err, "")
		}
		b.update(j.name, j.id, func(r *state.Record) {
			r.Pending = &pend
			if r.Shed.StatusReason == config.StatusReasonCloudUnreadable {
				// The instance was read and the start accepted: a start that
				// was kept is one in flight again (see answered).
				r.Shed.StatusReason = ""
			}
		})
	}
	inst, err := b.prov.Wait(ctx, pend)
	if unknownOutcome(err) {
		return b.keep(j, state.JobStart, fmt.Errorf("waiting for machine %s to start: %w", rec.Ref, err))
	}
	if err != nil {
		return b.failStart(j, err, "")
	}
	b.noteInstance(j, inst)

	info, err := b.waitAgent(ctx, j)
	if err != nil {
		return b.failStart(j, err, linkReason(err, config.StatusReasonUnreachable))
	}
	if !guest.ProtocolCompatible(info.Protocol) || !guest.LayoutSupported(info.Layout) {
		err := fmt.Errorf("the guest agent reports protocol %d and layout %d, which this server cannot drive", info.Protocol, info.Layout)
		return b.failStart(j, err, config.StatusReasonLayout)
	}

	rec = b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return b.failStart(j, err, config.StatusReasonUnreachable)
	}
	b.joinBus(j.name, j.id, func(id vmutil.BusIdentity) {
		b.credMgr.SetupCredentials(ctx, agent, j.name, nil, nil, id)
	})
	if !rec.NoProvision {
		backend.Phase(ctx, "provision")
		b.provision(ctx, agent, rec, false)
		if ctx.Err() != nil {
			return errSuperseded
		}
	}
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusRunning
		r.Shed.StatusReason = ""
		r.Job = ""
		r.Pending, r.Mutation = nil, nil
		r.Layout = info.Layout
	}) {
		return errSuperseded
	}
	backend.Status(ctx, "Shed is running")
	return nil
}

// failStart records a failed start: the shed goes back to "stopped" with the
// reason. A refusal (capacity, quota) left the VM stopped. When the VM came
// up and its agent could not be used, the VM is stopped again so it does not
// run unreachable. An instance that no longer exists takes its record with
// it.
func (b *Backend) failStart(j *job, cause error, reason string) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	if r := provider.Reason(cause); r != "" {
		reason = r
	}
	b.credMgr.StopListener(j.name)
	b.closeLink(j.id)

	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	if errors.Is(cause, provider.ErrNotFound) {
		// Dropped only on the sweep's evidence: the list omits the machine
		// and an exact read answers 404. Otherwise the shed is recorded
		// stopped and a later pass decides.
		if gone, why := b.confirmGone(j.ctx, rec.Ref); gone {
			b.forgetGone(j.name, j.id, why, "start")
			return cause
		}
		b.update(j.name, j.id, func(r *state.Record) {
			r.Shed.Status = config.StatusStopped
			r.Shed.StatusReason = ""
			r.Job, r.Queued = "", ""
			r.Pending, r.Mutation = nil, nil
		})
		return cause
	}
	b.logf("remote: start of shed %s failed (%s): %v", j.name, reason, cause)
	// A refusal leaves the machine stopped; a lease or person refusal, or a
	// delete in progress, leaves nothing running to be stopped. Anything
	// else may have left it running.
	leftStopped := provider.IsRefusal(cause) || errors.Is(cause, provider.ErrLease) || errors.Is(cause, provider.ErrDeleting) || notDelivered(cause)
	if !leftStopped {
		b.stopQuietly(rec.Ref, "failed start of "+j.name)
	}
	b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusStopped
		r.Shed.StatusReason = reason
		r.Job = ""
		r.Queued = ""
		r.Pending, r.Mutation = nil, nil
	})
	return cause
}

// refuseStart records a start the server did not send because the firewall
// check is failing: the shed goes back to "stopped" with reason "firewall".
// Nothing was asked of the cloud, so nothing is stopped.
func (b *Backend) refuseStart(j *job, cause error) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	b.logf("remote: start of shed %s was not sent: %v", j.name, cause)
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusStopped
		r.Shed.StatusReason = config.StatusReasonFirewall
		r.Job = ""
		r.Queued = ""
		r.Pending, r.Mutation = nil, nil
	}) {
		return errSuperseded
	}
	return cause
}

// stopQuietly stops a machine on a bounded context of its own.
func (b *Backend) stopQuietly(ref provider.Ref, why string) {
	ctx, cancel := context.WithTimeout(b.rootCtx, cleanupTimeout)
	defer cancel()
	pend, err := b.mutate(ctx, ref, "", func(rid string) (provider.Pending, error) { return b.prov.Stop(ctx, ref, rid) })
	if err == nil {
		_, err = b.prov.Wait(ctx, pend)
	}
	if err != nil && !errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrDeleting) {
		b.logf("remote: stopping the machine of a %s: %v", why, err)
	}
}

// confirmGone reports whether a shed's machine is gone on the evidence the
// sweep drops a record on: a list that omits it and an exact read that
// answers 404. Any read that fails is "not confirmed".
func (b *Backend) confirmGone(ctx context.Context, ref provider.Ref) (gone bool, cause string) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	l, err := b.prov.List(ctx)
	if err != nil {
		return false, ""
	}
	for _, inst := range l.Instances {
		if inst.Ref == ref {
			return false, ""
		}
	}
	_, err = b.prov.Get(ctx, ref)
	if !errors.Is(err, provider.ErrNotFound) {
		return false, ""
	}
	return true, deletionCauses(l.Deleted)[string(ref)]
}

// goneFor is confirmGone and, when the machine is gone, forgetGone, for a
// job: it reports whether the shed was dropped.
func (b *Backend) goneFor(j *job, ref provider.Ref, what string) bool {
	gone, why := b.confirmGone(j.ctx, ref)
	if gone {
		b.forgetGone(j.name, j.id, why, what)
	}
	return gone
}

// forgetGone drops a shed whose machine is confirmed gone (confirmGone)
// during a job (what: "start", "stop", "probe"), as the periodic pass does:
// when the list named the machine's deletion, with a line naming the cause
// and the service-deletion audit event.
func (b *Backend) forgetGone(name, id, cause, what string) {
	b.mu.Lock()
	r := b.currentLocked(name, id)
	var event *audit.ShedEvent
	if r != nil {
		if cause != "" {
			event = serviceDeletionEvent(r, cause)
		}
		b.dropLocked(name)
	}
	auditLog := b.auditLog
	b.mu.Unlock()
	if r == nil {
		return
	}
	if cause != "" {
		b.logf("remote: shed %s (%s): the account service deleted its machine (cause %s); dropping its record (seen by its %s)", name, id, cause, what)
	} else {
		b.logf("remote: shed %s (%s) has no machine any more; dropping its record (seen by its %s)", name, id, what)
	}
	b.recordServiceDeletion(auditLog, event, name)
	b.credMgr.StopListener(name)
	b.closeLink(id)
}

// --- stop ---

func (b *Backend) runStop(ctx context.Context, j *job) error {
	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	backend.Phase(ctx, "provider")
	backend.Status(ctx, "Stopping the cloud machine...")
	b.credMgr.StopListener(j.name)

	// Each read of the machine brings the shed's user into line with its
	// owner, as every read does (reconcileUserLocked).
	seen := func(inst provider.Instance) { b.reconcileUserRead(j.name, j.id, inst) }
	// A stop resumed after a restart whose call was accepted waits on the
	// operation it stored. Otherwise the request id is stored before the
	// call: a retry, or the job resumed, sends the same one.
	var pend provider.Pending
	var err error
	if p := rec.Pending; p != nil && p.Ref == rec.Ref && p.Want == provider.StateStopped {
		pend = *p
	} else {
		rid, rerr := b.requestID(j, state.JobStop)
		if rerr != nil {
			if errors.Is(rerr, errSuperseded) {
				return errSuperseded
			}
			return b.keep(j, state.JobStop, fmt.Errorf("recording the stop's request id: %w", rerr))
		}
		pend, err = b.mutateSeeing(ctx, rec.Ref, rid, seen, func(rid string) (provider.Pending, error) { return b.prov.Stop(ctx, rec.Ref, rid) })
		if err == nil {
			b.update(j.name, j.id, func(r *state.Record) { r.Pending = &pend })
		}
	}
	var inst provider.Instance
	if err == nil {
		inst, err = b.prov.Wait(ctx, pend)
	}
	if err != nil {
		return b.failStop(j, rec.Ref, err)
	}
	b.closeLink(j.id)
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusStopped
		r.Shed.StatusReason = ""
		r.Job = ""
		r.Pending, r.Mutation = nil, nil
		b.noteMachineLocked(r, inst)
		b.reconcileUserFromLocked(r, inst)
	}) {
		return errSuperseded
	}
	backend.Status(ctx, "Shed is stopped")
	return nil
}

// failStop records a failed stop from what the cloud says the instance is
// doing now. When the cloud cannot be asked, the shed stays "stopping" and
// the next stop call, or the next server start, tries again.
func (b *Backend) failStop(j *job, ref provider.Ref, cause error) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	if errors.Is(cause, provider.ErrNotFound) {
		if gone, why := b.confirmGone(j.ctx, ref); gone {
			b.forgetGone(j.name, j.id, why, "stop")
			return cause
		}
	}
	if unknownOutcome(cause) || notDelivered(cause) {
		// Whether the stop was sent, or where it stands, is not known: the
		// stop is kept as it is (its Pending, its request id, a start
		// queued behind it) and the next pass resumes it.
		return b.keep(j, state.JobStop, fmt.Errorf("stopping machine %s: %w", ref, cause))
	}
	b.logf("remote: stop of shed %s failed: %v", j.name, cause)
	ctx, cancel := context.WithTimeout(b.rootCtx, probeTimeout)
	defer cancel()
	inst, err := b.prov.Get(ctx, ref)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		// Not confirmed gone (the list still names it): nothing runs
		// there, and a later pass decides.
		b.update(j.name, j.id, func(r *state.Record) {
			r.Job = ""
			r.Pending, r.Mutation = nil, nil
			r.Shed.Status = config.StatusStopped
		})
		return cause
	case err != nil:
		return b.keep(j, state.JobStop, fmt.Errorf("the stop failed (%w) and machine %s could not be read: %w", cause, ref, err))
	case inst.State != provider.StateStopped && inst.State != provider.StateRunning:
		// Still on its way somewhere: the stop is kept for the next pass.
		b.update(j.name, j.id, func(r *state.Record) { b.noteMachineLocked(r, inst) })
		return b.keep(j, state.JobStop, fmt.Errorf("the stop failed (%w) and machine %s is %s", cause, ref, inst.State))
	}
	b.update(j.name, j.id, func(r *state.Record) {
		r.Pending, r.Mutation = nil, nil
		b.noteMachineLocked(r, inst)
		b.reconcileUserFromLocked(r, inst)
		r.Job = ""
		if inst.State == provider.StateStopped {
			r.Shed.Status = config.StatusStopped
			b.closeLinkLater(j.id)
		} else {
			r.Shed.Status = config.StatusRunning
		}
	})
	return cause
}

// closeLinkLater closes a shed's link without Backend.mu held by the caller's
// critical section.
func (b *Backend) closeLinkLater(id string) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.closeLink(id)
	}()
}

// --- delete ---

// setMarkerRef records the machine of a shed whose delete was acknowledged
// before its create had been accepted.
func (b *Backend) setMarkerRef(id string, ref provider.Ref) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := b.deletes[id]; d != nil && d.marker != nil && d.marker.Ref == "" {
		d.marker.Ref = ref
		if err := b.store.SaveMarker(d.marker); err != nil {
			b.logf("remote: updating the delete marker of %s: %v", d.marker.Name, err)
		}
		return
	}
	// The delete job has not been registered yet; write the marker file so
	// it finds the ref when it loads.
	markers, err := b.store.Markers()
	if err != nil {
		return
	}
	for _, m := range markers {
		if m.ID == id && m.Ref == "" {
			m.Ref = ref
			_ = b.store.SaveMarker(m)
		}
	}
}

// startDelete runs the delete job for a marker. prior, when set, is the
// shed's cancelled lifecycle job; the delete waits for it to end, so an
// insert that was in flight has either been accepted or refused.
func (b *Backend) startDelete(marker *state.Marker, prior *job) {
	b.mu.Lock()
	if b.closed || b.deletes[marker.ID] != nil {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(b.rootCtx)
	j := &job{
		kind:     jobDelete,
		name:     marker.Name,
		id:       marker.ID,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		accepted: make(chan struct{}),
		notify:   make(chan struct{}),
		marker:   marker,
		prior:    prior,
	}
	b.deletes[marker.ID] = j
	b.mu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			cancel()
			b.mu.Lock()
			if j.gone {
				if err := b.store.DeleteMarker(j.id); err != nil {
					b.logf("remote: removing the delete marker of shed %s: %v", j.name, err)
				}
				delete(b.pending, j.id)
				if m := j.marker; m != nil {
					delete(b.flushedIDs, m.LabelID)
					delete(b.flushedIDs, m.DestroyID)
				}
			}
			delete(b.deletes, j.id)
			b.finishLocked()
			b.mu.Unlock()
			close(j.done)
		}()
		timer := backend.NewPhaseTimer(fmt.Sprintf("delete name=%s backend=%s", j.name, backend.TypeRemote), nil)
		ctx := backend.ContextWithProgress(ctx, timer.Track)
		err := b.runDelete(ctx, j)
		if ctx.Err() != nil {
			b.logf("remote: delete of shed %s ended early; its marker stands: %v", j.name, ctx.Err())
			return
		}
		b.logf("timing: %s", timer.Finish(err))
	}()
}

func (b *Backend) runDelete(ctx context.Context, j *job) error {
	if j.prior != nil {
		select {
		case <-j.prior.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	ref := j.marker.Ref
	b.mu.Unlock()

	backend.Phase(ctx, "provider")
	if ref == "" {
		// The create had not been accepted when the delete was
		// acknowledged. Look for it for a while: a create in flight
		// becomes visible within seconds.
		found, err := b.findInstance(ctx, j.id)
		if err != nil {
			return fmt.Errorf("%w (the marker stands and the delete is retried)", err)
		}
		if found == "" {
			return j.finished()
		}
		ref = found
	}

	if err := b.markDeleting(ctx, j.marker, ref); err != nil {
		b.logf("remote: delete of shed %s: labelling: %v", j.name, err)
	}
	rid, err := b.markerRequestID(j.marker, &j.marker.DestroyID)
	if err != nil {
		return fmt.Errorf("%w (the marker stands and the delete is retried)", err)
	}
	pend, err := b.mutate(ctx, ref, rid, func(rid string) (provider.Pending, error) { return b.prov.Destroy(ctx, ref, rid) })
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return j.finished()
	case err != nil:
		return fmt.Errorf("destroying the machine of %s (the marker stands and the delete is retried): %w", j.name, err)
	}
	if _, err := b.prov.Wait(ctx, pend); err != nil && !errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("waiting for the machine of %s to go (the marker stands and the delete is retried): %w", j.name, err)
	}
	return j.finished()
}

// markerRequestID returns the request id in *field of a delete marker,
// making one when it has none, so a retry or the delete resumed after a
// restart sends the same one. It is returned only once a save of the marker
// holding it has been flushed in this process: a new id is saved first, and
// so is one read back from disk (a save whose rename landed and whose flush
// failed leaves it there, and a power cut could still take it away). An
// error (state.ErrNotDurable included) means the id is not known to be on
// disk and the call must not be sent; a new id is taken back, one from disk
// is kept, and the next retry saves it again.
func (b *Backend) markerRequestID(m *state.Marker, field *string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if *field != "" && b.flushedIDs[*field] {
		return *field, nil
	}
	fresh := *field == ""
	if fresh {
		*field = provider.NewRequestID()
	}
	rid := *field
	if err := b.saveMarker(m); err != nil {
		if fresh {
			*field = ""
		}
		return "", fmt.Errorf("recording a request id in the delete marker of %s: %w", m.Name, err)
	}
	b.flushedIDs[rid] = true
	return rid, nil
}

// findInstance looks for the machine of a shed ID whose create was in
// flight. It returns "" only when at least one read answered that there is
// none; when the provider could not be read at all it returns the error, so
// the caller's marker stands.
func (b *Backend) findInstance(ctx context.Context, id string) (provider.Ref, error) {
	answered := false
	var lastErr error
	for attempt := 0; attempt < findInsertAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(b.findInterval()):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		inst, err := b.prov.Get(ctx, provider.Ref(id))
		switch {
		case err == nil:
			return inst.Ref, nil
		case errors.Is(err, provider.ErrNotFound):
			answered = true
		case ctx.Err() != nil:
			return "", ctx.Err()
		default:
			lastErr = err
		}
	}
	if !answered {
		return "", fmt.Errorf("looking for the machine of shed id %s: %w", id, lastErr)
	}
	return "", nil
}

// findInterval is the pause between looks for an in-flight insert. It scales
// down with a shortened fast-fail window so tests do not wait.
func (b *Backend) findInterval() time.Duration {
	if b.opts.FastFailWindow < provider.FastFailWindow {
		return 20 * time.Millisecond
	}
	return findInsertInterval
}

// finished records that a delete job's machine is gone. The marker is
// removed as the job leaves Backend.deletes, in one critical section, so a
// marker with no job is always one whose work is still to do.
func (j *job) finished() error {
	j.gone = true
	return nil
}

// --- the link to a shed's agent ---

// shedLink is one shed's link and what the backend caches about reaching it.
type shedLink struct {
	link transport.Link
	keys *hostKeys
}

// linkFor returns the shed's link, opening it if needed. Opening touches no
// network.
func (b *Backend) linkFor(rec *state.Record) (transport.Link, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, transport.ErrClosed
	}
	id, name := rec.Shed.ID, rec.Shed.Name
	if c := b.conns[id]; c != nil {
		return c.link, nil
	}
	keys := &hostKeys{b: b, name: name, id: id}
	link, err := b.links.Open(b.rootCtx, transport.Target{
		ShedID:    id,
		ShedName:  name,
		Addr:      keys.addr,
		Connected: keys.connected,
		HostKeys:  keys,
	})
	if err != nil {
		return nil, fmt.Errorf("opening the link to %s: %w", name, err)
	}
	b.conns[id] = &shedLink{link: link, keys: keys}
	return link, nil
}

// closeLink closes and forgets a shed's link.
func (b *Backend) closeLink(id string) {
	b.mu.Lock()
	c := b.conns[id]
	delete(b.conns, id)
	b.mu.Unlock()
	if c != nil {
		_ = c.link.Close()
	}
}

// agentFor returns an agent client over the shed's link.
func (b *Backend) agentFor(rec *state.Record) (*vmutil.AgentClient, error) {
	link, err := b.linkFor(rec)
	if err != nil {
		return nil, err
	}
	return vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify), nil
}

// waitAgent probes the shed's agent until it answers. Phase "boot" lasts
// until the link connects, phase "agent" until the probe is answered. A
// machine that offers no way in this server can use (errNoAccess) or a host
// it may not dial ends the wait at once.
//
// A guest that is still booting is unreachable, and may refuse the server's
// key for a while (sshd can start before the boot unit has written the key
// file), so both are retried, the refusal for a bounded time. A host key
// that does not match the pin ends the wait at once.
func (b *Backend) waitAgent(ctx context.Context, j *job) (guest.AgentInfo, error) {
	backend.Phase(ctx, "boot")
	backend.Status(ctx, "Waiting for the VM to boot...")
	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return guest.AgentInfo{}, errSuperseded
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return guest.AgentInfo{}, err
	}

	deadline := time.Now().Add(b.opts.BootTimeout)
	var authSince time.Time
	inAgentPhase := false
	enterAgentPhase := func() {
		if !inAgentPhase {
			inAgentPhase = true
			backend.Phase(ctx, "agent")
			backend.Status(ctx, "Waiting for agent to come up...")
		}
	}
	for {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		info, err := agent.Probe(pctx)
		cancel()
		if err == nil {
			enterAgentPhase()
			return info, nil
		}
		switch {
		case ctx.Err() != nil:
			return guest.AgentInfo{}, ctx.Err()
		case errors.Is(err, transport.ErrHostKeyMismatch), errors.Is(err, transport.ErrClosed), errors.Is(err, errNoAccess):
			return guest.AgentInfo{}, err
		case errors.Is(err, transport.ErrAuth):
			if authSince.IsZero() {
				authSince = time.Now()
			} else if time.Since(authSince) > b.opts.AuthGrace {
				return guest.AgentInfo{}, fmt.Errorf("the guest still refuses the server's key after %s: %w", b.opts.AuthGrace, err)
			}
		case errors.Is(err, transport.ErrUnreachable), errors.Is(err, transport.ErrNoHostKey):
			// Still booting.
		default:
			// The link is up; the agent is not answering yet.
			enterAgentPhase()
		}
		if time.Now().After(deadline) {
			return guest.AgentInfo{}, fmt.Errorf("the agent did not answer within %s: %w", b.opts.BootTimeout, err)
		}
		select {
		case <-time.After(b.opts.ProbeInterval):
		case <-ctx.Done():
			return guest.AgentInfo{}, ctx.Err()
		}
	}
}

// linkReason picks the status reason for a failure to reach the agent.
func linkReason(err error, fallback string) string {
	switch {
	case errors.Is(err, transport.ErrHostKeyMismatch):
		return config.StatusReasonHostKey
	case errors.Is(err, errNoAccess):
		return config.StatusReasonAccess
	}
	return fallback
}
