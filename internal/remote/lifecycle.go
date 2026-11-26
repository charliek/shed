package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
)

// ownerFor decides whom a create's machine is for. A service client's shed
// is the client's, with its lease; a person's (a user of the account
// service's list) is theirs. On a server whose provider is the account
// service (the compute contract takes exactly one of user and client on
// every create), whatever the catalog says, and where a test provider's
// catalog asks for it, anyone else (a local admin, the implicit user) is
// refused 422 COMPUTE_POLICY_REFUSED with reason person: local admins
// manage cloud sheds and do not create them.
func (b *Backend) ownerFor(cat provider.Catalog, req config.CreateShedRequest, origin string) (provider.Owner, error) {
	required := cat.OwnerRequired || b.cfg.Provider == config.RemoteProviderService
	switch {
	case req.Owner != "":
		return provider.Owner{Client: req.Owner, Lease: b.clientLease(cat, req.Owner)}, nil
	case !required:
		return provider.Owner{User: req.User}, nil
	case req.User != "" && origin == delivery.OriginDirectory:
		return provider.Owner{User: req.User}, nil
	}
	return provider.Owner{}, &config.ComputeRefusal{Status: http.StatusUnprocessableEntity, Code: config.ErrComputePolicyRefused, Reason: "person",
		Err: errors.New("only a person of the account service or a service client creates a cloud shed; a local admin can list, stop, delete and extend them")}
}

// clientLease is the lease a service client's machine gets: the client's
// policy.max_age (with nothing configured, the service's longest client
// lease). Plan 041 A3 adds the create request's own lease, refused
// POLICY_VIOLATION above max_age; it replaces this choice there.
func (b *Backend) clientLease(cat provider.Catalog, client string) time.Duration {
	if b.opts.Server != nil && b.opts.Server.Auth != nil {
		if c, ok := b.opts.Server.Auth.Clients[client]; ok && c.Policy.MaxAge.Duration() > 0 {
			return c.Policy.MaxAge.Duration()
		}
	}
	if cat.Policy.ClientMaxLease >= provider.MinClientLease {
		return cat.Policy.ClientMaxLease
	}
	return provider.MinClientLease
}

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
	if err := config.ValidateNewShedName(req.Name); err != nil {
		return nil, err
	}
	if err := config.ValidateShedOwnership(req.Owner, req.User); err != nil {
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

	cat, ok := b.Catalog()
	if !ok {
		return nil, computeUnavailable(ErrNotReady)
	}
	// Who the machine is for: only a person or a client creates one where
	// the provider says so (a local admin manages cloud sheds and does not
	// create them). Decided before anything else is asked of the service.
	origin := b.deliverySvc.Origin(req.User)
	owner, err := b.ownerFor(cat, req, origin)
	if err != nil {
		return nil, err
	}
	size, err := resolveSize(cat, req)
	if err != nil {
		return nil, err
	}
	diskGB, err := b.cfg.ResolveDiskGB(req.UpperSizeBytes)
	if err != nil {
		return nil, err
	}
	image := req.Image
	if image == "" {
		image = provider.ImageDefault
	}
	if !cat.HasImage(image) {
		return nil, &config.ComputeRefusal{Status: http.StatusUnprocessableEntity, Code: config.ErrComputePolicyRefused, Reason: "image",
			Err: fmt.Errorf("image %q is not one the account service offers (%s or %s)", image, provider.ImageDefault, strings.Join(cat.Aliases, ", "))}
	}
	// A name that is taken is answered as such whatever the firewall check
	// says: a client reads that 409 as "the shed exists".
	b.mu.Lock()
	_, exists := b.records[req.Name]
	b.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}
	// No machine is created while the firewall check fails.
	if err := b.firewallGate(ctx, network); err != nil {
		return nil, err
	}

	id := req.ID
	if id == "" {
		if id, err = config.NewShedID(); err != nil {
			return nil, err
		}
	}
	repoURL := httpsRepoURL(req.Repo)
	// Everything that is stored or reported is derived from the URL with
	// its user and password removed; only the clone sees repoURL.
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

	// The create is stored whole and replayed verbatim on a resume, its
	// request id included. Where the user came from, by the configuration
	// now, is in its spec: the evidence an adoption recovers it from.
	spec, err := provider.NewSpec(id, req.Name, owner,
		provider.ShedSpec{CreatedAt: createdAt, Repo: storedRepo, Network: network, LandingDir: landingDir, UserOrigin: origin},
		b.opts.AuthorizedKeys, size.Name, diskGB, image,
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
			User:           req.User,
			NetworkProfile: network,
			Arch:           size.Arch,
		},
		Spec:        &spec,
		Job:         state.JobCreate,
		NoProvision: req.NoProvision,
		// Every step owed until the create job decides it.
		Delivery:    delivery.NewRecord(),
		NoServerEnv: req.NoServerEnv,
		ScriptKey:   req.ScriptKey,
		UserOrigin:  origin,
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
	j.deliver = true
	// The delivery's admission, taken with the job, before the cloud
	// provisions or the agent is waited for: a stop or delete of the shed
	// during its boot ends the run before it asks.
	j.ticket = b.deliverySvc.Admit(req.Name, id)
	j.actor, j.hasActor = audit.ActorFrom(ctx)
	j.fastWindow = true
	j.nameDeadline = time.Now().Add(b.nameWait())
	j.firstRun = true
	first := b.shedOf(rec)
	b.mu.Unlock()
	b.launch(j)

	window := time.NewTimer(b.opts.FastFailWindow)
	defer window.Stop()
	select {
	case <-j.accepted:
	case <-j.done:
	case <-window.C:
		// A create deciding what holds its name (nameHeld) has one answer
		// to give, and gives it within a bounded time: it is waited for.
		b.mu.Lock()
		naming := j.naming
		b.mu.Unlock()
		if naming != nil {
			select {
			case <-naming:
			case <-j.done:
			case <-ctx.Done():
			}
		}
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

// GetShed returns a shed from the local record cache. It answers while the
// backend is degraded too: the SSH server reads a shed through it, so an
// existing shed's sessions keep working while the service is down.
func (b *Backend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		return nil, err
	}
	return b.shedOf(r), nil
}

