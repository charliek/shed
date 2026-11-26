package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/vmutil"
)

// listRetry is the default pause between attempts at the first list.
const listRetry = 5 * time.Second

// Reconcile is the start-up pass. It reads the provider's catalog and lists
// this server's machines once (retrying both every listRetry until they
// answer or ctx ends), repairs the local cache against them, resumes the
// jobs a previous server left in flight, and then closes Ready. The server
// runs it once, in the background. Until Ready the backend is degraded:
// reads answer from the records, and calls that need the service are
// refused.
//
// What the pass does:
//   - a "ready" machine with no record is adopted: its record is rebuilt
//     from the machine (its owner, size, image, spec and labels). A running
//     one stays "starting" until its agent answers a probe;
//   - a record that was "starting" or "stopping" gets a new job that carries
//     on from what the provider says now;
//   - a machine with a local delete marker, or labelled shed_state=deleting,
//     is destroyed. Nothing else is ever destroyed but an abandoned create;
//   - a record whose machine is not in the list is dropped once a read of
//     that one machine confirms it is gone (and logged with the cause when
//     the list names its deletion), unless the record is in "error" or its
//     create had not been accepted yet;
//   - anything else is logged and left alone.
func (b *Backend) Reconcile(ctx context.Context) {
	var listing provider.Listing
	for {
		err := b.refreshCatalog(ctx)
		if err == nil {
			listing, err = b.prov.List(ctx)
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		b.logf("remote: reconcile: reading the account service's machines failed; retrying in %s: %v", b.opts.ListRetry, err)
		select {
		case <-time.After(b.opts.ListRetry):
		case <-ctx.Done():
			return
		case <-b.rootCtx.Done():
			return
		}
	}
	if cat, _ := b.Catalog(); b.fw.mode == config.RemoteFirewallAuto && !cat.IngressWritable {
		err := fmt.Errorf("remote: remote.firewall.mode: %s writes the machines' ingress, and the account service says its ingress is managed elsewhere (not writable); use remote.firewall.mode: %s with the sources it applies",
			config.RemoteFirewallAuto, config.RemoteFirewallStatic)
		b.logf("%v", err)
		b.fail(err)
		return
	}
	// The firewall check's first run, so a result exists before the first
	// create or start, and before any job is resumed. A failing check never
	// keeps the server from starting, because existing sheds must stay
	// listable and deletable, with one exception: auto mode against a
	// service that refuses to write the ingress (INGRESS_NOT_WRITABLE) is a
	// configuration that cannot work, and stops the start as the catalog's
	// word does above.
	b.CheckFirewall(ctx)
	if st := b.FirewallStatus(); b.fw.mode == config.RemoteFirewallAuto && st.Failure != nil && st.Failure.Reason == FirewallNotWritable {
		err := fmt.Errorf("remote: remote.firewall.mode: %s writes the machines' ingress, and the account service refuses to (%s); use remote.firewall.mode: %s with the sources it applies",
			config.RemoteFirewallAuto, st.Failure.What, config.RemoteFirewallStatic)
		b.logf("%v", err)
		b.fail(err)
		return
	}
	b.reconcile(ctx, listing)
	b.readyOnce.Do(func() { close(b.ready) })
	b.startSweeper()
}

func (b *Backend) reconcile(ctx context.Context, listing provider.Listing) {
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
	deleted := deletionCauses(listing.Deleted)

	byID := make(map[string]provider.Instance, len(listing.Instances))
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
				r.Job, r.Queued, r.Pending, r.Mutation = "", "", nil, nil
				b.saveLocked(r)
			}
			continue
		}
		b.dropLocked(name)
		delete(recordIDs, id)
	}

	for _, inst := range listing.Instances {
		id := string(inst.Ref)
		byID[id] = inst

		if m := marked[id]; m != nil {
			if m.Ref == "" {
				m.Ref = inst.Ref
			}
			continue // destroyed below, with every other marker
		}
		if inst.ShedState() == provider.ShedStateDeleting {
			m := &state.Marker{ID: id, Name: inst.Name, Ref: inst.Ref, At: b.now().UTC()}
			if err := b.saveMarkerLocked(m); err != nil {
				note("machine %s is labelled deleting but its marker could not be written: %v", id, err)
				continue
			}
			if name, ok := recordIDs[id]; ok {
				b.dropLocked(name)
				delete(recordIDs, id)
			}
			deletes = append(deletes, m)
			continue
		}
		if _, known := recordIDs[id]; known {
			continue
		}

		// No record. A create that no one is finishing is destroyed: its
		// machine would run to its lease and hold a place under the
		// service's limits, on no list.
		if b.abandonedCreate(inst) {
			m := &state.Marker{ID: id, Name: inst.Name, Ref: inst.Ref, At: b.now().UTC()}
			if err := b.saveMarkerLocked(m); err != nil {
				note("machine %s (shed %s) is an abandoned create but its marker could not be written: %v", id, inst.Name, err)
				continue
			}
			note("machine %s (shed %s) has been %q since %s with no local record; destroying it", id, inst.Name, inst.ShedState(), inst.CreatedAt.UTC().Format(time.RFC3339))
			deletes = append(deletes, m)
			continue
		}
		// Otherwise adopt it only if it was a finished shed.
		if inst.ShedState() != provider.ShedStateReady {
			note("machine %s (shed %s) is %q with no local record; leaving it alone", id, inst.Name, inst.ShedState())
			continue
		}
		if inst.State == provider.StateDeleting {
			note("machine %s (shed %s) is being deleted by the account service; not adopting it", id, inst.Name)
			continue
		}
		if other := b.records[inst.Name]; other != nil {
			note("machine %s claims shed name %s, which shed %s already has; leaving it alone", id, inst.Name, other.Shed.ID)
			continue
		}
		rec, err := b.adoptedRecord(inst)
		if err != nil {
			note("machine %s (shed %s) cannot be adopted: %v", id, inst.Name, err)
			continue
		}
		if err := b.store.SaveRecord(rec); err != nil {
			note("machine %s (shed %s) could not be recorded: %v", id, inst.Name, err)
			continue
		}
		b.records[inst.Name] = rec
		recordIDs[id] = inst.Name
		b.logf("remote: reconcile: adopted shed %s (%s) as %s", inst.Name, id, rec.Shed.Status)
		switch {
		case rec.UserConflict != "":
			note("adopted shed %s belongs to %q, which is not a valid user name; only an admin can reach it", inst.Name, rec.UserConflict)
		case inst.Client != "" && inst.User != "":
			note("adopted shed %s belongs to client %q and user %q; only an admin can reach it", inst.Name, inst.Client, inst.User)
		}
		if rec.Job != "" {
			launch = append(launch, b.newJobLocked(rec.Job, inst.Name, id))
		}
	}

	for name, r := range b.records {
		if b.jobs[name] != nil {
			continue // adopted just now
		}
		inst, present := byID[r.Shed.ID]
		if !present {
			cause := deleted[r.Shed.ID]
			switch {
			case r.Shed.Status == config.StatusError:
				// An error record outlives its machine.
			case cause != "" && r.Ref != "":
				// The service deleted it: dropped once a read confirms.
				confirm = append(confirm, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status, cause: cause})
			case r.Job == state.JobCreate && !r.Ready:
				// The create had not finished; it is sent again, and
				// Create is idempotent.
				launch = append(launch, b.newJobLocked(state.JobCreate, name, r.Shed.ID))
			case r.Ref != "":
				// Not in the list. A record is dropped only once a read
				// of that one machine says it is gone.
				confirm = append(confirm, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status})
			default:
				note("shed %s (%s) never had a machine; dropping its record", name, r.Shed.ID)
				b.dropLocked(name)
			}
			continue
		}
		if r.Ref == "" {
			r.Ref = inst.Ref
		}
		b.noteMachineLocked(r, inst)
		b.reconcileUserLocked(r, inst, note)
		b.saveLocked(r)
		if r.Job == state.JobCreate && !r.Ready && inst.ShedState() == provider.ShedStateReady {
			// The create's last provider write landed and its last local
			// one did not. It is a finished shed: resuming the create could
			// only end in the clean-up of a failed one, which destroys the
			// machine and its disk.
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
				note("shed %s was running and the provider reports it stopped; recording it stopped (%s)", name, r.Shed.StatusReason)
			case provider.StateStopping:
				r.Shed.Status = config.StatusStopping
				r.Job = state.JobStop
				b.saveLocked(r)
				launch = append(launch, b.newJobLocked(state.JobStop, name, r.Shed.ID))
			default:
				note("shed %s is recorded running and the provider reports %s; leaving it alone", name, inst.State)
			}
		case config.StatusStopped:
			if inst.State != provider.StateStopped {
				note("shed %s is recorded stopped and the provider reports %s; leaving it alone", name, inst.State)
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

// deletionCauses maps each recently deleted machine's shed id to why it was
// deleted.
func deletionCauses(dels []provider.Deletion) map[string]string {
	out := make(map[string]string, len(dels))
	for _, d := range dels {
		out[string(d.Ref)] = d.Cause
	}
	return out
}

// abandonedCreate reports whether inst is a create nobody will finish: still
// labelled creating StaleCreateAge after the provider made it. The caller
// has checked that no record, delete marker or delete job names its shed
// ID. A create this server is working on always has a record, so only a
// create whose record was lost (the state directory, or a server that died
// with the record unwritten) gets here. A machine whose age is unknown is
// left alone.
func (b *Backend) abandonedCreate(inst provider.Instance) bool {
	return inst.ShedState() == provider.ShedStateCreating &&
		!inst.CreatedAt.IsZero() && b.now().Sub(inst.CreatedAt) >= StaleCreateAge
}

// reconcileUserLocked brings a record's owner into line with its machine's,
// and reports whether it changed the record (the caller saves it). The
// machine's user and client are fixed at create and are the truth about who
// owns it, so the two agree unless the record was lost or changed:
//
//   - the machine names no owner: the record stands;
//   - the machine names a user that is not a valid user name
//     (config.ValidUserName; no server asks for one): a conflict, whatever
//     the record says. It is never stored or reported as a user;
//   - the record has neither a user nor an owner and the machine a user:
//     the record lost it and takes the machine's, with the origin its spec
//     recorded;
//   - the record names another user or client than the machine: a
//     conflict.
//
// A conflict fails closed: the shed is reported with config.UserUnresolved,
// so only an admin can reach it, until a read of the machine finds the two
// in line again, which clears it. It is noted at every such read.
//
// It runs on every path that reads the shed's machine: the start-up pass,
// the periodic pass and its corrections, and the create, start, probe and
// stop jobs.
func (b *Backend) reconcileUserLocked(r *state.Record, inst provider.Instance, note func(string, ...any)) bool {
	before := busIdentity(r)
	defer func() {
		if after := busIdentity(r); after.User != before.User {
			b.busUserChangedLocked(r)
		}
	}()
	conflict := ""
	changed := false
	switch {
	case inst.Client != "" && inst.User != "":
		// No service answers so (a machine has one owner); fail closed.
		conflict = inst.User
		note("shed %s: its machine belongs to client %q and user %q; only an admin can reach it", r.Shed.Name, inst.Client, inst.User)
	case inst.Client != "":
		if inst.Client != r.Shed.Owner || r.Shed.User != "" {
			conflict = "client " + inst.Client
			note("shed %s is recorded with user %q and owner %q and its machine belongs to client %q; only an admin can reach it until the two agree",
				r.Shed.Name, r.Shed.User, r.Shed.Owner, inst.Client)
		}
	case inst.User == "":
	case !config.ValidUserName(inst.User):
		conflict = inst.User
		note("shed %s: its machine belongs to %q, which is not a valid user name; only an admin can reach it", r.Shed.Name, inst.User)
	case inst.User == r.Shed.User && r.Shed.Owner == "":
	case r.Shed.User == "" && r.Shed.Owner == "":
		r.Shed.User = inst.User
		// Its origin only from the machine's spec (what the create
		// recorded); with none it is not known, and the shed gets no
		// delivery: never inferred from the configuration of the day.
		r.UserOrigin = originEvidence(specOrigin(inst))
		changed = true
		note("shed %s had no user on record and its machine belongs to %s; recording that user", r.Shed.Name, inst.User)
	default:
		conflict = inst.User
		note("shed %s is recorded with user %q and owner %q and its machine belongs to %q; only an admin can reach it until the two agree",
			r.Shed.Name, r.Shed.User, r.Shed.Owner, inst.User)
	}
	if conflict != r.UserConflict {
		if conflict == "" {
			note("shed %s: its record and its machine agree on its owner again", r.Shed.Name)
		}
		r.UserConflict = conflict
		changed = true
	}
	return changed
}

// specOrigin is the user origin a machine's spec recorded, or "".
func specOrigin(inst provider.Instance) string {
	if s, err := inst.ShedSpec(); err == nil {
		return s.UserOrigin
	}
	return ""
}

// originEvidence is the recorded origin a machine's spec gives: the spec's
// when it is one, otherwise unknown (never empty, which is a v0.11 record's,
// nor inferred from the configuration).
func originEvidence(origin string) string {
	if origin == delivery.OriginLocal || origin == delivery.OriginDirectory {
		return origin
	}
	return delivery.OriginUnknown
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
// worker again). Each round runs under busApplyMu; the test hook runs before
// it is taken.
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

// reconcileUserFromLocked is reconcileUserLocked from a machine a job or the
// periodic pass read, for a caller that holds b.mu and saves the record when
// it reports a change. A machine that is not the record's decides nothing.
func (b *Backend) reconcileUserFromLocked(r *state.Record, inst provider.Instance) bool {
	if string(inst.Ref) != r.Shed.ID {
		return false
	}
	return b.reconcileUserLocked(r, inst, b.logNote)
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

// adoptedRecord rebuilds a shed's record from its machine: name, ID and
// owner from the machine's members, the rest from its spec and its size and
// image. Its host key is pinned at its first contact (Learn). A user that is not
// a valid user name is not taken as the user: it is adopted as a conflict
// (Record.UserConflict), reported unresolved.
func (b *Backend) adoptedRecord(inst provider.Instance) (*state.Record, error) {
	spec, err := inst.ShedSpec()
	if err != nil {
		return nil, err
	}
	rec := &state.Record{
		Shed: config.Shed{
			Name:           inst.Name,
			ID:             string(inst.Ref),
			CreatedAt:      spec.CreatedAt.UTC(),
			Repo:           spec.Repo,
			Backend:        config.BackendRemote,
			CPUs:           inst.Size.CPUs,
			MemoryMB:       inst.Size.MemoryMB,
			LandingDir:     spec.LandingDir,
			Image:          inst.ImageResolved,
			Owner:          inst.Client,
			NetworkProfile: spec.Network,
			Arch:           inst.Size.Arch,
		},
		// Where its user came from: only from the machine's spec, the
		// evidence its create wrote. None is recorded as unknown (the shed
		// gets no delivery, and the start-up migration never fills it),
		// never the configuration of the day.
		UserOrigin: originEvidence(spec.UserOrigin),
		Ref:        inst.Ref,
		Ready:      true,
		// Its history is not known: nothing is marked done that the
		// server did not see. Every step skipped, delivery none.
		Delivery: delivery.AdoptedRecord(),
		Layout:   inst.Layout(),
	}
	// No pin from the attestation alone: the first contact (a probe of a
	// running machine, the first start of a stopped one) compares the key
	// sshd presents with the attested one and pins it then (Learn).
	b.noteMachineLocked(rec, inst)
	if config.ValidUserName(inst.User) {
		rec.Shed.User = inst.User
	} else {
		rec.UserConflict = inst.User // "" for no user
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
	case errors.Is(err, provider.ErrNotFound) && b.goneFor(j, rec.Ref, "probe"):
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
			b.noteMachineLocked(r, inst)
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
		// The reasons a create or start gives: access (no way in this
		// server can use, or a host it may not dial) and host_key (a key
		// other than the pinned or attested one) are kept as such.
		reason := linkReason(err, config.StatusReasonUnreachable)
		if reason == config.StatusReasonUnreachable && errors.Is(err, transport.ErrNoHostKey) && rec.HostKeyPin == "" {
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
