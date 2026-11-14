package remote

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
)

// settledPoll is how often WaitSettled re-reads a record that is in a
// transient status with no job to follow.
const settledPoll = 250 * time.Millisecond

var (
	repoShorthand = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*/[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	repoGitHubSCP = regexp.MustCompile(`^git@github\.com:([a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+?)(\.git)?$`)
)

// httpsRepoURL turns the owner/repo shorthand, and the git@github.com form
// the API expands it to, into an HTTPS URL. A cloud shed holds no SSH key and
// no forwarded agent, so an SSH clone could only fail there. Other URLs pass
// through.
func httpsRepoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if repoShorthand.MatchString(repo) {
		return "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
	}
	if m := repoGitHubSCP.FindStringSubmatch(repo); m != nil {
		return "https://github.com/" + m[1] + ".git"
	}
	return repo
}

// asksForEgress reports whether a create's egress selection asks for any
// profile. Nothing and exactly ["off"] ask for none, as the API's own gate
// reads them (internal/api: asksForEgress), so a client that spells out "no
// egress profile" is not refused for a feature it did not ask for.
func asksForEgress(egress []string) bool {
	return len(egress) > 1 || (len(egress) == 1 && egress[0] != "off")
}

// CreateShed saves the shed as "starting", starts its create job and
// returns. It waits up to the fast-fail window first: a quota or capacity
// refusal that arrives inside it is returned here and leaves no record.
func (b *Backend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}
	switch {
	case req.LocalDir != "":
		return nil, fmt.Errorf("%w: local_dir on the remote backend", config.ErrFeatureUnsupportedSentinel)
	case len(req.AddDirs) > 0:
		return nil, fmt.Errorf("%w: add_dirs on the remote backend", config.ErrFeatureUnsupportedSentinel)
	case req.Mounts != nil && len(*req.Mounts) > 0:
		return nil, fmt.Errorf("%w: mounts on the remote backend", config.ErrFeatureUnsupportedSentinel)
	case req.FromSnapshot != "":
		return nil, fmt.Errorf("%w: from_snapshot on the remote backend", config.ErrFeatureUnsupportedSentinel)
	case asksForEgress(req.Egress):
		return nil, fmt.Errorf("%w: egress profiles on the remote backend", config.ErrFeatureUnsupportedSentinel)
	}
	network := req.Network
	if network == "" {
		network = config.NetworkProfileOpen
	}
	if !b.SupportsNetworkProfile(network) {
		return nil, fmt.Errorf("%w: %q", config.ErrNetworkProfileUnsupportedSentinel, network)
	}

	size, err := b.cfg.ResolveSize(req.CPUs, req.MemoryMB)
	if err != nil {
		return nil, err
	}
	diskGB, err := b.cfg.ResolveDiskGB(req.UpperSizeBytes)
	if err != nil {
		return nil, err
	}
	image, err := b.cfg.ResolveImage(req.Image)
	if err != nil {
		return nil, err
	}
	// A name that is taken is answered as such whatever the firewall check
	// says: a client reads that 409 as "the shed exists".
	b.mu.Lock()
	_, exists := b.records[req.Name]
	b.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}
	// No VM is created while the firewall check fails.
	if err := b.firewallGate(ctx); err != nil {
		return nil, err
	}
	if resolver, ok := b.prov.(provider.ImageResolver); ok {
		// Bounded: this runs before the fast-fail window, and the two
		// together must fit in the time a client waits for a create.
		rctx, cancel := context.WithTimeout(ctx, b.opts.ImageResolveTimeout)
		exact, err := resolver.ResolveImage(rctx, image)
		cancel()
		if err != nil {
			if errors.Is(err, provider.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s: %w", config.ErrUnknownImageSentinel, image, err)
			}
			return nil, fmt.Errorf("resolving image %s: %w", image, err)
		}
		image = exact
	}

	id := req.ID
	if id == "" {
		if id, err = config.NewShedID(); err != nil {
			return nil, err
		}
	}
	repoURL := httpsRepoURL(req.Repo)
	// Everything that is stored, labelled or reported is derived from the
	// URL with its user and password removed; only the clone sees repoURL.
	storedRepo := provider.StripRepoUserinfo(repoURL)
	landingDir := ""
	if repoURL != "" {
		dir, err := config.RepoDirName(repoURL)
		if err != nil || strings.Contains(dir, "@") {
			// The name git would clone into is not a usable one (a URL
			// with no path yields its authority, credentials included).
			return nil, fmt.Errorf("%w: cannot derive a directory name from repository URL %q", config.ErrInvalidShedRequestSentinel, storedRepo)
		}
		landingDir = config.HomePath + "/" + dir
	}
	createdAt := b.now().UTC().Round(0)
	maxRun := b.runLimit(req.Owner)

	spec, err := provider.NewSpec(
		provider.Identity{Env: b.cfg.Environment, ID: id, Name: req.Name, Owner: req.Owner},
		provider.ShedSpec{CreatedAt: createdAt, Repo: storedRepo, Network: network, LandingDir: landingDir},
		b.opts.AuthorizedKeys, size, diskGB, image, maxRun,
	)
	if err != nil {
		return nil, mapRefusal(err)
	}

	rec := &state.Record{
		Shed: config.Shed{
			Name:           req.Name,
			ID:             id,
			Status:         config.StatusStarting,
			CreatedAt:      createdAt,
			Repo:           storedRepo,
			Backend:        config.BackendRemote,
			CPUs:           size.CPUs,
			MemoryMB:       size.MemoryMB,
			LandingDir:     landingDir,
			Image:          image,
			Owner:          req.Owner,
			NetworkProfile: network,
		},
		Env:         b.cfg.Environment,
		Spec:        &spec,
		Job:         state.JobCreate,
		NoProvision: req.NoProvision,
		Size:        size.Name,
		DiskGB:      diskGB,
		MaxRun:      maxRun,
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("remote backend is shutting down")
	}
	if _, exists := b.records[req.Name]; exists {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}
	if err := b.checkCreateLocked(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	// A record that is in place is one the next server would resume, so the
	// create goes on with it (inPlace); any other error left no record.
	if err := b.inPlace(b.store.SaveRecord(rec)); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	b.records[req.Name] = rec
	j := b.newJobLocked(state.JobCreate, req.Name, id)
	j.repoURL = repoURL
	j.fastWindow = true
	first := b.shedOf(rec)
	b.mu.Unlock()
	b.launch(j)

	window := time.NewTimer(b.opts.FastFailWindow)
	defer window.Stop()
	select {
	case <-j.accepted:
	case <-j.done:
	case <-window.C:
	case <-ctx.Done():
	}

	b.mu.Lock()
	j.fastWindow = false
	fastErr := j.fastErr
	cur := b.currentLocked(req.Name, id)
	var shed *config.Shed
	if cur != nil {
		shed = b.shedOf(cur)
	}
	b.mu.Unlock()
	if fastErr != nil {
		return nil, mapRefusal(fastErr)
	}
	if shed == nil {
		// Deleted while the create was being answered.
		return first, nil
	}
	return shed, nil
}