// ListSheds returns every shed in the local record cache, ordered by name,
// degraded or not.
func (b *Backend) ListSheds(context.Context) ([]config.Shed, error) {
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
		network := r.Shed.NetworkProfile
		b.mu.Unlock()
		if err := b.firewallGate(ctx, network); err != nil {
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
	// A provision's step in hand finishes first; its next step then finds
	// the stop and ends the provision.
	defer b.waitStep(name)()
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	// A delivery of this shed (this incarnation: its id, checked just
	// now) ends: one in flight is aborted, and one admitted and not started
	// never asks.
	b.deliverySvc.EndShed(name, r.Shed.ID)
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

// DeleteShed hides the shed at once and destroys its machine in the
// background. It succeeds in every status. Before it returns it writes a
// local delete marker and, for a bounded time, sets shed_state=deleting on
// the machine, so the delete is finished even if this server dies right
// after.
func (b *Backend) DeleteShed(ctx context.Context, name string) error {
	if err := b.checkReady(); err != nil {
		return err
	}
	// A provision's step in hand finishes first (see StopShed).
	defer b.waitStep(name)()
	b.mu.Lock()
	r, err := b.lookupLocked(ctx, name)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	// A delivery of this shed (this incarnation: its id, checked just
	// now) ends: one in flight is aborted, and one admitted and not started
	// never asks.
	b.deliverySvc.EndShed(name, r.Shed.ID)
	// The marker carries the owner the shed was reported with
	// (ownership), never the raw stored one: a wait that follows the delete
	// through the marker is bound to what its caller was authorized on.
	reportedOwner, _ := ownership(r)
	marker := &state.Marker{
		ID: r.Shed.ID, Name: name, Ref: r.Ref, At: b.now().UTC(),
		Owner: reportedOwner, CreatedAt: r.Shed.CreatedAt,
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
		if err := b.markDeleting(lctx, marker, ref); err != nil {
			b.logf("remote: delete %s: could not label the machine %s=%s (the local marker stands): %v",
				name, provider.LabelState, provider.ShedStateDeleting, err)
		}
		cancel()
	}
	b.startDelete(marker, prior)
	return nil
}

// markDeleting sets shed_state=deleting on the machine, with the request id
// the delete's marker keeps for it. A missing machine, or one the service is
// deleting already, is success.
func (b *Backend) markDeleting(ctx context.Context, m *state.Marker, ref provider.Ref) error {
	set := map[string]*string{provider.LabelState: provider.Str(string(provider.ShedStateDeleting))}
	rid, err := b.markerRequestID(m, &m.LabelID)
	if err != nil {
		return err
	}
	err = b.retryUnknown(ctx, rid, func(rid string) error {
		_, err := b.prov.SetLabels(ctx, ref, rid, set, nil)
		return err
	})
	if errors.Is(err, provider.ErrNotFound) || errors.Is(err, provider.ErrDeleting) {
		return nil
	}
	return err
}

// WaitSettled blocks until the shed has no job in flight and returns it as it
// then stands; a shed in status "error" is returned with a nil error. The
// job's progress, from its beginning, is replayed into the progress sink on
// ctx. Cancelling ctx ends the wait, not the job. A shed deleted meanwhile
// gives config.ErrShedNotFoundSentinel.
func (b *Backend) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
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
// the caller was allowed to see, or nil. The marker carries the deleted shed's
// owner and creation time, so another client's shed is as invisible while it
// is destroyed as it was before, and a wait bound to the deleted incarnation
// (backend.WithRequiredCreatedAt) finds that one's job by them. It looks
// whether or not a record holds the name now: the caller has already found
// that record is not the shed it asks after, and a shed created again under
// the name (by anyone) must not hide the old one's unfinished delete.
func (b *Backend) deletingLocked(ctx context.Context, name string) *job {
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
// Backend.deletes, so such a marker is a destroy that did not happen. Like
// deletingLocked it looks whether or not a record holds the name now.
//
// Markers are read from disk only when one is known to stand (Backend.pending).
// A read that fails, of the directory or of one marker known to stand, is
// returned as an error, never as "no marker": the marker is then the only
// evidence of the unfinished delete.
func (b *Backend) deleteOwedLocked(ctx context.Context, name string) (bool, error) {
	if len(b.pending) == 0 {
		return false, nil
	}
	markers, err := b.store.Markers()
	if err != nil {
		return false, err
	}
	read := make(map[string]bool, len(markers))
	for _, m := range markers {
		read[m.ID] = true
		if m.Name != name || m.KeepRecord || b.deletes[m.ID] != nil {
			continue
		}
		if backend.CheckOwner(ctx, name, m.Owner) == nil && backend.CheckCreatedAt(ctx, name, m.CreatedAt) == nil {
			return true, nil
		}
	}
	// Store.Markers leaves out a file it cannot read or parse, without an
	// error. A marker this server knows to stand (b.pending; files and map
	// change together under b.mu) that did not come back may be this shed's:
	// the file is where its name is kept, so whose it is cannot be told. One
	// with a delete job is in hand and is waited for by the caller.
	for id := range b.pending {
		if !read[id] && b.deletes[id] == nil {
			return false, fmt.Errorf("the delete marker of shed ID %s is on record and could not be read", id)
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

// resolveSize is the size a create gets. The API resolved it already
// (req.SizeName, with the numbers a service client's caps were checked
// against): that size is used as it is, and a catalog that has changed
// since (the size gone, or other numbers) is refused SIZE_UNAVAILABLE, never
// resolved again to a size nobody checked. A caller other than the API
// (SizeName empty) has the numbers resolved here.
func resolveSize(cat provider.Catalog, req config.CreateShedRequest) (backend.CatalogSize, error) {
	if req.SizeName == "" {
		return computeCatalog(cat).ResolveSize(req.CPUs, req.MemoryMB)
	}
	s, ok := cat.SizeNamed(req.SizeName)
	if !ok || s.CPUs != req.CPUs || s.MemoryMB != req.MemoryMB {
		return backend.CatalogSize{}, fmt.Errorf("%w: the size %s (%d CPUs, %d MB) the request was checked against is no longer offered as such; send the create again",
			config.ErrSizeUnavailableSentinel, req.SizeName, req.CPUs, req.MemoryMB)
	}
	return backend.CatalogSize{Name: s.Name, CPUs: s.CPUs, MemoryMB: s.MemoryMB, Arch: s.Arch, DiskGBDefault: s.DiskGBDefault}, nil
}
