package remote

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
)

// startSweeper starts the periodic pass, once, after the start-up pass.
func (b *Backend) startSweeper() {
	if b.opts.SweepInterval <= 0 {
		return
	}
	b.sweepOnce.Do(func() {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return
		}
		b.wg.Add(1)
		b.mu.Unlock()
		go func() {
			defer b.wg.Done()
			ticker := time.NewTicker(b.opts.SweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-b.rootCtx.Done():
					return
				case <-ticker.C:
				}
				ctx, cancel := context.WithTimeout(b.rootCtx, sweepTimeout)
				err := b.Sweep(ctx)
				cancel()
				if err != nil && b.rootCtx.Err() == nil {
					b.logf("remote: periodic pass: %v", err)
				}
			}
		}()
	})
}

// sweepCheck is a shed whose record and the provider's list disagree. cause
// is why the list says the service deleted its machine ("" when it does not
// say).
type sweepCheck struct {
	name, id string
	ref      provider.Ref
	status   string
	listed   bool
	cause    string
}

// Sweep is the periodic pass. It reads the catalog again, runs the firewall
// check, lists this server's machines once and then, for sheds with no job
// in flight:
//
//   - starts the delete job of every delete marker that has none (a destroy
//     that failed is retried here, not only at the next server start);
//   - writes a delete marker for a machine still labelled creating
//     StaleCreateAge after the provider made it, with no record: a create
//     whose record was lost, which nothing else would ever remove;
//   - records a "running" shed whose machine stopped (from outside) as
//     "stopped", with the reason;
//   - drops the record of a shed whose machine no longer exists, once an
//     exact read confirms it, naming the cause when the list gives the
//     machine's deletion (its lease ended, a delete at the service);
//   - records a "stopped" shed whose machine runs as "starting" and probes
//     it, so a machine that costs money is never shown as stopped;
//   - gives a shed left "stopping" by a failed call a new stop job;
//   - runs again the create or start of a shed kept "starting" because the
//     provider could not be read when its job looked (Backend.keep);
//   - brings each listed shed's owner into line with its machine's
//     (reconcileUserLocked).
//
// A machine the service is deleting is never answered with a stop. The pass
// destroys nothing itself: only a delete job does, and a delete job exists
// only for a delete marker. Each correction is confirmed with a fresh read
// of the one machine and applied only if the shed is still as it was, so a
// request that arrives during the pass wins. A list that fails drops
// nothing.
func (b *Backend) Sweep(ctx context.Context) error {
	if err := b.checkReady(); err != nil {
		return err
	}
	if err := b.refreshCatalog(ctx); err != nil {
		b.logf("remote: periodic pass: reading the catalog (the last one is kept): %v", err)
	}
	// Refresh the firewall check. Its result gates creates and starts only;
	// nothing below depends on it, and nothing is stopped or deleted for it.
	b.CheckFirewall(ctx)
	listing, err := b.prov.List(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]provider.Instance, len(listing.Instances))
	for _, inst := range listing.Instances {
		byID[string(inst.Ref)] = inst
	}
	deleted := deletionCauses(listing.Deleted)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	// Read under the lock: a delete job clears its marker before it leaves
	// b.deletes, so a marker seen here with no job is one nobody works on.
	markers, err := b.store.Markers()
	if err != nil {
		b.logf("remote: periodic pass: reading delete markers: %v", err)
	}
	var retry, abandoned []*state.Marker
	now := b.now()
	for _, m := range markers {
		if b.deletes[m.ID] == nil && now.Sub(m.At) >= b.opts.DeleteRetryAge {
			retry = append(retry, m)
		}
	}
	// A create nobody is finishing (see abandonedCreate) gets a marker, and
	// with it a delete job.
	recorded := make(map[string]bool, len(b.records))
	for _, r := range b.records {
		recorded[r.Shed.ID] = true
	}
	marked := make(map[string]bool, len(markers))
	for _, m := range markers {
		marked[m.ID] = true
	}
	for _, inst := range listing.Instances {
		id := string(inst.Ref)
		if recorded[id] || marked[id] || b.deletes[id] != nil || !b.abandonedCreate(inst) {
			continue
		}
		m := &state.Marker{ID: id, Name: inst.Name, Ref: inst.Ref, At: now.UTC()}
		if err := b.saveMarkerLocked(m); err != nil {
			b.logf("remote: periodic pass: machine %s (shed %s) is an abandoned create but its marker could not be written: %v", id, inst.Name, err)
			continue
		}
		b.logf("remote: periodic pass: machine %s (shed %s) has been %q since %s with no local record; destroying it",
			id, inst.Name, inst.ShedState(), inst.CreatedAt.UTC().Format(time.RFC3339))
		abandoned = append(abandoned, m)
	}
	var checks []sweepCheck
	var launch []*job
	for name, r := range b.records {
		if b.jobs[name] != nil {
			continue
		}
		inst, listed := byID[r.Shed.ID]
		scheduled := len(checks) + len(launch)
		switch r.Shed.Status {
		case config.StatusStarting:
			// A create or start with no job is one that was kept because
			// the cloud could not be read (Backend.keep). It is run again
			// here: one read of its instance, and on from what that shows.
			// A probe is not: a start call or the next server start
			// retries it.
			if (r.Job == state.JobCreate && !r.Ready) || r.Job == state.JobStart {
				if j := b.healLocked(r); j != nil {
					launch = append(launch, j)
				}
			}
		case config.StatusStopping:
			if j := b.healLocked(r); j != nil {
				b.logf("remote: periodic pass: shed %s is stopping with no job; stopping it again", name)
				launch = append(launch, j)
			}
		case config.StatusRunning:
			if r.Ref != "" && (!listed || inst.State == provider.StateStopped || inst.State == provider.StateStopping) {
				checks = append(checks, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status, listed: listed, cause: deleted[r.Shed.ID]})
			}
		case config.StatusStopped:
			if r.Ref != "" && (!listed || inst.State == provider.StateRunning || inst.State == provider.StateStarting) {
				checks = append(checks, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status, listed: listed, cause: deleted[r.Shed.ID]})
			}
		}
		// A listed machine that needs no correction and no job (its state
		// matches the record) is still a read of it: its facts are kept,
		// and an owner that changes on a shed that keeps running is noticed
		// at this pass, not at its next restart. A correction or a job
		// reads the machine again and reconciles there.
		if listed && len(checks)+len(launch) == scheduled {
			seen, image := r.Machine, r.Shed.Image
			b.noteMachineLocked(r, inst)
			// Written only when it changed: a pass over unchanged sheds
			// rewrites (and fsyncs) no record.
			if b.reconcileUserFromLocked(r, inst) || seen == nil || r.Shed.Image != image || !reflect.DeepEqual(*seen, *r.Machine) {
				b.saveLocked(r)
			}
		}
	}
	b.mu.Unlock()

	for _, m := range retry {
		b.logf("remote: periodic pass: retrying the delete of shed %s (%s)", m.Name, m.ID)
		b.startDelete(m, nil)
	}
	for _, m := range abandoned {
		b.startDelete(m, nil)
	}
	for _, j := range launch {
		b.launch(j)
	}
	for _, c := range checks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.correct(ctx, c)
	}
	return nil
}