// GetShed returns a shed from the local record cache.
func (b *Backend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		return nil, err
	}
	return b.shedOf(r), nil
}

// ListSheds returns every shed in the local record cache, ordered by name.
func (b *Backend) ListSheds(context.Context) ([]config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]config.Shed, 0, len(b.records))
	for _, r := range b.records {
		out = append(out, *b.shedOf(r))
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out, nil
}

// StartShed starts a stopped shed in the background.
//
//	starting  no-op (and withdraws a queued stop)
//	running   already running
//	stopping  queued: the start runs when the stop has finished
//	stopped   "starting"
//	error     refused with the reason
//
// A call that would start a VM (the shed is stopped, or stopping so the start
// is queued) is refused while the firewall check fails.
func (b *Backend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	gated := false
relock:
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if !gated && (r.Shed.Status == config.StatusStopped || r.Shed.Status == config.StatusStopping) {
		// The gate may read the cloud, so it runs without the lock; the
		// shed is then looked at afresh.
		b.mu.Unlock()
		if err := b.firewallGate(ctx); err != nil {
			return nil, err
		}
		gated = true
		goto relock
	}
	var launch *job
	switch r.Shed.Status {
	case config.StatusStarting:
		if r.Queued == state.QueuedStop {
			r.Queued = ""
			b.saveLocked(r)
		}
		launch = b.healLocked(r)
	case config.StatusRunning:
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyRunningSentinel, name)
	case config.StatusStopping:
		r.Queued = state.QueuedStart
		b.saveLocked(r)
		launch = b.healLocked(r)
	case config.StatusStopped:
		if b.closed {
			b.mu.Unlock()
			return nil, errors.New("remote backend is shutting down")
		}
		if err := b.checkStartLocked(); err != nil {
			r.Shed.StatusReason = config.StatusReasonLimit
			b.saveLocked(r)
			b.mu.Unlock()
			return nil, err
		}
		r.Shed.Status = config.StatusStarting
		r.Shed.StatusReason = ""
		r.Job = state.JobStart
		r.Queued = ""
		b.saveLocked(r)
		launch = b.newJobLocked(state.JobStart, name, r.Shed.ID)
	default:
		reason := r.Shed.StatusReason
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: shed %s is in status %q (%s); delete it", config.ErrShedNotStoppedSentinel, name, config.StatusError, reason)
	}
	shed := b.shedOf(r)
	b.mu.Unlock()
	if launch != nil {
		b.launch(launch)
	}
	return shed, nil
}

