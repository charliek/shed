// Package remote is the backend that runs each shed as one VM in a cloud. It
// holds a provider.Provider (the cloud) and a transport.Factory (the way to a
// guest's agent) and nothing cloud-specific of its own.
//
// Create, start, stop and delete each save the new status, start a job on a
// context the server owns and return. A job is background work for one shed
// that outlives the HTTP request. GetShed and ListSheds read only the local
// record cache (internal/remote/state).
package remote

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/vmutil"
)

// Defaults for Options.
const (
	// DefaultBootTimeout bounds the wait, after the cloud reports a VM
	// running, for its agent to answer a probe.
	DefaultBootTimeout = 5 * time.Minute
	// DefaultAuthGrace is how long a guest may keep refusing the server's
	// key before the job gives up. sshd can accept connections before the
	// guest's boot unit has written the key file.
	DefaultAuthGrace = 90 * time.Second
	// DefaultProbeInterval is the time between probes while a guest boots.
	DefaultProbeInterval = 250 * time.Millisecond
	// probeTimeout bounds one probe.
	probeTimeout = 15 * time.Second
	// labelTimeout bounds the shed_state=deleting write made before a
	// delete is acknowledged.
	labelTimeout = 10 * time.Second
	// cleanupTimeout bounds best-effort cloud work after a failed job.
	cleanupTimeout = 3 * time.Minute
	// closeGrace is how long Close waits for jobs to notice the shutdown.
	closeGrace = 5 * time.Second
	// DefaultCloneTimeout bounds one attempt at a create's --repo clone.
	DefaultCloneTimeout = 5 * time.Minute
	// DefaultCloneRetryPause is the pause between two clone attempts.
	DefaultCloneRetryPause = 5 * time.Second
	// DefaultSweepInterval is the time between two periodic passes.
	DefaultSweepInterval = 60 * time.Second
	// DefaultDeleteRetryAge is how old a delete marker with no job must be
	// before the periodic pass retries it. A delete that was just
	// acknowledged has its own job on the way.
	DefaultDeleteRetryAge = 2 * time.Minute
	// StaleCreateAge is how long after the cloud made it an instance may
	// stay labelled creating with no local record before the start-up and
	// periodic passes destroy it as an abandoned create. A create waits 15 s
	// for the insert, 5 minutes for the agent and less for everything else,
	// and has a record throughout; this is three times that.
	StaleCreateAge = 15 * time.Minute
	// DefaultImageResolveTimeout bounds the image lookup a create makes
	// before it saves anything. With the fast-fail window it keeps the JSON
	// create's reply under the 30 s a client waits.
	DefaultImageResolveTimeout = 10 * time.Second
	// sweepTimeout bounds one periodic pass.
	sweepTimeout = 2 * time.Minute
	// localAuthorizedKey stands in for the server's public key when the
	// transport needs none (the local link).
	localAuthorizedKey = "none local-transport"
)

// ErrNotReady is returned by shed calls made before the first complete list
// of the cloud. The server waits for Ready before it serves shed routes, so
// a client sees it only if that wait was skipped.
var ErrNotReady = errors.New("remote backend has not finished listing the cloud")

