package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
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

	// fastWindow and fastErr are guarded by Backend.mu. While fastWindow is
	// true the create handler has not answered yet, and a failure is its to
	// report.
	fastWindow bool
	fastErr    error

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

// --- cloud helpers ---

// verify checks, against the cloud's current labels, that the instance is
// this shed's. Every job calls it before each mutating cloud call.
func (b *Backend) verify(ctx context.Context, ref provider.Ref, id string) error {
	inst, err := b.prov.Get(ctx, ref)
	if err != nil {
		if unread(err) {
			return &unreadError{err}
		}
		return err
	}
	if got := inst.Labels[provider.LabelID]; got != id {
		return fmt.Errorf("%w: instance %s carries shed id %q, not %q", provider.ErrForeign, inst.Name, got, id)
	}
	if got := inst.Labels[provider.LabelEnv]; got != b.cfg.Environment {
		return fmt.Errorf("%w: instance %s is in environment %q", provider.ErrForeign, inst.Name, got)
	}
	// An instance labelled by another version of the label set is reported
	// and never touched (provider.LabelSchema). The error is not ErrForeign:
	// the instance is this shed's, so a delete keeps its marker and is tried
	// again by a server that can read the labels.
	if got := inst.Labels[provider.LabelSchema]; got != provider.SchemaVersion {
		return fmt.Errorf("%w: instance %s carries label %s=%q and this server reads %q; leaving it alone",
			provider.ErrInvalid, inst.Name, provider.LabelSchema, got, provider.SchemaVersion)
	}
	return nil
}

// unread reports whether err, from a read of one instance, means the read
// itself failed, so that nothing is known about the instance. "It is not
// there" and "it is another environment's" are answers, not failures.
func unread(err error) bool {
	return err != nil && !errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrForeign)
}

// unreadError is verify's error when its read of the instance failed, as
// opposed to a read that showed an instance the caller must not touch. A job
// may give up a mutation for it, and must never stop or destroy anything
// because of it.
type unreadError struct{ err error }

func (e *unreadError) Error() string { return e.err.Error() }
func (e *unreadError) Unwrap() error { return e.err }

func isUnread(err error) bool {
	var u *unreadError
	return errors.As(err, &u)
}

// keep ends a create or start step that cannot decide what to do: the cloud
// did not say what the instance is doing (a read failed), or showed labels
// this server does not read. Giving the job up as failed is not an option. A
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
// holds its name and its slot under the limits, because its VM may be
// running and billed; the VM's own run limit stops it in any case.
func (b *Backend) keep(j *job, kind string, cause error) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	b.mu.Lock()
	r := b.currentLocked(j.name, j.id)
	if r == nil {
		b.mu.Unlock()
		return errSuperseded
	}
	first := r.Shed.StatusReason != config.StatusReasonCloudUnreadable
	if first {
		r.Shed.StatusReason = config.StatusReasonCloudUnreadable
		b.saveLocked(r)
	}
	b.mu.Unlock()
	if first {
		// Once, not at every pass: the reason on the record says the rest.
		b.logf("remote: %s of shed %s is kept as it is, with nothing changed, and looked at again at every periodic pass: %v", kind, j.name, cause)
	}
	return fmt.Errorf("%w: %w", errUnresolved, cause)
}