// StopShed stops a running shed in the background.
//
//	starting  queued: the stop runs when the shed is up
//	running   "stopping"
//	stopping  no-op (and withdraws a queued start)
//	stopped   refused
//	error     refused
func (b *Backend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	var launch *job
	switch r.Shed.Status {
	case config.StatusStarting:
		r.Queued = state.QueuedStop
		b.saveLocked(r)
		launch = b.healLocked(r)
	case config.StatusRunning:
		if b.closed {
			b.mu.Unlock()
			return nil, errors.New("remote backend is shutting down")
		}
		r.Shed.Status = config.StatusStopping
		r.Shed.StatusReason = ""
		r.Job = state.JobStop
		r.Queued = ""
		b.saveLocked(r)
		launch = b.newJobLocked(state.JobStop, name, r.Shed.ID)
	case config.StatusStopping:
		if r.Queued == state.QueuedStart {
			r.Queued = ""
			b.saveLocked(r)
		}
		launch = b.healLocked(r)
	default:
		status := r.Shed.Status
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is %s", config.ErrShedNotRunningSentinel, name, status)
	}
	shed := b.shedOf(r)
	b.mu.Unlock()
	if launch != nil {
		b.launch(launch)
	}
	return shed, nil
}

// healLocked returns a new job for a record that is in a transient status
// with nothing working on it, which a failed cloud call can leave behind. It
// returns nil when a job is in flight.
func (b *Backend) healLocked(r *state.Record) *job {
	if b.closed || b.jobs[r.Shed.Name] != nil {
		return nil
	}
	kind := r.Job
	if kind == "" {
		switch r.Shed.Status {
		case config.StatusStarting:
			kind = state.JobStart
		case config.StatusStopping:
			kind = state.JobStop
		default:
			return nil
		}
		r.Job = kind
		b.saveLocked(r)
	}
	return b.newJobLocked(kind, r.Shed.Name, r.Shed.ID)
}