// Options configures a Backend.
type Options struct {
	// Config is the validated remote block. Required.
	Config *config.RemoteConfig
	// Server is the whole server config; it supplies service clients'
	// max_age and the settings provisioning reads. May be nil in tests.
	Server *config.ServerConfig
	// Provider is the cloud. Required.
	Provider provider.Provider
	// Links makes the connection to each guest's agent. Required.
	Links transport.Factory
	// StateDir holds the record cache and delete markers. Required.
	StateDir string
	// AuthorizedKeys are the server's SSH public keys in authorized_keys
	// form, written to each instance's metadata. Required with the SSH
	// transport.
	AuthorizedKeys []string
	// Bridge is the plugin bridge message channels register with. May be
	// nil.
	Bridge *plugin.Bridge
	// KeepFailedCreate leaves the VM of a failed create in place, for
	// inspection, until the shed is deleted. By default it is destroyed.
	KeepFailedCreate bool

	// FastFailWindow, BootTimeout, AuthGrace and ProbeInterval override the
	// defaults; tests shorten them.
	FastFailWindow time.Duration
	BootTimeout    time.Duration
	AuthGrace      time.Duration
	ProbeInterval  time.Duration
	// HostKeyPollInterval is the least time between two reads of a guest's
	// published host key. Default 6 s, which keeps under Google's limit on
	// guest-attribute reads.
	HostKeyPollInterval time.Duration
	// CloneTimeout bounds one attempt at a create's --repo clone and
	// CloneRetryPause is the pause before the next attempt.
	CloneTimeout    time.Duration
	CloneRetryPause time.Duration
	// SweepInterval is the time between two periodic passes (see Sweep).
	// Default 60 s; a negative value turns the periodic pass off.
	SweepInterval time.Duration
	// DeleteRetryAge is how old a delete marker with no job must be before
	// the periodic pass retries it. Default 2 minutes.
	DeleteRetryAge time.Duration
	// ImageResolveTimeout bounds the image lookup a create makes before it
	// saves anything. Default 10 s.
	ImageResolveTimeout time.Duration
	// FirewallSources are the source ranges the environment's firewall
	// rule must have with remote.firewall.mode: static, as the server
	// resolved them at start-up (config.RemoteRuntime.FirewallSources).
	// Required in static mode.
	FirewallSources []string
	// EchoClient, when set, supplies the HTTP transport for auto mode's
	// echo requests. Tests pass one that trusts their servers.
	EchoClient *http.Client
	// Logf receives the backend's log lines. Default log.Printf.
	Logf func(format string, args ...any)
	// Now is the clock for record timestamps. Default time.Now.
	Now func() time.Time
}

// Backend is the remote backend. Build one with New, then run Reconcile
// once; shed calls answer ErrNotReady until its first pass has finished.
type Backend struct {
	opts    Options
	cfg     *config.RemoteConfig
	prov    provider.Provider
	links   transport.Factory
	fw      *firewallState
	store   *state.Store
	credMgr *vmutil.CredentialManager
	logf    func(format string, args ...any)
	now     func() time.Time
	// clone runs one attempt at a create's --repo clone. It is
	// vmutil.CloneRepo; tests replace it.
	clone func(ctx context.Context, agent *vmutil.AgentClient, serverCfg *config.ServerConfig, repo string, owned bool, extraEnv []string) error

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup

	ready     chan struct{}
	readyOnce sync.Once
	sweepOnce sync.Once

	mu       sync.Mutex
	closed   bool
	records  map[string]*state.Record // by shed name
	jobs     map[string]*job          // lifecycle job in flight, by shed name
	deletes  map[string]*job          // delete job in flight, by shed ID
	pending  map[string]time.Time     // delete markers on disk: shed ID -> when acknowledged
	conns    map[string]*shedLink     // by shed ID
	idle     chan struct{}            // closed and replaced whenever a job ends
	reported []string                 // what the last reconcile pass reported
}

var (
	_ backend.Backend            = (*Backend)(nil)
	_ backend.AsyncLifecycle     = (*Backend)(nil)
	_ backend.NetworkProfiles    = (*Backend)(nil)
	_ backend.CapabilityReporter = (*Backend)(nil)
)

