package remote

import (
	"context"
	"errors"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
)

// runLimitSlack is how close to its run limit an instance must have stopped
// for the stop to be put down to the limit.
const runLimitSlack = 2 * time.Minute

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

// sweepCheck is a shed whose record and the cloud's list disagree.
type sweepCheck struct {
	name, id string
	ref      provider.Ref
	status   string
	listed   bool
}

// Sweep is the periodic pass. It runs the firewall check, lists this
// environment's instances once and then, for sheds with no job in flight:
//
//   - starts the delete job of every delete marker that has none (a destroy
//     that failed is retried here, not only at the next server start);
//   - writes a delete marker for an instance still labelled creating
//     StaleCreateAge after the cloud made it, with no record: a create whose
//     record was lost, which nothing else would ever remove;
//   - records a "running" shed whose instance stopped (the run limit, or a
//     stop from outside) as "stopped", with the reason;
//   - drops the record of a shed whose instance no longer exists;
//   - records a "stopped" shed whose instance runs as "starting" and probes
//     it, so a VM that costs money is never shown as stopped;
//   - gives a shed left "stopping" by a failed cloud call a new stop job;
//   - runs again the create or start of a shed kept "starting" because the
//     cloud could not be read when its job looked (Backend.keep).
//
// It destroys nothing itself: only a delete job does, and a delete job exists
// only for a delete marker. Each correction is confirmed with a fresh read of
// the one instance and applied only if the shed is still as it was, so a
// request that arrives during the pass wins.
func (b *Backend) Sweep(ctx context.Context) error {
	if err := b.checkReady(); err != nil {
		return err
	}
	// Refresh the firewall check. Its result gates creates and starts only;
	// nothing below depends on it, and nothing is stopped or deleted for it.
	b.CheckFirewall(ctx)
	insts, err := b.prov.List(ctx, provider.Selector{Env: b.cfg.Environment})
	if err != nil {
		return err
	}
	byID := make(map[string]provider.Instance, len(insts))
	for _, inst := range insts {
		if inst.Labels[provider.LabelEnv] != b.cfg.Environment {
			continue
		}
		if id := inst.Labels[provider.LabelID]; id != "" {
			byID[id] = inst
		}
	}

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
	carriers := make(map[string]int, len(insts))
	for _, inst := range insts {
		carriers[inst.Labels[provider.LabelID]]++
	}
	for _, inst := range insts {
		id, err := inst.Identity()
		if err != nil || id.Env != b.cfg.Environment || carriers[id.ID] != 1 {
			continue
		}
		if recorded[id.ID] || marked[id.ID] || b.deletes[id.ID] != nil || !b.abandonedCreate(inst) {
			continue
		}
		m := &state.Marker{ID: id.ID, Name: id.Name, Env: id.Env, Ref: inst.Ref, At: now.UTC()}
		if err := b.saveMarkerLocked(m); err != nil {
			b.logf("remote: periodic pass: instance %s (shed %s) is an abandoned create but its marker could not be written: %v", inst.Name, id.Name, err)
			continue
		}
		b.logf("remote: periodic pass: instance %s (shed %s) has been %q since %s with no local record; destroying it",
			inst.Name, id.Name, inst.ShedState(), inst.CreatedAt.UTC().Format(time.RFC3339))
		abandoned = append(abandoned, m)
	}
	var checks []sweepCheck
	var launch []*job
	for name, r := range b.records {
		if b.jobs[name] != nil {
			continue
		}
		inst, listed := byID[r.Shed.ID]
		switch r.Shed.Status {
		case config.StatusStarting:
			// A create or start with no job is one that was kept because
			// the cloud could not be read (Backend.keep). It is run again
			// here: one read of its instance, and on from what that shows.
			// A probe is not: a start call or the next server start
			// retries it.
			if r.Job == state.JobCreate && !r.Ready {
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
				checks = append(checks, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status, listed: listed})
			}
		case config.StatusStopped:
			if r.Ref != "" && (!listed || inst.State == provider.StateRunning || inst.State == provider.StateStarting) {
				checks = append(checks, sweepCheck{name: name, id: r.Shed.ID, ref: r.Ref, status: r.Shed.Status, listed: listed})
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

// correct re-reads one instance and brings its shed's record into line.
func (b *Backend) correct(ctx context.Context, c sweepCheck) {
	inst, err := b.prov.Get(ctx, c.ref)
	gone := errors.Is(err, provider.ErrNotFound)
	if err != nil && !gone {
		b.logf("remote: reading the instance of shed %s (its record is kept): %v", c.name, err)
		return
	}
	if !gone && inst.Labels[provider.LabelID] != c.id {
		b.logf("remote: periodic pass: the instance at %s no longer carries shed id %s; leaving shed %s alone", c.ref, c.id, c.name)
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
	switch {
	case gone && !c.listed:
		b.logf("remote: shed %s (%s) has no instance in the cloud any more; dropping its record", c.name, c.id)
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
		r.Address = inst.Address
		r.AddressAt = b.now()
		b.saveLocked(r)
		launch = b.newJobLocked(state.JobProbe, c.name, c.id)
	}
	b.mu.Unlock()

	if dropped || stopped {
		b.credMgr.StopListener(c.name)
		b.closeLink(c.id)
	}
	if launch != nil {
		b.launch(launch)
	}
}

// recordStoppedLocked records a shed whose instance stopped without this
// server asking: by its run limit when it stopped at about that age,
// otherwise by something outside.
func (b *Backend) recordStoppedLocked(r *state.Record, inst provider.Instance) {
	reason := config.StatusReasonExternal
	if r.MaxRun > 0 && !inst.StartedAt.IsZero() {
		stoppedAt := inst.StoppedAt
		if stoppedAt.IsZero() || stoppedAt.Before(inst.StartedAt) {
			stoppedAt = b.now()
		}
		if stoppedAt.Sub(inst.StartedAt) >= r.MaxRun-runLimitSlack {
			reason = config.StatusReasonRunLimit
		}
	}
	r.Shed.Status = config.StatusStopped
	r.Shed.StatusReason = reason
	r.Job = ""
	r.Queued = ""
	r.Pending = nil
	r.Address = ""
	b.saveLocked(r)
}