// DeleteShed hides the shed at once and destroys its VM in the background.
// It succeeds in every status. Before it returns it writes a local delete
// marker and, for a bounded time, sets shed_state=deleting on the instance,
// so the delete is finished even if this server dies right after.
func (b *Backend) DeleteShed(ctx context.Context, name string) error {
	if err := b.checkReady(); err != nil {
		return err
	}
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	marker := &state.Marker{
		ID: r.Shed.ID, Name: name, Env: r.Env, Ref: r.Ref, At: b.now().UTC(),
		Owner: r.Shed.Owner, CreatedAt: r.Shed.CreatedAt,
	}
	cleanup := b.deletes[marker.ID]
	if cleanup != nil && cleanup.marker != nil {
		// The clean-up of this shed's failed create is still destroying
		// its VM. That job carries on; its marker now stands for the
		// whole shed. The job's own copy changes only once the new one is
		// written: a delete that is refused here leaves it a clean-up.
		whole := *cleanup.marker
		whole.KeepRecord = false
		whole.At = marker.At
		marker = &whole
	}
	if err := b.saveMarkerLocked(marker); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("recording the delete of %s: %w", name, err)
	}
	if cleanup != nil && cleanup.marker != nil {
		*cleanup.marker = *marker
		marker = cleanup.marker
	}
	prior := b.jobs[name]
	if prior != nil {
		prior.cancel()
	}
	b.dropLocked(name)
	ref := marker.Ref
	b.mu.Unlock()

	b.credMgr.StopListener(name)
	b.closeLink(marker.ID)

	if ref != "" {
		lctx, cancel := context.WithTimeout(b.rootCtx, labelTimeout)
		if err := b.markDeleting(lctx, ref, marker.ID); err != nil {
			b.logf("remote: delete %s: could not label the instance %s=%s (the local marker stands): %v",
				name, provider.LabelState, provider.ShedStateDeleting, err)
		}
		cancel()
	}
	b.startDelete(marker, prior)
	return nil
}

// markDeleting sets shed_state=deleting on the instance after checking that
// it is the shed's own. A missing instance is success.
func (b *Backend) markDeleting(ctx context.Context, ref provider.Ref, id string) error {
	if err := b.verify(ctx, ref, id); err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			return nil
		}
		return err
	}
	return b.prov.SetLabels(ctx, ref, map[string]string{provider.LabelState: string(provider.ShedStateDeleting)})
}