// New builds the backend and loads the record cache. It makes no cloud call
// and no echo request.
func New(opts Options) (*Backend, error) {
	switch {
	case opts.Config == nil:
		return nil, errors.New("remote: no remote config")
	case opts.Provider == nil:
		return nil, errors.New("remote: no provider")
	case opts.Links == nil:
		return nil, errors.New("remote: no link factory")
	case !provider.ValidEnv(opts.Config.Environment):
		return nil, fmt.Errorf("remote: environment %q is not a valid name", opts.Config.Environment)
	}
	if len(opts.AuthorizedKeys) == 0 {
		if opts.Links.Name() != transport.NameLocal {
			return nil, errors.New("remote: no SSH public key to give new instances")
		}
		opts.AuthorizedKeys = []string{localAuthorizedKey}
	}
	if opts.FastFailWindow <= 0 {
		opts.FastFailWindow = provider.FastFailWindow
	}
	if opts.BootTimeout <= 0 {
		opts.BootTimeout = DefaultBootTimeout
	}
	if opts.AuthGrace <= 0 {
		opts.AuthGrace = DefaultAuthGrace
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = DefaultProbeInterval
	}
	if opts.HostKeyPollInterval <= 0 {
		opts.HostKeyPollInterval = guestAttrInterval
	}
	if opts.CloneTimeout <= 0 {
		opts.CloneTimeout = DefaultCloneTimeout
	}
	if opts.CloneRetryPause <= 0 {
		opts.CloneRetryPause = DefaultCloneRetryPause
	}
	if opts.SweepInterval == 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if opts.DeleteRetryAge <= 0 {
		opts.DeleteRetryAge = DefaultDeleteRetryAge
	}
	if opts.ImageResolveTimeout <= 0 {
		opts.ImageResolveTimeout = DefaultImageResolveTimeout
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	fw, err := newFirewallState(opts.Config, opts.FirewallSources, opts.EchoClient)
	if err != nil {
		return nil, err
	}

	store, err := state.Open(opts.StateDir)
	if err != nil {
		return nil, err
	}
	records, bad, err := store.Records()
	if err != nil {
		return nil, err
	}
	for _, p := range bad {
		opts.Logf("remote: ignoring unreadable shed record %s", p)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		opts:       opts,
		cfg:        opts.Config,
		prov:       opts.Provider,
		links:      opts.Links,
		fw:         fw,
		store:      store,
		credMgr:    vmutil.NewCredentialManager(opts.Server, opts.Bridge, config.BackendRemote, vmutil.NewHealthTracker()),
		logf:       opts.Logf,
		now:        opts.Now,
		clone:      vmutil.CloneRepo,
		rootCtx:    ctx,
		rootCancel: cancel,
		ready:      make(chan struct{}),
		records:    make(map[string]*state.Record),
		jobs:       make(map[string]*job),
		deletes:    make(map[string]*job),
		pending:    make(map[string]time.Time),
		conns:      make(map[string]*shedLink),
		idle:       make(chan struct{}),
	}
	for _, r := range records {
		b.records[r.Shed.Name] = r
	}
	return b, nil
}

// Type returns backend.TypeRemote.
func (b *Backend) Type() backend.Type { return backend.TypeRemote }

// Capabilities reports what the remote backend supports.
func (b *Backend) Capabilities() backend.Capabilities { return backend.RemoteCapabilities() }

// SupportsNetworkProfile reports whether a shed may ask for the profile.
// "isolated" means isolation by network topology: no route between sheds and
// no private route to the server host. Outbound traffic is not filtered, so a
// shed still reaches any public address, a public port of the server host
// included; no egress rule exists or is checked. It is refused when the
// config accepts a firewall rule open to more than the server (mode any, or
// acknowledged wide ranges).
func (b *Backend) SupportsNetworkProfile(profile string) bool {
	switch profile {
	case config.NetworkProfileOpen:
		return true
	case config.NetworkProfileIsolated:
		return !b.cfg.Firewall.AdmitsAny()
	}
	return false
}

// Ready is closed once the first reconcile pass has listed the cloud and
// repaired the cache. Shed routes may be served only after that.
func (b *Backend) Ready() <-chan struct{} { return b.ready }

// WaitReady blocks until Ready or until ctx ends.
func (b *Backend) WaitReady(ctx context.Context) error {
	select {
	case <-b.ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("remote backend: waiting for the first cloud list: %w", ctx.Err())
	}
}

func (b *Backend) checkReady() error {
	select {
	case <-b.ready:
		return nil
	default:
		return ErrNotReady
	}
}

// Close stops every job where it stands, closes every link and message
// channel, and leaves the records as they are: a shed that was starting or
// stopping is picked up by the next server's reconcile pass. No VM is
// stopped.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()

	b.rootCancel()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
		b.logf("remote: jobs still running %s after shutdown began", closeGrace)
	}

	b.credMgr.Close()
	b.mu.Lock()
	conns := b.conns
	b.conns = make(map[string]*shedLink)
	b.mu.Unlock()
	for _, c := range conns {
		_ = c.link.Close()
	}
	return nil
}

