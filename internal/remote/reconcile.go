package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
)

// listRetry is the pause between attempts at the first cloud list.
const listRetry = 5 * time.Second

// Reconcile is the start-up pass. It lists this environment's instances once
// (retrying until the list is complete or ctx ends), repairs the local cache
// against it, resumes the jobs a previous server left in flight, and then
// closes Ready. The server runs it once, in the background; shed routes wait
// for Ready.
//
// What the pass does:
//   - a "ready" instance with no record is adopted: its record is rebuilt
//     from labels, shed-spec metadata and cloud state. A running one stays
//     "starting" until its agent answers a probe;
//   - a record that was "starting" or "stopping" gets a new job that carries
//     on from what the cloud says now;
//   - an instance with a local delete marker, or labelled
//     shed_state=deleting, is destroyed. Nothing else is ever destroyed;
//   - a record whose instance is not in the list is dropped once a read of
//     that one instance confirms it is gone, unless the record is in "error"
//     or its create had not reached the cloud yet;
//   - anything else is logged and left alone.
func (b *Backend) Reconcile(ctx context.Context) {
	var insts []provider.Instance
	for {
		var err error
		insts, err = b.prov.List(ctx, provider.Selector{Env: b.cfg.Environment})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		b.logf("remote: reconcile: listing environment %s failed; retrying in %s: %v", b.cfg.Environment, listRetry, err)
		select {
		case <-time.After(listRetry):
		case <-ctx.Done():
			return
		case <-b.rootCtx.Done():
			return
		}
	}
	b.reconcile(ctx, insts)
	b.readyOnce.Do(func() { close(b.ready) })
	b.startSweeper()
}

