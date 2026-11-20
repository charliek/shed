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
	"github.com/charliek/shed/internal/vmutil"
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
	// The firewall check's first run, so a result exists before the first
	// create or start. It only logs: a failing check never keeps the server
	// from starting, because existing sheds must stay listable and
	// deletable.
	b.CheckFirewall(ctx)
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

		// No record. A create that no one is finishing is destroyed: its
		// VM would run to its limit and its disk would be billed for
		// ever, on no list and under no limit.
		if b.abandonedCreate(inst) {
			m := &state.Marker{ID: id.ID, Name: id.Name, Env: id.Env, Ref: inst.Ref, At: b.now().UTC()}
			if err := b.saveMarkerLocked(m); err != nil {
				note("instance %s (shed %s) is an abandoned create but its marker could not be written: %v", inst.Name, id.Name, err)
				continue
			}
			note("instance %s (shed %s) has been %q since %s with no local record; destroying it", inst.Name, id.Name, inst.ShedState(), inst.CreatedAt.UTC().Format(time.RFC3339))
			deletes = append(deletes, m)
			continue
		}
		// Otherwise adopt it only if it was a finished shed.
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
		switch {
		case rec.UserConflict != "":
			note("adopted shed %s is labelled %s=%q, which is not a valid user name; only an admin can reach it until the label is corrected", id.Name, provider.LabelUser, id.User)
		case id.Owner != "" && id.User != "":
			note("adopted shed %s is labelled with owner %q and user %q; only an admin can reach it", id.Name, id.Owner, id.User)
		}
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
		if b.reconcileUserLocked(r, inst.Labels[provider.LabelUser], note) {
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
		b.joinBus(rec.Shed.Name, rec.Shed.ID, func(id vmutil.BusIdentity) {
			b.credMgr.ResumeMessageChannel(rec.Shed.Name, agent, id)
		})
	}
}

// abandonedCreate reports whether inst is a create nobody will finish: still
// labelled creating StaleCreateAge after the cloud made it. The caller has
// checked that it carries this environment's labels in a schema this server
// reads, and that no record, delete marker or delete job names its shed ID. A
// create this server is working on always has a record, so only a create
// whose record was lost (the state directory, or a server that died with the
// record unwritten) gets here. An instance whose age is unknown is left
// alone.
func (b *Backend) abandonedCreate(inst provider.Instance) bool {
	return inst.ShedState() == provider.ShedStateCreating &&
		!inst.CreatedAt.IsZero() && b.now().Sub(inst.CreatedAt) >= StaleCreateAge
}

// reconcileUserLocked brings a record's user into line with the shed_user
// label on its instance, and reports whether it changed the record (the
// caller saves it). The label is written at create, beside the record, so the
// two agree unless one was lost or changed:
//
//   - no user on the label (absent, or empty): the record stands;
//   - a label that is a valid label value but not a valid user name
//     (config.ValidUserName; no server writes one): a conflict, whatever the
//     record says. It is never stored or reported as a user;
//   - the record has neither a user nor an owner and the label has a user:
//     the record lost it (a build that predates users rewrote it, or a
//     rollback did) and takes the label's;
//   - the record names another user, or an owner, and the label a user: a
//     conflict.
//
// A conflict fails closed: the shed is reported with config.UserUnresolved,
// so only an admin can reach it, until a read of the instance finds the two
// in line again, which clears it. It is noted at every such read.
//
// It runs on every path that reads the shed's instance: the start-up pass,
// whether the instance was in the list or read through the record's stored
// reference (correct); the periodic pass, for every listed instance and every
// correction it reads; and the create, start, probe and stop jobs
// (noteInstance, resumeCommitted, reconcileUserFromLocked). A save that fails
// leaves the record in memory as decided, and the next read decides again
// from the label.
func (b *Backend) reconcileUserLocked(r *state.Record, label string, note func(string, ...any)) bool {
	before := busIdentity(r)
	defer func() {
		if after := busIdentity(r); after.User != before.User {
			b.busUserChangedLocked(r)
		}
	}()
	conflict := ""
	changed := false
	switch {
	case label == "":
	case !config.ValidUserName(label):
		conflict = label
		note("shed %s: its instance is labelled %s=%q, which is not a valid user name; only an admin can reach it until the label is corrected",
			r.Shed.Name, provider.LabelUser, label)
	case label == r.Shed.User && r.Shed.Owner == "":
	case r.Shed.User == "" && r.Shed.Owner == "":
		r.Shed.User = label
		changed = true
		note("shed %s had no user on record and its instance is labelled %s=%s; recording that user", r.Shed.Name, provider.LabelUser, label)
	default:
		conflict = label
		note("shed %s is recorded with user %q and owner %q and its instance is labelled %s=%q; only an admin can reach it until the two agree",
			r.Shed.Name, r.Shed.User, r.Shed.Owner, provider.LabelUser, label)
	}
	if conflict != r.UserConflict {
		if conflict == "" {
			note("shed %s: its record and its instance's %s label agree again", r.Shed.Name, provider.LabelUser)
		}
		r.UserConflict = conflict
		changed = true
	}
	return changed
}