// WaitIdle blocks until no job of any kind is in flight, or ctx ends. Tests
// and orderly shutdowns use it.
func (b *Backend) WaitIdle(ctx context.Context) error {
	for {
		b.mu.Lock()
		n := len(b.jobs) + len(b.deletes)
		idle := b.idle
		b.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Reported returns what the last reconcile pass found and left alone.
func (b *Backend) Reported() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.reported...)
}

// shedOf returns the API view of a record.
func (b *Backend) shedOf(r *state.Record) *config.Shed {
	shed := r.Shed
	shed.Backend = config.BackendRemote
	if r.Shed.Status == config.StatusRunning {
		if hs, ok := b.credMgr.HealthTracker().Get(r.Shed.Name); ok {
			lastHealthy := hs.LastSeen
			startedAt := hs.AgentStartedAt
			shed.LastHealthy = &lastHealthy
			shed.StartedAt = &startedAt
		}
	}
	return &shed
}

// saveLocked writes a record to disk. A failure is logged: the record in
// memory stays the working copy, and the next save retries.
func (b *Backend) saveLocked(r *state.Record) {
	if err := b.store.SaveRecord(r); err != nil {
		b.logf("remote: saving the record of shed %s: %v", r.Shed.Name, err)
	}
}

// saveMarkerLocked writes a delete marker and notes it as pending. An error
// means no marker was put in place, and nothing will ever act on one. A
// marker that is in place with only its directory's flush failed
// (state.ErrNotDurable) is a written marker: the periodic pass and the next
// start-up read it and destroy the VM, so the caller must not report the
// delete as failed.
func (b *Backend) saveMarkerLocked(m *state.Marker) error {
	if err := b.inPlace(b.store.SaveMarker(m)); err != nil {
		return err
	}
	b.pending[m.ID] = m.At
	return nil
}

// inPlace turns the error of a state write that did put its file in place
// (state.ErrNotDurable) into a log line and nil. What is lost is only that a
// power cut might undo the write; the cloud's own labels cover that for a
// delete (shed_state=deleting) and the start-up pass for a create.
func (b *Backend) inPlace(err error) error {
	if errors.Is(err, state.ErrNotDurable) {
		b.logf("remote: %v; carrying on with the file as written", err)
		return nil
	}
	return err
}

// dropLocked removes a record from memory and disk.
func (b *Backend) dropLocked(name string) {
	delete(b.records, name)
	if err := b.store.DeleteRecord(name); err != nil {
		b.logf("remote: removing the record of shed %s: %v", name, err)
	}
}

// currentLocked returns the record of the shed with this name and ID, or nil
// when it was deleted (or deleted and created again, which is another shed).
func (b *Backend) currentLocked(name, id string) *state.Record {
	r := b.records[name]
	if r == nil || r.Shed.ID != id {
		return nil
	}
	return r
}

// snapshot returns a copy of the shed's record, or nil when it is gone.
func (b *Backend) snapshot(name, id string) *state.Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentLocked(name, id).Clone()
}

// update applies fn to the shed's record and saves it. It reports false, and
// changes nothing, when the record is gone.
func (b *Backend) update(name, id string, fn func(*state.Record)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.currentLocked(name, id)
	if r == nil {
		return false
	}
	fn(r)
	b.saveLocked(r)
	return true
}