// correct re-reads one machine and brings its shed's record into line.
func (b *Backend) correct(ctx context.Context, c sweepCheck) {
	inst, err := b.prov.Get(ctx, c.ref)
	gone := errors.Is(err, provider.ErrNotFound)
	if err != nil && !gone {
		b.logf("remote: reading the machine of shed %s (its record is kept): %v", c.name, err)
		return
	}

	var launch *job
	dropped, stopped := false, false
	b.mu.Lock()
	r := b.currentLocked(c.name, c.id)
	if r == nil || b.closed || b.jobs[c.name] != nil || r.Shed.Status != c.status || r.Ref != c.ref {
		b.mu.Unlock()
		return // a request got there first
	}
	if !gone {
		b.noteMachineLocked(r, inst)
		b.reconcileUserLocked(r, inst, b.logNote)
		b.saveLocked(r)
	}
	var event *audit.ShedEvent
	switch {
	case gone && !c.listed && c.cause != "":
		b.logf("remote: shed %s (%s): the account service deleted its machine (cause %s); dropping its record", c.name, c.id, c.cause)
		event = serviceDeletionEvent(r, c.cause)
		b.dropLocked(c.name)
		dropped = true
	case gone && !c.listed:
		b.logf("remote: shed %s (%s) has no machine any more; dropping its record", c.name, c.id)
		b.dropLocked(c.name)
		dropped = true
	case gone:
		// Listed a moment ago and gone now: decide at the next pass.
	case c.status == config.StatusRunning && inst.State == provider.StateStopped:
		b.recordStoppedLocked(r, inst)
		stopped = true
		b.logf("remote: periodic pass: shed %s was running and the cloud reports it stopped; recording it stopped (%s)", c.name, r.Shed.StatusReason)
	case c.status == config.StatusRunning && inst.State == provider.StateStopping:
		r.Shed.Status = config.StatusStopping
		r.Shed.StatusReason = ""
		r.Job = state.JobStop
		b.saveLocked(r)
		launch = b.newJobLocked(state.JobStop, c.name, c.id)
	case c.status == config.StatusStopped && (inst.State == provider.StateRunning || inst.State == provider.StateStarting):
		// It runs, so it holds a run slot and costs money, whatever
		// max_running says. The probe job never starts or stops it.
		b.logf("remote: periodic pass: shed %s is recorded stopped and the cloud reports %s; recording it starting and probing it", c.name, inst.State)
		r.Shed.Status = config.StatusStarting
		r.Shed.StatusReason = ""
		r.Job = state.JobProbe
		b.saveLocked(r)
		launch = b.newJobLocked(state.JobProbe, c.name, c.id)
	}
	auditLog := b.auditLog
	b.mu.Unlock()

	b.recordServiceDeletion(auditLog, event, c.name)

	if dropped || stopped {
		b.credMgr.StopListener(c.name)
		b.closeLink(c.id)
	}
	if launch != nil {
		b.launch(launch)
	}
}