// busSync is the application of a shed's reported user to its bus route,
// after a change of that user held the route (busUserChangedLocked).
type busSync struct {
	epoch   uint64 // the newest hold on the route, lifted once applied
	running bool   // a worker is applying it
	again   bool   // the reported user changed while the worker ran
}

// Test hooks (nil in production): busSyncHook runs in a bus-route worker
// just before it applies the user it read, busSyncDoneHook when the worker
// ends.
var (
	busSyncHook     func(name string)
	busSyncDoneHook func(name string)
)

// busUserChangedLocked is called, under b.mu, the moment a record's reported
// user changes (a label conflict that makes it unresolved, a heal, a
// conflict that clears; every such change goes through reconcileUserLocked).
// The shed's bus route is held at once, without waiting for anything: from
// this instant no request of the shed, queued ones included, reaches the
// previous user's machine and no answer of that machine begins to be written
// into the shed, and the shed's pending requests are dropped. A frame
// already being written finishes within its own bound. The new user is then
// applied out of line by a worker for this shed alone (it takes the
// credential manager's locks, which are never taken under b.mu), and the
// hold is lifted when it has been applied.
func (b *Backend) busUserChangedLocked(r *state.Record) {
	name := r.Shed.Name
	epoch := b.credMgr.SuspendShedRoute(name, r.Shed.CreatedAt)
	s := b.busSyncs[name]
	if s == nil {
		s = &busSync{}
		b.busSyncs[name] = s
	}
	s.epoch = epoch
	b.runBusSyncLocked(name, s)
}

// runBusSyncLocked starts s's worker, or, when it is running, has it go
// round again. The caller holds b.mu.
func (b *Backend) runBusSyncLocked(name string, s *busSync) {
	if s.running {
		s.again = true
		return
	}
	if b.closed {
		return
	}
	s.running = true
	b.wg.Add(1)
	go b.busSyncWorker(name, s)
}

// busSyncWorker applies the shed name's reported user to its message
// channel (CredentialManager.SetShedUser), reading the record afresh each
// round, and then lifts the route hold. Each shed has at most one
// worker. The hold is lifted only with the epoch read with the record (a
// change since makes a new epoch and another round), and only while no
// channel start is in progress (a start reads the record before it
// registers, so it could register the previous user; its end runs the
// worker again). Each round runs under busApplyMu, so it cannot interleave
// with applyBusRoute; the test hook runs before it is taken.
func (b *Backend) busSyncWorker(name string, s *busSync) {
	defer b.wg.Done()
	if busSyncDoneHook != nil {
		defer busSyncDoneHook(name)
	}
	for {
		if busSyncHook != nil {
			busSyncHook(name)
		}
		b.busApplyMu.Lock()
		b.mu.Lock()
		s.again = false
		epoch := s.epoch
		r := b.records[name]
		have := r != nil && !b.closed
		var id vmutil.BusIdentity
		if have {
			id = busIdentity(r)
		}
		b.mu.Unlock()

		if have && !id.Owned && b.credMgr.SetShedUser(name, id.CreatedAt, id.User) {
			b.logf("remote: shed %s now routes credential requests as user %q", name, id.User)
		}

		b.mu.Lock()
		if s.again && !b.closed {
			b.mu.Unlock()
			b.busApplyMu.Unlock()
			continue
		}
		s.running = false
		if b.busStarting[name] == 0 {
			b.credMgr.ResumeShedRoute(name, epoch)
			if b.busSyncs[name] == s {
				delete(b.busSyncs, name)
			}
		}
		b.mu.Unlock()
		b.busApplyMu.Unlock()
		return
	}
}