// lookupLocked finds a record by name and applies the caller's ownership
// requirements to it.
func (b *Backend) lookupLocked(ctx context.Context, name string) (*state.Record, error) {
	r := b.records[name]
	if r == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	if err := backend.CheckOwner(ctx, name, r.Shed.Owner); err != nil {
		return nil, err
	}
	if err := backend.CheckCreatedAt(ctx, name, r.Shed.CreatedAt); err != nil {
		return nil, err
	}
	return r, nil
}

// unsupported is the error for a feature the remote backend does not have.
// It matches both the feature-unsupported sentinel and the older
// not-supported one, so either API mapping answers 501.
func unsupported(what string) error {
	return fmt.Errorf("%w: %w: %s on the remote backend", config.ErrFeatureUnsupportedSentinel, config.ErrNotSupportedSentinel, what)
}

// mapRefusal turns a provider refusal into the API's sentinel. Other errors
// pass through.
func mapRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, provider.ErrLimit):
		return fmt.Errorf("%w: %w", config.ErrServerLimitReachedSentinel, err)
	case errors.Is(err, provider.ErrQuota):
		return fmt.Errorf("%w: %w", config.ErrCloudQuotaExceededSentinel, err)
	case errors.Is(err, provider.ErrCapacity):
		return fmt.Errorf("%w: %w", config.ErrZoneCapacityUnavailableSentinel, err)
	case errors.Is(err, provider.ErrInvalid):
		return fmt.Errorf("%w: %w", config.ErrInvalidShedRequestSentinel, err)
	}
	return err
}

// --- features the remote backend does not have ---

// ResetShed is not supported.
func (b *Backend) ResetShed(context.Context, string) (*config.Shed, error) {
	return nil, unsupported("reset")
}

// ListImages returns no images: a cloud image is named in the server config,
// not managed through the API.
func (b *Backend) ListImages(context.Context) ([]config.ImageInfo, error) {
	return []config.ImageInfo{}, nil
}

// InspectImage is not supported.
func (b *Backend) InspectImage(context.Context, string) (config.ImageInspectResponse, error) {
	return config.ImageInspectResponse{}, unsupported("image inspect")
}

// TagImage is not supported.
func (b *Backend) TagImage(context.Context, string, string) error {
	return unsupported("image tag")
}

// PullImage is not supported.
func (b *Backend) PullImage(context.Context, string, string, string, bool) (string, error) {
	return "", unsupported("image pull")
}

// PushImage is not supported.
func (b *Backend) PushImage(context.Context, string, string) error {
	return unsupported("image push")
}

// DeleteImage is not supported.
func (b *Backend) DeleteImage(context.Context, string) error {
	return unsupported("image delete")
}

// PruneImages has nothing to prune.
func (b *Backend) PruneImages(context.Context, bool) ([]config.ImageInfo, error) {
	return []config.ImageInfo{}, nil
}

// DiskUsage reports nothing: the server host holds no shed disks.
func (b *Backend) DiskUsage(context.Context) (config.DiskUsage, error) {
	name := ""
	if b.opts.Server != nil {
		name = b.opts.Server.Name
	}
	return config.DiskUsage{ServerName: name, Backend: config.BackendRemote, GeneratedAt: b.now()}, nil
}

// Prune has nothing to prune.
func (b *Backend) Prune(context.Context, backend.PruneOptions) (config.PruneReport, error) {
	return config.PruneReport{}, nil
}

// CreateSnapshot is not supported.
func (b *Backend) CreateSnapshot(context.Context, config.SnapshotCreateRequest) (*config.Snapshot, error) {
	return nil, unsupported("snapshots")
}

// ListSnapshots returns no snapshots.
func (b *Backend) ListSnapshots(context.Context) ([]config.Snapshot, error) {
	return []config.Snapshot{}, nil
}

// GetSnapshot is not supported.
func (b *Backend) GetSnapshot(context.Context, string) (*config.Snapshot, error) {
	return nil, unsupported("snapshots")
}

// DeleteSnapshot is not supported.
func (b *Backend) DeleteSnapshot(context.Context, string) error {
	return unsupported("snapshots")
}