// recordStoppedLocked records a shed whose machine stopped without this
// server asking: by something outside.
func (b *Backend) recordStoppedLocked(r *state.Record, inst provider.Instance) {
	r.Shed.Status = config.StatusStopped
	r.Shed.StatusReason = config.StatusReasonExternal
	r.Job = ""
	r.Queued = ""
	r.Pending, r.Mutation = nil, nil
	b.noteMachineLocked(r, inst)
	b.saveLocked(r)
}

// serviceDeletionEvent is the activity event of a shed whose machine the
// account service deleted (its lease ended, a delete at the service).
func serviceDeletionEvent(r *state.Record, cause string) *audit.ShedEvent {
	return &audit.ShedEvent{Event: audit.EventShedDelete, Identity: audit.SystemIdentity(), ShedRef: shedRefOf(r), Result: audit.ResultOK, Cause: cause}
}

// recordServiceDeletion writes a service deletion's event, when there is
// one and an audit log.
func (b *Backend) recordServiceDeletion(auditLog *audit.Log, event *audit.ShedEvent, name string) {
	if event == nil || auditLog == nil {
		return
	}
	if err := auditLog.Record(event); err != nil {
		b.logf("remote: recording the service's delete of shed %s: %v", name, err)
	}
}

// shedRefOf is the shed an audit event names, as its record reports it.
func shedRefOf(r *state.Record) audit.ShedRef {
	owner, user := ownership(r)
	return audit.ShedRef{Shed: r.Shed.Name, ShedID: r.Shed.ID, ShedUser: user, ShedOwner: owner}
}