// mutate runs one accepting provider call (start, stop or destroy) with the
// shed ID re-checked first. On provider.ErrBusy it waits for the transition
// in progress to end and calls again.
func (b *Backend) mutate(ctx context.Context, ref provider.Ref, id string, call func() (provider.Pending, error)) (provider.Pending, error) {
	for attempt := 0; ; attempt++ {
		if err := b.verify(ctx, ref, id); err != nil {
			return provider.Pending{}, err
		}
		p, err := call()
		if !errors.Is(err, provider.ErrBusy) || attempt >= maxBusyRetries {
			return p, err
		}
		if _, werr := b.prov.Wait(ctx, provider.Settle(ref)); werr != nil {
			return provider.Pending{}, werr
		}
	}
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
	backend.Status(ctx, "Creating the cloud instance...")
	// A create resumed after a restart goes back to the instance its first
	// attempt was given, where that instance is. Sending the request again
	// is right only when that instance is gone: Create places an instance
	// where the server is configured now, so a server that came back with
	// another zone would make a second instance and lose track of the first.
	var pend provider.Pending
	if rec.Ref != "" {
		inst, err := b.prov.Get(ctx, rec.Ref)
		switch {
		case errors.Is(err, provider.ErrNotFound):
		case unread(err):
			// The read failed, so nothing is known: the instance may be a
			// create half done, or a shed the server that died had finished
			// (labelled ready) and that the list did not show. Failing the
			// create would queue its VM for destruction either way.
			return b.keep(j, state.JobCreate, fmt.Errorf("reading instance %s: %w", rec.Ref, err))
		case err != nil:
			return b.failCreate(j, err, config.StatusReasonCreateFailed)
		case inst.Labels[provider.LabelID] != j.id || inst.Labels[provider.LabelEnv] != b.cfg.Environment:
			err := fmt.Errorf("%w: instance %s is not this shed's", provider.ErrForeign, inst.Name)
			return b.failCreate(j, err, config.StatusReasonCreateFailed)
		case inst.Labels[provider.LabelSchema] != provider.SchemaVersion:
			// Labelled by another version of the label set: reported and
			// never touched (provider.LabelSchema). Checked here, before the
			// guest is connected to, given credentials, cloned into and
			// provisioned; the check before the ready label would come after
			// all of that. Not failed either: the clean-up of a failed create
			// leaves a marker that a server reading these labels would act
			// on, and this may be a finished shed.
			return b.keep(j, state.JobCreate, fmt.Errorf("instance %s carries label %s=%q and this server reads %q; leaving it alone",
				inst.Name, provider.LabelSchema, inst.Labels[provider.LabelSchema], provider.SchemaVersion))
		case inst.ShedState() == provider.ShedStateReady:
			return b.resumeCommitted(ctx, j, inst)
		default:
			pend = provider.Pending{Ref: rec.Ref, Want: provider.StateRunning}
		}
	}
	if pend.Ref == "" {
		// The request was let in past the firewall check, but that may be
		// long ago: a create resumed after a restart was let in by a server
		// that is gone. No insert is sent while the check fails.
		if err := b.firewallGate(ctx); err != nil {
			return b.failCreate(j, err, config.StatusReasonFirewall)
		}
		// Create is idempotent on the instance name and shed ID.
		var err error
		if pend, err = b.prov.Create(ctx, *rec.Spec); err != nil {
			return b.failCreate(j, err, config.StatusReasonCreateFailed)
		}
		if !b.update(j.name, j.id, func(r *state.Record) {
			r.Ref = pend.Ref
			r.Pending = &pend
		}) {
			// Deleted while the insert was being accepted: hand the
			// delete job the instance it could not have known.
			b.setMarkerRef(j.id, pend.Ref)
			return errSuperseded
		}
	}
	inst, err := b.prov.Wait(ctx, pend)
	if err != nil {
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	b.noteInstance(j, inst)
	j.markAccepted()

	info, err := b.waitAgent(ctx, j)
	if err != nil {
		return b.failCreate(j, err, linkReason(err, config.StatusReasonCreateFailed))
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
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	owned := rec.Shed.Owner != ""
	b.credMgr.SetupCredentials(ctx, agent, j.name, nil, nil, owned)

	if rec.Shed.Repo != "" && !rec.Cloned {
		repo := j.repoURL
		if repo == "" {
			repo = rec.Shed.Repo
		}
		backend.Phase(ctx, "repo")
		backend.Status(ctx, "Cloning repository...")
		if err := b.cloneRepo(ctx, agent, rec, repo, owned); err != nil {
			if ctx.Err() != nil {
				return errSuperseded
			}
			b.logf("Warning: failed to clone repo %s: %v", config.SanitizeRepoURL(rec.Shed.Repo), err)
			backend.StatusWarning(ctx, "Failed to clone repository (see server logs for details)")
		} else {
			backend.Status(ctx, "Repository cloned")
		}
		b.update(j.name, j.id, func(r *state.Record) { r.Cloned = true })
	}

	if !rec.NoProvision && !rec.Provisioned {
		backend.Phase(ctx, "provision")
		b.provision(ctx, agent, rec, true)
		if ctx.Err() != nil {
			return errSuperseded
		}
		b.update(j.name, j.id, func(r *state.Record) { r.Provisioned = true })
	}

	backend.Phase(ctx, "ready")
	if err := b.verify(ctx, rec.Ref, j.id); err != nil {
		if isUnread(err) {
			// Everything but the label is done and the instance cannot be
			// read just now. The create is kept, not failed and destroyed;
			// run again, it skips the steps recorded as done.
			return b.keep(j, state.JobCreate, fmt.Errorf("reading instance %s before labelling it ready: %w", rec.Ref, err))
		}
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	labels := map[string]string{
		provider.LabelState:  string(provider.ShedStateReady),
		provider.LabelLayout: provider.LayoutLabel(info.Layout),
	}
	if err := b.prov.SetLabels(ctx, rec.Ref, labels); err != nil {
		return b.failCreate(j, err, config.StatusReasonCreateFailed)
	}
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusRunning
		r.Shed.StatusReason = ""
		r.Job = ""
		r.Pending = nil
		r.Ready = true
		r.Layout = info.Layout
	}) {
		return errSuperseded
	}
	backend.Status(ctx, "Shed is running")
	return nil
}

// commitCreatedLocked repairs the record of a create that the cloud shows as
// finished. A create's last two writes are the shed_state=ready label and the
// local Ready flag, in that order; a server that died between them left a
// finished shed with a record that still says "create in flight". The label
// is the commit point: such a shed is never created again and never cleaned
// up as a failed create, whatever its instance is doing now. The record takes
// the status an adopted shed would get.
func (b *Backend) commitCreatedLocked(r *state.Record, inst provider.Instance) {
	r.Ready, r.Cloned, r.Provisioned = true, true, true
	r.Pending = nil
	if r.Ref == "" {
		r.Ref = inst.Ref
	}
	if l := inst.Layout(); l != 0 {
		r.Layout = l
	}
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
		r.Address = inst.Address
		r.AddressAt = b.now()
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
	b.commitCreatedLocked(r, inst)
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

// failCreate records a failed create. Inside the fast-fail window a refusal
// (quota, capacity, an invalid request) is handed to the create handler and
// leaves no record; any other failure leaves the shed in status "error".
//
// Unless the VM is kept for inspection it is destroyed, by a delete job
// backed by a marker written here, in the same critical section as the
// status: a destroy that fails, or a server that dies first, is retried by
// the periodic pass and at the next start. The marker is written even when
// the cloud gave no instance back, unless the cloud refused the request
// outright: an insert whose reply was lost may still have made an instance,
// and the delete job looks for it by shed ID.
func (b *Backend) failCreate(j *job, cause error, reason string) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	if r := provider.Reason(cause); r != "" {
		reason = r
	}
	refused := provider.IsRefusal(cause) || errors.Is(cause, provider.ErrInvalid)
	b.mu.Lock()
	rec := b.currentLocked(j.name, j.id)
	if rec == nil {
		b.mu.Unlock()
		return errSuperseded
	}
	ref := rec.Ref
	removed := j.fastWindow && refused
	if j.fastWindow {
		j.fastErr = cause
	}
	var marker *state.Marker
	if !b.opts.KeepFailedCreate && (ref != "" || !refused) {
		marker = &state.Marker{
			ID: j.id, Name: j.name, Env: rec.Env, Ref: ref, At: b.now().UTC(),
			Owner: rec.Shed.Owner, CreatedAt: rec.Shed.CreatedAt, KeepRecord: !removed,
		}
		if err := b.saveMarkerLocked(marker); err != nil {
			b.logf("remote: create of shed %s failed and its clean-up could not be recorded: %v", j.name, err)
		}
	}
	if removed {
		b.dropLocked(j.name)
	} else {
		rec.Shed.Status = config.StatusError
		rec.Shed.StatusReason = reason
		rec.Job = ""
		rec.Queued = ""
		rec.Pending = nil
		b.saveLocked(rec)
	}
	b.mu.Unlock()

	b.logf("remote: create of shed %s failed (%s): %v", j.name, reason, cause)
	b.credMgr.StopListener(j.name)
	b.closeLink(j.id)
	if marker != nil {
		b.startDelete(marker, nil)
	}
	return cause
}

// noteInstance copies what the cloud reported about a started instance into
// the record.
func (b *Backend) noteInstance(j *job, inst provider.Instance) {
	now := b.now()
	b.update(j.name, j.id, func(r *state.Record) {
		r.Pending = nil
		r.Zone = inst.Zone
		if inst.Ref != "" {
			r.Ref = inst.Ref
		}
		r.Address = inst.Address
		r.AddressAt = now
	})
}

// cloneRepo clones the shed's repository through the agent. Each attempt runs
// under its own deadline, and a failed attempt is retried a bounded number of
// times: a guest's first clone can fail or hang while its network settles.
// Git never prompts (GIT_TERMINAL_PROMPT=0), so a repository that needs
// credentials the URL lacks fails at once instead of waiting on a terminal.
// repo may carry credentials; it is never logged.
func (b *Backend) cloneRepo(ctx context.Context, agent *vmutil.AgentClient, rec *state.Record, repo string, owned bool) error {
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
		b.logf("remote: clone of %s into shed %s failed (attempt %d of %d); retrying in %s: %v",
			shown, rec.Shed.Name, attempt, cloneAttempts, b.opts.CloneRetryPause, err)
		backend.Status(ctx, fmt.Sprintf("Clone failed; retrying (attempt %d of %d)...", attempt+1, cloneAttempts))
		b.removePartialClone(ctx, agent, repo)
		select {
		case <-time.After(b.opts.CloneRetryPause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// removePartialClone removes what a failed clone left behind, so the next
// attempt does not find a directory in its way. Best-effort.
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

// provision runs the shed's provisioning hooks through the agent, the same
// way the local backends do. Best-effort: failures are logged.
func (b *Backend) provision(ctx context.Context, agent *vmutil.AgentClient, rec *state.Record, runInstall bool) {
	workDir := rec.Shed.LandingDir
	if workDir == "" {
		workDir = config.HomePath
	}
	provisioner := vmutil.NewProvisioner(agent, rec.Shed.Name)
	provisioner.SetWorkDir(workDir)
	provisioner.SetOutput(os.Stdout, os.Stderr)
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
	backend.Status(ctx, "Starting the cloud instance...")
	// The request was let in past the firewall check, but the job may run
	// much later: resumed after a restart, or queued behind a stop. No start
	// is sent to a stopped VM while the check fails. A VM that is already on
	// its way up or running (a resumed start the cloud had accepted) is
	// followed, not judged again: the check never stops a VM, and the record
	// must not call a running one stopped.
	if gerr := b.firewallGate(ctx); gerr != nil {
		if ctx.Err() != nil {
			return errSuperseded
		}
		inst, err := b.prov.Get(ctx, rec.Ref)
		if unread(err) {
			// Whether the VM is up is not known. A failed start stops the
			// VM (failStart), and this one may be running, so the start is
			// neither failed nor refused: it is kept and looked at again.
			return b.keep(j, state.JobStart, fmt.Errorf("the firewall check fails (%w) and instance %s could not be read: %w", gerr, rec.Ref, err))
		}
		if err != nil {
			return b.failStart(j, err, "", false)
		}
		if inst.State != provider.StateStarting && inst.State != provider.StateRunning {
			return b.refuseStart(j, gerr)
		}
	}
	maxRun := b.runLimit(rec.Shed.Owner)
	pend, err := b.mutate(ctx, rec.Ref, j.id, func() (provider.Pending, error) {
		return b.prov.Start(ctx, rec.Ref, provider.StartOptions{MaxRun: maxRun})
	})
	if isUnread(err) {
		// The read that precedes the start call failed and no start was
		// accepted. The VM may be up all the same (a start resumed after a
		// restart), so it is not stopped on the strength of that.
		return b.keep(j, state.JobStart, fmt.Errorf("reading instance %s before starting it: %w", rec.Ref, err))
	}
	if err != nil {
		return b.failStart(j, err, "", false)
	}
	b.update(j.name, j.id, func(r *state.Record) {
		r.Pending = &pend
		r.MaxRun = maxRun
	})
	inst, err := b.prov.Wait(ctx, pend)
	if err != nil {
		return b.failStart(j, err, "", false)
	}
	b.noteInstance(j, inst)

	info, err := b.waitAgent(ctx, j)
	if err != nil {
		return b.failStart(j, err, linkReason(err, config.StatusReasonUnreachable), true)
	}
	if !guest.ProtocolCompatible(info.Protocol) || !guest.LayoutSupported(info.Layout) {
		err := fmt.Errorf("the guest agent reports protocol %d and layout %d, which this server cannot drive", info.Protocol, info.Layout)
		return b.failStart(j, err, config.StatusReasonLayout, true)
	}

	rec = b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	agent, err := b.agentFor(rec)
	if err != nil {
		return b.failStart(j, err, config.StatusReasonUnreachable, true)
	}
	b.credMgr.SetupCredentials(ctx, agent, j.name, nil, nil, rec.Shed.Owner != "")
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
		r.Pending = nil
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
func (b *Backend) failStart(j *job, cause error, reason string, stopVM bool) error {
	if j.ctx.Err() != nil {
		return errSuperseded
	}
	if r := provider.Reason(cause); r != "" {
		reason = r
		stopVM = false
	}
	b.credMgr.StopListener(j.name)
	b.closeLink(j.id)

	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	if errors.Is(cause, provider.ErrNotFound) {
		b.logf("remote: start of shed %s: its instance is gone; dropping the record: %v", j.name, cause)
		b.forget(j.name, j.id)
		return cause
	}
	b.logf("remote: start of shed %s failed (%s): %v", j.name, reason, cause)
	if stopVM || !provider.IsRefusal(cause) {
		b.stopQuietly(rec.Ref, j.id, "failed start of "+j.name)
	}
	b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusStopped
		r.Shed.StatusReason = reason
		r.Job = ""
		r.Queued = ""
		r.Pending = nil
		r.Address = ""
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
		r.Pending = nil
		r.Address = ""
	}) {
		return errSuperseded
	}
	return cause
}

// stopQuietly stops an instance on a bounded context of its own.
func (b *Backend) stopQuietly(ref provider.Ref, id, why string) {
	ctx, cancel := context.WithTimeout(b.rootCtx, cleanupTimeout)
	defer cancel()
	pend, err := b.mutate(ctx, ref, id, func() (provider.Pending, error) { return b.prov.Stop(ctx, ref) })
	if err == nil {
		_, err = b.prov.Wait(ctx, pend)
	}
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		b.logf("remote: stopping the instance of a %s: %v", why, err)
	}
}

// forget removes a shed whose instance is gone: record, link and channel.
func (b *Backend) forget(name, id string) {
	b.mu.Lock()
	if b.currentLocked(name, id) != nil {
		b.dropLocked(name)
	}
	b.mu.Unlock()
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
	backend.Status(ctx, "Stopping the cloud instance...")
	b.credMgr.StopListener(j.name)

	pend, err := b.mutate(ctx, rec.Ref, j.id, func() (provider.Pending, error) { return b.prov.Stop(ctx, rec.Ref) })
	if err == nil {
		b.update(j.name, j.id, func(r *state.Record) { r.Pending = &pend })
		_, err = b.prov.Wait(ctx, pend)
	}
	if err != nil {
		return b.failStop(j, rec.Ref, err)
	}
	b.closeLink(j.id)
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusStopped
		r.Shed.StatusReason = ""
		r.Job = ""
		r.Pending = nil
		r.Address = ""
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
		b.logf("remote: stop of shed %s: its instance is gone; dropping the record: %v", j.name, cause)
		b.forget(j.name, j.id)
		return cause
	}
	b.logf("remote: stop of shed %s failed: %v", j.name, cause)
	ctx, cancel := context.WithTimeout(b.rootCtx, probeTimeout)
	defer cancel()
	inst, err := b.prov.Get(ctx, ref)
	if err != nil {
		b.update(j.name, j.id, func(r *state.Record) { r.Job = ""; r.Pending = nil })
		return cause
	}
	b.update(j.name, j.id, func(r *state.Record) {
		r.Pending = nil
		switch inst.State {
		case provider.StateStopped:
			r.Shed.Status = config.StatusStopped
			r.Job = ""
			r.Address = ""
			b.closeLinkLater(j.id)
		case provider.StateRunning:
			r.Shed.Status = config.StatusRunning
			r.Job = ""
		default:
			r.Job = ""
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

// setMarkerRef records the instance of a shed whose delete was acknowledged
// before its insert had been accepted.
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
		// The insert had not been accepted when the delete was
		// acknowledged. Look for it for a while: an insert in flight
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

	if err := b.markDeleting(ctx, ref, j.id); err != nil {
		if errors.Is(err, provider.ErrForeign) {
			b.logf("remote: delete of shed %s: %v; leaving the instance alone", j.name, err)
			return j.finished()
		}
		b.logf("remote: delete of shed %s: labelling: %v", j.name, err)
	}
	pend, err := b.mutate(ctx, ref, j.id, func() (provider.Pending, error) { return b.prov.Destroy(ctx, ref) })
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return j.finished()
	case errors.Is(err, provider.ErrForeign):
		b.logf("remote: delete of shed %s: %v; leaving the instance alone", j.name, err)
		return j.finished()
	case err != nil:
		return fmt.Errorf("destroying the instance of %s (the marker stands and the delete is retried): %w", j.name, err)
	}
	if _, err := b.prov.Wait(ctx, pend); err != nil && !errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("waiting for the instance of %s to go (the marker stands and the delete is retried): %w", j.name, err)
	}
	return j.finished()
}

// findInstance looks for the instance carrying a shed ID. It returns "" only
// when at least one complete list did not show it; when the cloud could not
// be listed at all it returns the error, so the caller's marker stands.
func (b *Backend) findInstance(ctx context.Context, id string) (provider.Ref, error) {
	listed := false
	var lastErr error
	for attempt := 0; attempt < findInsertAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(b.findInterval()):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		insts, err := b.prov.List(ctx, provider.Selector{Env: b.cfg.Environment})
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = err
			continue
		}
		listed = true
		for _, inst := range insts {
			if inst.Labels[provider.LabelID] == id {
				return inst.Ref, nil
			}
		}
	}
	if !listed {
		return "", fmt.Errorf("looking for the instance of shed id %s: %w", id, lastErr)
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

// finished records that a delete job's instance is gone (or was never the
// shed's own). The marker is removed as the job leaves Backend.deletes, in
// one critical section, so a marker with no job is always one whose work is
// still to do.
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
		ShedID:   id,
		ShedName: name,
		Addr:     keys.addr,
		HostKeys: keys,
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
// until the link connects, phase "agent" until the probe is answered.
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
		case errors.Is(err, transport.ErrHostKeyMismatch), errors.Is(err, transport.ErrClosed), errors.Is(err, errNoRelearn):
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
	if errors.Is(err, transport.ErrHostKeyMismatch) {
		return config.StatusReasonHostKey
	}
	return fallback
}