// WaitSettled blocks until the shed has no job in flight and returns it as it
// then stands; a shed in status "error" is returned with a nil error. The
// job's progress, from its beginning, is replayed into the progress sink on
// ctx. Cancelling ctx ends the wait, not the job. A shed deleted meanwhile
// gives config.ErrShedNotFoundSentinel.
func (b *Backend) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.checkReady(); err != nil {
		return nil, err
	}
	var following *job
	sent := 0
	for {
		b.mu.Lock()
		r, err := b.lookupLocked(ctx, name)
		if err != nil {
			notFound := errors.Is(err, config.ErrShedNotFoundSentinel)
			gone := b.deletingLocked(ctx, name)
			var owed bool
			var unread error
			if notFound && gone == nil {
				owed, unread = b.deleteOwedLocked(ctx, name)
			}
			b.mu.Unlock()
			if unread != nil {
				// Not "not found": a streamed delete reads that as a delete
				// that finished. The cause names the state directory, so it
				// goes to the log and not to the caller.
				b.logf("remote: shed %s: whether a delete of it is unfinished is not known; the delete markers could not be read: %v", name, unread)
				return nil, fmt.Errorf("shed %s: %w", name, ErrDeleteUnknown)
			}
			if owed {
				return nil, deleteOwed(name)
			}
			if gone == nil || !notFound {
				return nil, err
			}
			// The shed's delete was acknowledged and its VM is still
			// going: wait for the delete job, then report the shed gone,
			// if it is. A job that ended with its marker standing did
			// not destroy the VM, and "gone" would be untrue.
			select {
			case <-gone.done:
				if !gone.gone {
					return nil, deleteOwed(name)
				}
				return nil, err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		j := b.jobs[name]
		shed := b.shedOf(r)
		b.mu.Unlock()

		if j != following {
			if following != nil {
				// Flush what the job that just ended said last.
				following.replay(ctx, sent)
			}
			following, sent = j, 0
		}
		if j == nil {
			if shed.Status != config.StatusStarting && shed.Status != config.StatusStopping {
				return shed, nil
			}
			select {
			case <-time.After(settledPoll):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		// Take the wake-up channel before reading, so an event that lands
		// between the two is not slept through.
		changed := j.changed()
		sent = j.replay(ctx, sent)
		select {
		case <-j.done:
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// deletingLocked returns the delete job in flight for a shed of this name that
// the caller was allowed to see, or nil. A name with no record is all the
// caller has; the marker carries the deleted shed's owner and creation time,
// so another client's shed is as invisible while it is destroyed as it was
// before.
func (b *Backend) deletingLocked(ctx context.Context, name string) *job {
	if b.records[name] != nil {
		return nil
	}
	for _, d := range b.deletes {
		if d.name != name || d.marker == nil || d.marker.KeepRecord {
			continue
		}
		if backend.CheckOwner(ctx, name, d.marker.Owner) != nil || backend.CheckCreatedAt(ctx, name, d.marker.CreatedAt) != nil {
			continue
		}
		return d
	}
	return nil
}

// ErrDeleteOwed is what WaitSettled returns for a shed whose delete was
// acknowledged and whose VM is not destroyed yet with nothing working on it:
// the destroy failed, or was cut short, and the periodic pass will try again.
// It is not config.ErrShedNotFoundSentinel, which a waiter reads as "gone".
var ErrDeleteOwed = errors.New("the delete is recorded but the VM could not be destroyed yet; the server keeps trying")

// ErrDeleteUnknown is what WaitSettled returns for a name with no record and
// no delete job when delete markers stand and cannot be read: a destroy that
// failed leaves nothing but its marker, so the server cannot tell a shed that
// is gone from one whose VM still exists. Like ErrDeleteOwed it is not
// config.ErrShedNotFoundSentinel.
var ErrDeleteUnknown = errors.New("the server could not read its delete records, so it cannot say whether the VM is destroyed (see the server log)")

func deleteOwed(name string) error { return fmt.Errorf("shed %s: %w", name, ErrDeleteOwed) }

// deleteOwedLocked reports whether a delete marker stands, with no delete job
// in flight, for a shed of this name that the caller was allowed to see. The
// marker is removed in the critical section in which its job leaves
// Backend.deletes, so such a marker is a destroy that did not happen.
//
// Markers are read from disk only when one is known to stand (Backend.pending).
// A read that fails is returned as an error, never as "no marker": the marker
// is then the only evidence of the unfinished delete.
func (b *Backend) deleteOwedLocked(ctx context.Context, name string) (bool, error) {
	if b.records[name] != nil || len(b.pending) == 0 {
		return false, nil
	}
	markers, err := b.store.Markers()
	if err != nil {
		return false, err
	}
	for _, m := range markers {
		if m.Name != name || m.KeepRecord || b.deletes[m.ID] != nil {
			continue
		}
		if backend.CheckOwner(ctx, name, m.Owner) == nil && backend.CheckCreatedAt(ctx, name, m.CreatedAt) == nil {
			return true, nil
		}
	}
	return false, nil
}

// replay sends the job's progress events from index from into the sink on
// ctx and returns the next index.
func (j *job) replay(ctx context.Context, from int) int {
	j.emu.Lock()
	events := append([]backend.ProgressEvent(nil), j.events[min(from, len(j.events)):]...)
	next := len(j.events)
	j.emu.Unlock()
	for _, ev := range events {
		backend.EmitProgress(ctx, ev)
	}
	return next
}