// applyBusRoute applies the shed name's reported user to its message channel
// at once, in the caller's goroutine, and lifts the route hold the change
// made, so the route is usable when it returns (AssignUser), whatever the
// shed's bus-route worker is doing. It is one round of that worker under
// busApplyMu: it reads the record and the newest hold's epoch, applies the
// user, and lifts the hold only if no change has come since (its epoch is
// still the newest) and no channel start is in progress; otherwise the
// worker, which a change always runs again, applies the newer user after it.
// It leaves the worker's own state (running, again) alone: a worker that
// runs later finds the user applied and the hold gone, and only tidies up.
// The caller holds no lock.
func (b *Backend) applyBusRoute(name string) {
	b.busApplyMu.Lock()
	defer b.busApplyMu.Unlock()
	b.mu.Lock()
	s := b.busSyncs[name]
	r := b.records[name]
	if s == nil || r == nil || b.closed {
		b.mu.Unlock()
		return
	}
	epoch := s.epoch
	id := busIdentity(r)
	b.mu.Unlock()

	if !id.Owned && b.credMgr.SetShedUser(name, id.CreatedAt, id.User) {
		b.logf("remote: shed %s now routes credential requests as user %q", name, id.User)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busSyncs[name] == s && s.epoch == epoch && b.busStarting[name] == 0 && !b.closed {
		b.credMgr.ResumeShedRoute(name, epoch)
	}
}

// joinBus opens (or resumes) the shed incarnation (name, shedID)'s message
// channel with start, passing the bus identity its record reports now. It
// counts the start as in progress, so a route held meanwhile is not lifted
// before the channel is registered, and then runs the shed's pending
// bus-route worker, if any, so the channel ends up with the newest user. A
// shed whose record is gone, or is another incarnation now, is left alone.
func (b *Backend) joinBus(name, shedID string, start func(vmutil.BusIdentity)) {
	b.mu.Lock()
	r := b.currentLocked(name, shedID)
	if r == nil {
		b.mu.Unlock()
		return
	}
	id := busIdentity(r)
	b.busStarting[name]++
	b.mu.Unlock()

	start(id)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busStarting[name]--; b.busStarting[name] <= 0 {
		delete(b.busStarting, name)
	}
	if s := b.busSyncs[name]; s != nil {
		b.runBusSyncLocked(name, s)
	}
}

// reconcileUserFromLocked is reconcileUserLocked from an instance a job or the
// periodic pass read, for a caller that holds b.mu and saves the record when
// it reports a change. An instance that does not carry the record's shed ID
// in this environment, or is labelled by another version of the label set,
// decides nothing.
func (b *Backend) reconcileUserFromLocked(r *state.Record, inst provider.Instance) bool {
	if inst.Labels[provider.LabelID] != r.Shed.ID || inst.Labels[provider.LabelEnv] != b.cfg.Environment || schemaOf(inst) != nil {
		return false
	}
	return b.reconcileUserLocked(r, inst.Labels[provider.LabelUser], b.logNote)
}

// reconcileUserRead is reconcileUserFromLocked under b.mu, for the shed's
// current record (the incarnation id), saving it when it changed.
func (b *Backend) reconcileUserRead(name, id string, inst provider.Instance) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r := b.currentLocked(name, id); r != nil && b.reconcileUserFromLocked(r, inst) {
		b.saveLocked(r)
	}
}

// logNote is the note reconcileUserLocked writes to outside the start-up
// pass: the log alone.
func (b *Backend) logNote(format string, args ...any) {
	b.logf("remote: "+format, args...)
}

// adoptedRecord rebuilds a shed's record from its instance: name, ID, owner
// and user from labels, the shed-spec metadata, and the cloud's own facts. A
// label set that names both an owner and a user is adopted as it is, and
// reported as config.ShedOwnership says (no one but an admin reaches it). A
// shed_user label that is not a valid user name is not taken as the user: it
// is adopted as a conflict (Record.UserConflict), reported unresolved.
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
			Arch:           inst.Arch,
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
	if config.ValidUserName(id.User) {
		rec.Shed.User = id.User
	} else {
		rec.UserConflict = id.User // "" for no user
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
			b.reconcileUserFromLocked(r, inst)
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
		b.joinBus(j.name, j.id, func(id vmutil.BusIdentity) {
			b.credMgr.ResumeMessageChannel(j.name, agent, id)
		})
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