func (b *Backend) reconcile(ctx context.Context, insts []provider.Instance) {
	var report []string
	note := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		report = append(report, line)
		b.logf("remote: reconcile: %s", line)
	}

	markers, err := b.store.Markers()
	if err != nil {
		note("could not read delete markers: %v", err)
	}
	marked := make(map[string]*state.Marker, len(markers))
	for _, m := range markers {
		marked[m.ID] = m
	}

	byID := make(map[string]provider.Instance, len(insts))
	var deletes []*state.Marker
	var launch []*job
	var resumeChannels []*state.Record
	var confirm []sweepCheck

	b.mu.Lock()
	for _, m := range markers {
		b.pending[m.ID] = m.At
	}
	recordIDs := make(map[string]string, len(b.records)) // shed ID -> name
	for name, r := range b.records {
		recordIDs[r.Shed.ID] = name
	}

	// A record that survived beside its own delete marker (the server died
	// between the two writes) is dropped: the delete was acknowledged.
	// The marker of a failed create's clean-up keeps the record, in
	// "error": if the server died between the marker and the status, the
	// status is written now.
	for id, m := range marked {
		name, ok := recordIDs[id]
		if !ok {
			continue
		}
		if m.KeepRecord {
			if r := b.records[name]; r.Shed.Status != config.StatusError {
				r.Shed.Status = config.StatusError
				r.Shed.StatusReason = config.StatusReasonCreateFailed
				r.Job, r.Queued, r.Pending = "", "", nil
				b.saveLocked(r)
			}
			continue
		}
		b.dropLocked(name)
		delete(recordIDs, id)
	}

	for _, inst := range insts {
		id, err := inst.Identity()
		if err != nil {
			note("instance %s has labels this server cannot read (%v); leaving it alone", inst.Name, err)
			continue
		}
		if id.Env != b.cfg.Environment {
			continue
		}
		if _, dup := byID[id.ID]; dup {
			note("two instances carry shed id %s; leaving %s alone", id.ID, inst.Name)
			continue
		}
		byID[id.ID] = inst

		if m := marked[id.ID]; m != nil {
			if m.Ref == "" {
				m.Ref = inst.Ref
			}
			continue // destroyed below, with every other marker
		}
		if inst.ShedState() == provider.ShedStateDeleting {
			m := &state.Marker{ID: id.ID, Name: id.Name, Env: id.Env, Ref: inst.Ref, At: b.now().UTC()}
			if err := b.saveMarkerLocked(m); err != nil {
				note("instance %s is labelled deleting but its marker could not be written: %v", inst.Name, err)
				continue
			}
			if name, ok := recordIDs[id.ID]; ok {
				b.dropLocked(name)
				delete(recordIDs, id.ID)
			}
			deletes = append(deletes, m)
			continue
		}
		if _, known := recordIDs[id.ID]; known {
			continue
		}

		// No record. Adopt it only if it was a finished shed.
		if inst.ShedState() != provider.ShedStateReady {
			note("instance %s (shed %s) is %q with no local record; leaving it alone", inst.Name, id.Name, inst.ShedState())
			continue
		}
		if other := b.records[id.Name]; other != nil {
			note("instance %s claims shed name %s, which shed %s already has; leaving it alone", inst.Name, id.Name, other.Shed.ID)
			continue
		}
		rec, err := b.adoptedRecord(inst, id)
		if err != nil {
			note("instance %s (shed %s) cannot be adopted: %v", inst.Name, id.Name, err)
			continue
		}
		if err := b.store.SaveRecord(rec); err != nil {
			note("instance %s (shed %s) could not be recorded: %v", inst.Name, id.Name, err)
			continue
		}
		b.records[id.Name] = rec
		recordIDs[id.ID] = id.Name
		b.logf("remote: reconcile: adopted shed %s (%s) as %s", id.Name, id.ID, rec.Shed.Status)
		if rec.Job != "" {
			launch = append(launch, b.newJobLocked(rec.Job, id.Name, id.ID))
		}
	}

	for name, r := range b.records {
		if b.jobs[name] != nil {
			continue // adopted just now
		}
		inst, present := byID[r.Shed.ID]
		if !present {
			switch {
			case r.Shed.Status == config.StatusError:
				// An error record outlives its instance.
			case r.Job == state.JobCreate && !r.Ready:
				// The create had not finished; it is sent again,
				// and Create is idempotent.
				launch = append(launch, b.newJobLocked(state.JobCreate, name, r.Shed.ID))
			case r.Ref != "":
				// Not in the list. That is not proof the instance is
				// gone (a server restarted with another zone lists
				// none of its sheds), so the one instance is read
				// before the record is dropped.
				confirm = append(confirm, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status})
			default:
				note("shed %s (%s) never had an instance; dropping its record", name, r.Shed.ID)
				b.dropLocked(name)
			}
			continue
		}
		if r.Ref == "" {
			r.Ref = inst.Ref
			b.saveLocked(r)
		}
		if r.Job == state.JobCreate && !r.Ready && inst.ShedState() == provider.ShedStateReady {
			// The create's last cloud write landed and its last local one
			// did not. It is a finished shed: resuming the create could
			// only end in the clean-up of a failed one, which destroys
			// the VM and its disk.
			b.commitCreatedLocked(r, inst)
			note("shed %s is labelled %s and its record still held the create; recording it %s", name, provider.ShedStateReady, r.Shed.Status)
		}
		switch r.Shed.Status {
		case config.StatusStarting, config.StatusStopping:
			kind := r.Job
			if kind == "" {
				kind = state.JobStart
				if r.Shed.Status == config.StatusStopping {
					kind = state.JobStop
				}
				r.Job = kind
				b.saveLocked(r)
			}
			launch = append(launch, b.newJobLocked(kind, name, r.Shed.ID))
		case config.StatusRunning:
			switch inst.State {
			case provider.StateRunning:
				resumeChannels = append(resumeChannels, r.Clone())
			case provider.StateStopped:
				b.recordStoppedLocked(r, inst)
				note("shed %s was running and the cloud reports it stopped; recording it stopped (%s)", name, r.Shed.StatusReason)
			case provider.StateStopping:
				r.Shed.Status = config.StatusStopping
				r.Job = state.JobStop
				b.saveLocked(r)
				launch = append(launch, b.newJobLocked(state.JobStop, name, r.Shed.ID))
			default:
				note("shed %s is recorded running and the cloud reports %s; leaving it alone", name, inst.State)
			}
		case config.StatusStopped:
			if inst.State != provider.StateStopped {
				note("shed %s is recorded stopped and the cloud reports %s; leaving it alone", name, inst.State)
			}
		}
	}
	b.reported = report
	b.mu.Unlock()

	for _, c := range confirm {
		b.correct(ctx, c)
	}

	for _, m := range markers {
		b.startDelete(m, nil)
	}
	for _, m := range deletes {
		b.startDelete(m, nil)
	}
	for _, j := range launch {
		b.launch(j)
	}
	for _, rec := range resumeChannels {
		agent, err := b.agentFor(rec)
		if err != nil {
			b.logf("remote: reconcile: no link to shed %s: %v", rec.Shed.Name, err)
			continue
		}
		b.credMgr.ResumeMessageChannel(rec.Shed.Name, agent, rec.Shed.Owner != "")
	}
}

// adoptedRecord rebuilds a shed's record from its instance: name, ID and
// owner from labels, the shed-spec metadata, and the cloud's own facts.
func (b *Backend) adoptedRecord(inst provider.Instance, id provider.Identity) (*state.Record, error) {
	spec, err := inst.ShedSpec()
	if err != nil {
		return nil, err
	}
	rec := &state.Record{
		Shed: config.Shed{
			Name:           id.Name,
			ID:             id.ID,
			CreatedAt:      spec.CreatedAt.UTC(),
			Repo:           spec.Repo,
			Backend:        config.BackendRemote,
			CPUs:           inst.CPUs,
			MemoryMB:       inst.MemoryMB,
			LandingDir:     spec.LandingDir,
			Image:          spec.Image,
			Owner:          id.Owner,
			NetworkProfile: spec.Network,
		},
		Env:         id.Env,
		Zone:        inst.Zone,
		Ref:         inst.Ref,
		Ready:       true,
		Cloned:      true,
		Provisioned: true,
		HostKeyPin:  inst.HostKeyPin(),
		Layout:      inst.Layout(),
		Size:        spec.Size,
		DiskGB:      inst.DiskGB,
		MaxRun:      inst.MaxRun,
	}
	switch inst.State {
	case provider.StateStopped:
		rec.Shed.Status = config.StatusStopped
	case provider.StateStopping:
		rec.Shed.Status = config.StatusStopping
		rec.Job = state.JobStop
	default:
		// Running, or on its way: "starting" until a probe succeeds.
		rec.Shed.Status = config.StatusStarting
		rec.Job = state.JobProbe
		rec.Address = inst.Address
		rec.AddressAt = b.now()
	}
	return rec, nil
}

// runProbe is the job of an adopted shed whose instance runs: wait for the
// cloud to settle, then for the agent to answer. It never starts, stops or
// changes the instance. If the agent cannot be reached the shed stays
// "starting" with a reason, and a later start call probes again.
func (b *Backend) runProbe(ctx context.Context, j *job) error {
	rec := b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	backend.Phase(ctx, "provider")
	inst, err := b.prov.Wait(ctx, provider.Settle(rec.Ref))
	switch {
	case ctx.Err() != nil:
		return errSuperseded
	case errors.Is(err, provider.ErrNotFound):
		b.logf("remote: adopted shed %s: its instance is gone; dropping the record", j.name)
		b.forget(j.name, j.id)
		return err
	case err != nil:
		b.update(j.name, j.id, func(r *state.Record) {
			r.Shed.StatusReason = config.StatusReasonUnreachable
		})
		return err
	}
	if inst.State == provider.StateStopped {
		b.update(j.name, j.id, func(r *state.Record) {
			r.Shed.Status = config.StatusStopped
			r.Shed.StatusReason = ""
			r.Job = ""
			r.Address = ""
		})
		return nil
	}
	b.noteInstance(j, inst)

	info, err := b.waitAgent(ctx, j)
	if err != nil {
		if ctx.Err() != nil {
			return errSuperseded
		}
		reason := config.StatusReasonUnreachable
		if errors.Is(err, transport.ErrNoHostKey) && rec.HostKeyPin == "" {
			reason = config.StatusReasonNoPin
		}
		b.logf("remote: adopted shed %s could not be probed (%s): %v", j.name, reason, err)
		b.closeLink(j.id)
		// Job stays JobProbe: a later start call, or the next server
		// start, probes again and never restarts the instance.
		b.update(j.name, j.id, func(r *state.Record) { r.Shed.StatusReason = reason })
		return err
	}
	if !guest.ProtocolCompatible(info.Protocol) || !guest.LayoutSupported(info.Layout) {
		// The instance runs an agent this server cannot drive. It is left
		// as it is, and the shed is not reported running.
		err := fmt.Errorf("the guest agent reports protocol %d and layout %d, which this server cannot drive", info.Protocol, info.Layout)
		b.logf("remote: adopted shed %s cannot be used: %v", j.name, err)
		b.closeLink(j.id)
		b.update(j.name, j.id, func(r *state.Record) { r.Shed.StatusReason = config.StatusReasonUnreachable })
		return err
	}
	rec = b.snapshot(j.name, j.id)
	if rec == nil {
		return errSuperseded
	}
	if agent, err := b.agentFor(rec); err == nil {
		b.credMgr.ResumeMessageChannel(j.name, agent, rec.Shed.Owner != "")
	}
	if !b.update(j.name, j.id, func(r *state.Record) {
		r.Shed.Status = config.StatusRunning
		r.Shed.StatusReason = ""
		r.Job = ""
		r.Layout = info.Layout
	}) {
		return errSuperseded
	}
	return nil
}
