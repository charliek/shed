// Package remote is the backend that runs each shed as one cloud machine. It
// holds a provider.Provider (where machines come from: the account service's
// compute, over the compute contract) and a transport.Factory (the way to a
// guest's agent) and nothing cloud-specific of its own.
//
// Create, start, stop and delete each save the new status, start a job on a
// context the server owns and return. A job is background work for one shed
// that outlives the HTTP request. GetShed and ListSheds read only the local
// record cache (internal/remote/state), so they answer, and existing links
// keep working, while the service is down.
package remote

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/lockmap"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/service"
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
	// DefaultUnknownRetryPause is the pause before a call that changes
	// something is sent again, with the same request id, after its reply
	// was lost (provider.ErrUnavailable); unknownRetries is how many times.
	DefaultUnknownRetryPause = 2 * time.Second
	unknownRetries           = 2
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
	// StaleCreateAge is how long after the provider made it a machine may
	// stay labelled creating with no local record before the start-up and
	// periodic passes destroy it as an abandoned create. A create waits 15 s
	// for the accept, 5 minutes for the agent and less for everything else,
	// and has a record throughout; this is three times that.
	StaleCreateAge = 15 * time.Minute
	// sweepTimeout bounds one periodic pass.
	sweepTimeout = 2 * time.Minute
	// localAuthorizedKey stands in for the server's public key when the
	// transport needs none (the local link). It is one well-formed
	// authorized_keys line, as the compute contract requires of every key a
	// create names (a service refuses anything else as INVALID); its private
	// half was discarded when it was made, so nobody can log in with it.
	localAuthorizedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN7Ig741G+LZ7HvFwT5YGDMQ8q+YVi5wYbcntd2EqsrE shed-local-transport"
)

// ErrNotReady is the cause of the refusal (503 COMPUTE_UNAVAILABLE) a create,
// start, stop, delete or extend gets before the first complete list of the
// machines: a server that started degraded, with the service down, answers
// reads from its records and refuses everything that needs the service
// until the list arrives.
var ErrNotReady = errors.New("the remote backend has not listed its machines yet: the account service could not be reached")

// ErrLegacyState is New's error for a state directory that holds what the
// in-tree cloud provider of earlier releases wrote.
var ErrLegacyState = errors.New("the remote state directory holds sheds of the in-tree cloud provider this server no longer has")

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
	// AuditLog is the server's audit log, where a shed whose machine the
	// account service deleted (its lease ended, a delete at the service)
	// is recorded. Nil records nothing. It is given here, not later, so
	// that the start-up pass (Reconcile), which drops such sheds too, has
	// it from its first line.
	AuditLog *audit.Log
	// FastFailWindow, BootTimeout, AuthGrace and ProbeInterval override the
	// defaults; tests shorten them.
	FastFailWindow time.Duration
	BootTimeout    time.Duration
	AuthGrace      time.Duration
	ProbeInterval  time.Duration
	// HostKeyPollInterval is the least time between two reads of a
	// machine whose access, or attested host key, is not known yet.
	// Default 3 s.
	HostKeyPollInterval time.Duration
	// UnknownRetryPause is the pause before a call whose reply was lost is
	// sent again. Default 2 s.
	UnknownRetryPause time.Duration
	// CloneTimeout bounds one attempt at a create's --repo clone and
	// CloneRetryPause is the pause before the next attempt.
	CloneTimeout    time.Duration
	CloneRetryPause time.Duration
	// ListRetry is the pause between two attempts at the start-up pass's
	// first list. Default 5 s.
	ListRetry time.Duration
	// SweepInterval is the time between two periodic passes (see Sweep).
	// Default 60 s; a negative value turns the periodic pass off.
	SweepInterval time.Duration
	// DeleteRetryAge is how old a delete marker with no job must be before
	// the periodic pass retries it. Default 2 minutes.
	DeleteRetryAge time.Duration
	// FirewallSources are the sources the machines' ingress must admit
	// with remote.firewall.mode: static, as the server resolved them at
	// start-up (config.RemoteRuntime.FirewallSources). Required in static
	// mode.
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
	access  accessRule
	store   *state.Store
	credMgr *vmutil.CredentialManager
	logf    func(format string, args ...any)
	now     func() time.Time
	// clone runs one attempt at a create's --repo clone. It is
	// vmutil.CloneRepo; tests replace it.
	clone func(ctx context.Context, agent *vmutil.AgentClient, serverCfg *config.ServerConfig, repo string, owned bool, extraEnv []string) error
	// workTree probes what stands at a repository's project directory
	// (vmutil.ProbeWorkTree), hooks runs a shed's hooks as the install step
	// (vmutil.Provisioner.HooksStep), and writer is the agent a delivered
	// stage is written through (the shed's own). Tests replace them.
	workTree func(ctx context.Context, agent *vmutil.AgentClient, repo string) (vmutil.WorkTree, error)
	hooks    func(ctx context.Context, agent *vmutil.AgentClient, rec *state.Record, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool)
	writer   func(agent *vmutil.AgentClient) delivery.Agent
	// rmPartial is removePartialClone (tests count it).
	rmPartial func(ctx context.Context, agent *vmutil.AgentClient, repo string)
	// saveRecord is the store's SaveRecord for a delivery step's write
	// (tests fail it).
	saveRecord func(*state.Record) error
	// saveMarker is the store's SaveMarker for the write of a delete
	// marker's request id (tests fail it).
	saveMarker func(*state.Marker) error
	// flushedIDs are the request ids, of delete markers and of records
	// (a create's, a start's, a stop's, a label write's), this process has
	// written with a save whose flush succeeded: only those are sent. An
	// id read back from disk is saved again, durably, before it is sent
	// (markerRequestID, confirmFlushed). Guarded by mu.
	flushedIDs map[string]bool

	// deliverySvc asks a shed user's source for its credentials (nil: no
	// delivery). stepLocks holds a provision's step in hand against a stop
	// or a delete of the shed (by name); provisioning holds one provision
	// per shed.
	deliverySvc *delivery.Service
	// resumeGate, when set (HoldResumes), holds a resumed create's steps
	// until it is closed (ReleaseResumes). Set before Reconcile.
	resumeGate   chan struct{}
	resumeOnce   sync.Once
	stepLocks    lockmap.NamedMutexMap
	provisioning lockmap.NamedMutexMap
	// halting is raised by a stop or a delete of the shed while it waits
	// for the step lock: a provision's next step gives way to it.
	halting lockmap.Flags
	// extending holds one extend per shed (by shed id) at a time, so an
	// extend's stored request id (Record.Extend) is read and written by one
	// call only. Its wait ends with the caller's context (extendSlots).
	extending extendSlots
	// usage keeps GET /api/cloud/usage's answers for usageTTL, by the user
	// asked about ("" for the organization only).
	usageMu sync.Mutex
	usage   map[string]usageEntry

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup

	ready     chan struct{}
	readyOnce sync.Once
	sweepOnce sync.Once
	// failed is closed, with startErr, when the start-up pass finds a
	// reason the server must not run (remote.firewall.mode: auto against a
	// service whose ingress is managed elsewhere); WaitReady returns it.
	failed     chan struct{}
	failedOnce sync.Once
	startErr   error

	// auditLog, when set (Options.AuditLog, at New), is where a machine
	// the service deleted (its lease ended, a page delete) is recorded as
	// a shed.delete event.
	auditLog *audit.Log

	// busApplyMu serializes every application of a shed's reported user to
	// its bus route (busSyncWorker): each reads the record and the hold's
	// epoch, applies the user and lifts the hold as one, so a stale user is
	// never applied after a newer one. Taken before mu, never under it.
	busApplyMu sync.Mutex

	mu     sync.Mutex
	closed bool
	// catalog is the provider's last catalog, read at start-up and at every
	// periodic pass; haveCatalog is false until the first read.
	catalog     provider.Catalog
	haveCatalog bool
	records     map[string]*state.Record // by shed name
	// busSyncs are the bus routes held for a change of the shed's reported
	// user and not yet lifted; busStarting counts the message-channel starts
	// in progress. Both by shed name (see busUserChangedLocked).
	busSyncs    map[string]*busSync
	busStarting map[string]int
	jobs        map[string]*job      // lifecycle job in flight, by shed name
	deletes     map[string]*job      // delete job in flight, by shed ID
	pending     map[string]time.Time // delete markers on disk: shed ID -> when acknowledged
	conns       map[string]*shedLink // by shed ID
	idle        chan struct{}        // closed and replaced whenever a job ends
	reported    []string             // what the last reconcile pass reported
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
		opts.HostKeyPollInterval = accessRetry
	}
	if opts.UnknownRetryPause <= 0 {
		opts.UnknownRetryPause = DefaultUnknownRetryPause
	}
	if opts.ListRetry <= 0 {
		opts.ListRetry = listRetry
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
	access, err := newAccessRule(opts.Config)
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
	if err := refuseLegacy(store, records); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		opts:     opts,
		cfg:      opts.Config,
		prov:     opts.Provider,
		links:    opts.Links,
		fw:       fw,
		access:   access,
		store:    store,
		credMgr:  vmutil.NewCredentialManager(opts.Server, opts.Bridge, config.BackendRemote, vmutil.NewHealthTracker()),
		logf:     opts.Logf,
		now:      opts.Now,
		clone:    vmutil.CloneRepo,
		workTree: vmutil.ProbeWorkTree,
		hooks: func(ctx context.Context, agent *vmutil.AgentClient, rec *state.Record, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool) {
			return provisionerFor(agent, rec).HooksStep(ctx, install, startup)
		},
		writer:      func(agent *vmutil.AgentClient) delivery.Agent { return agent },
		rootCtx:     ctx,
		rootCancel:  cancel,
		ready:       make(chan struct{}),
		failed:      make(chan struct{}),
		records:     make(map[string]*state.Record),
		busSyncs:    make(map[string]*busSync),
		busStarting: make(map[string]int),
		jobs:        make(map[string]*job),
		deletes:     make(map[string]*job),
		pending:     make(map[string]time.Time),
		conns:       make(map[string]*shedLink),
		idle:        make(chan struct{}),
	}
	b.rmPartial = b.removePartialClone
	b.saveRecord = b.store.SaveRecord
	b.saveMarker = b.store.SaveMarker
	b.flushedIDs = map[string]bool{}
	b.auditLog = opts.AuditLog
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

// Ready is closed once the first reconcile pass has listed the machines and
// repaired the cache. Before it the backend is degraded: reads answer from
// the records, and every call that needs the service is refused.
func (b *Backend) Ready() <-chan struct{} { return b.ready }

// WaitReady blocks until Ready, until the start-up pass finds a reason the
// server must not run (that error), or until ctx ends.
func (b *Backend) WaitReady(ctx context.Context) error {
	select {
	case <-b.ready:
		return nil
	case <-b.failed:
		return b.startErr
	case <-ctx.Done():
		return fmt.Errorf("remote backend: waiting for the first list of the machines: %w", ctx.Err())
	}
}

// fail records a reason the server must not run.
func (b *Backend) fail(err error) {
	b.failedOnce.Do(func() {
		b.startErr = err
		close(b.failed)
	})
}

// checkReady refuses a call that needs the service while the backend is
// degraded: 503 COMPUTE_UNAVAILABLE.
func (b *Backend) checkReady() error {
	select {
	case <-b.ready:
		return nil
	default:
		return computeUnavailable(ErrNotReady)
	}
}

// computeUnavailable is the 503 COMPUTE_UNAVAILABLE refusal: the service
// could not be asked, or the backend is degraded.
func computeUnavailable(err error) error {
	return &config.ComputeRefusal{Status: http.StatusServiceUnavailable, Code: config.ErrComputeUnavailable, Err: err}
}

// Catalog returns the provider's last catalog, ok false before the first
// read.
func (b *Backend) Catalog() (provider.Catalog, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.catalog, b.haveCatalog
}

// ComputeCatalog reports the catalog to the API (backend.Cataloger).
func (b *Backend) ComputeCatalog() (backend.ComputeCatalog, bool) {
	c, ok := b.Catalog()
	if !ok {
		return backend.ComputeCatalog{}, false
	}
	return computeCatalog(c), true
}

// computeCatalog is the provider's catalog as the API's type holds it.
func computeCatalog(c provider.Catalog) backend.ComputeCatalog {
	out := backend.ComputeCatalog{DefaultSize: c.DefaultSize, DefaultImage: c.DefaultImage, Images: append([]string(nil), c.Aliases...), GuestArch: c.GuestArch()}
	for _, s := range c.Sizes {
		out.Sizes = append(out.Sizes, backend.CatalogSize{Name: s.Name, CPUs: s.CPUs, MemoryMB: s.MemoryMB, Arch: s.Arch, DiskGBDefault: s.DiskGBDefault})
	}
	return out
}

// refreshCatalog reads the provider's catalog and keeps it. The first read
// logs the service clients' images the catalog does not name (S0 ruling 3).
func (b *Backend) refreshCatalog(ctx context.Context) error {
	c, err := b.prov.Catalog(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	first := !b.haveCatalog
	b.catalog, b.haveCatalog = c, true
	b.mu.Unlock()
	if first && b.opts.Server != nil && b.opts.Server.Auth != nil {
		for _, name := range slices.Sorted(maps.Keys(b.opts.Server.Auth.Clients)) {
			for _, img := range b.opts.Server.Auth.Clients[name].Policy.Images {
				if !c.HasImage(img) {
					b.logf("remote: auth.clients.%s.policy.images names %q, which the account service's catalog does not offer; that client's creates naming it are refused", name, img)
				}
			}
		}
	}
	return nil
}

// refuseLegacy is the start-up refusal of plan 041 R15: a record or a delete
// marker an earlier server wrote stops the server, naming what to do with the
// old binary. A file is an earlier server's when it lacks this build's
// schema (recognised before the rest of it is decoded, since an earlier
// shape may not decode at all), or when it carries the in-tree cloud
// provider's reference or environment.
func refuseLegacy(store *state.Store, records []*state.Record) error {
	var found []string
	stale, err := store.StaleFiles()
	if err != nil {
		return fmt.Errorf("remote: reading the state directory %s: %w", store.Dir(), err)
	}
	for _, f := range stale {
		name := f.Name
		if name == "" {
			name = filepath.Base(f.Path)
		}
		if f.Marker {
			found = append(found, fmt.Sprintf("an unfinished delete of shed %s (%s)", name, f.ID))
		} else {
			found = append(found, fmt.Sprintf("shed %s (it was written by an earlier server)", name))
		}
	}
	for _, r := range records {
		if why := r.Legacy(); why != "" {
			found = append(found, fmt.Sprintf("shed %s (%s)", r.Shed.Name, why))
		}
	}
	markers, err := store.Markers()
	if err != nil {
		return fmt.Errorf("remote: reading the delete markers in %s: %w", store.Dir(), err)
	}
	for _, m := range markers {
		if m.Legacy() {
			found = append(found, fmt.Sprintf("an unfinished delete of shed %s (%s)", m.Name, m.ID))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("remote: %w: %s. Run the previous shed-server release once more and `shed delete` each of these sheds; or, once their VMs are confirmed gone from the cloud, remove the directories sheds/ and deleting/ in %s (not the directory itself: it holds the audit log). Then start this server (see %s)",
		ErrLegacyState, strings.Join(found, "; "), store.Dir(), config.RemoteUpgradePage)
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

// shedOf returns the API view of a record: a copy, so nothing set here
// reaches the stored record.
func (b *Backend) shedOf(r *state.Record) *config.Shed {
	shed := r.Shed
	shed.Backend = config.BackendRemote
	shed.Owner, shed.User = ownership(r)
	// A remote shed mounts nothing: its selection is explicitly empty, as on
	// every backend's record a selection is reported (mount_selection). Set
	// on the copy only; the stored record carries none.
	shed.Mounts = config.SelectedMounts(nil)
	shed.Delivery = config.NewShedDelivery(r.Delivery, b.now())
	var warn *time.Duration
	if b.haveCatalog {
		w := b.catalog.Policy.Warn
		warn = &w
	}
	shed.Cloud = cloudOf(r, warn)
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

// cloudOf is a shed's cloud facts. From the machine the service last
// answered: its size, disk, image, times and lease, with the catalog's
// warning time (warn; nil when the catalog has not been read, which the
// record then leaves out). Before the first answer, what the server asked
// for: the size, the disk, the image name and the shed's creation time, with
// no lease (unknown). Every remote record carries it. Nothing of it names a
// project, a zone or an address.
func cloudOf(r *state.Record, warn *time.Duration) *config.ShedCloud {
	m := r.Machine
	if m == nil || m.Ref == "" {
		c := &config.ShedCloud{
			Size:      config.CloudSize{CPUs: r.Shed.CPUs, MemoryMB: r.Shed.MemoryMB},
			Image:     r.Shed.Image,
			CreatedAt: r.Shed.CreatedAt,
		}
		if sp := r.Spec; sp != nil {
			c.Size.Name, c.DiskGB = sp.Size, sp.DiskGB
			if c.Image == "" {
				c.Image = sp.Image
			}
		}
		return c
	}
	lease := &config.CloudLease{
		ExpiresAt:    m.Lease.ExpiresAt,
		MaxExpiresAt: m.Lease.MaxExpiresAt,
		Extendable:   m.Lease.Extendable,
		PresetsH:     slices.Clone(m.Lease.PresetsH),
	}
	if warn != nil {
		s := int64(*warn / time.Second)
		lease.WarnS = &s
	}
	c := &config.ShedCloud{
		Provider:  m.Provider,
		Size:      config.CloudSize{Name: m.Size.Name, CPUs: m.Size.CPUs, MemoryMB: m.Size.MemoryMB},
		DiskGB:    m.DiskGB,
		Image:     m.ImageResolved,
		CreatedAt: m.CreatedAt,
		Lease:     lease,
	}
	if c.Image == "" {
		c.Image = m.Image
	}
	if !m.StartedAt.IsZero() {
		t := m.StartedAt
		c.StartedAt = &t
	}
	if !m.StoppedAt.IsZero() {
		t := m.StoppedAt
		c.StoppedAt = &t
	}
	return c
}

// ownership returns the service client and user a record is reported with.
// A record that names both, or whose machine belongs to another user or
// client (Record.UserConflict), fails closed: no owner, and
// config.UserUnresolved, which no caller's user can equal.
func ownership(r *state.Record) (owner, user string) {
	if r.UserConflict != "" {
		return "", config.UserUnresolved
	}
	return config.ShedOwnership(r.Shed.Owner, r.Shed.User)
}

// busIdentity is the credential-bus identity of a record's shed: a service
// client's shed (any stored owner) stays off the bus, and the user is the
// reported one, so a record whose user is in conflict routes nowhere.
func busIdentity(r *state.Record) vmutil.BusIdentity {
	_, user := ownership(r)
	return vmutil.BusIdentity{Owned: r.Shed.Owner != "", User: user, CreatedAt: r.Shed.CreatedAt}
}

// saveLocked writes a record to disk. A failure is logged: the record in
// memory stays the working copy, and the next save retries.
func (b *Backend) saveLocked(r *state.Record) {
	if err := b.store.SaveRecord(r); err != nil {
		b.logf("remote: saving the record of shed %s: %v", r.Shed.Name, err)
	}
}

// updateDurable applies fn to a copy of the shed's record and writes the
// copy; it becomes the record in memory only once the write put it in
// place. It returns errSuperseded when the record is gone, the write's
// error when nothing was put in place (the record is unchanged), and
// state.ErrNotDurable when the copy is in place but its flush failed (the
// record then is the copy, as the disk has it).
func (b *Backend) updateDurable(name, id string, fn func(*state.Record)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.currentLocked(name, id)
	if r == nil {
		return errSuperseded
	}
	cp := r.Clone()
	fn(cp)
	err := b.saveRecord(cp)
	if err != nil && !errors.Is(err, state.ErrNotDurable) {
		return err
	}
	*r = *cp
	return err
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
	if r := b.records[name]; r != nil {
		// Its request ids are never sent again.
		if r.Mutation != nil {
			delete(b.flushedIDs, r.Mutation.RequestID)
		}
		if r.Spec != nil {
			delete(b.flushedIDs, r.Spec.RequestID)
		}
	}
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
// requirements to it: the reported owner and user and the creation time the
// access guard authorized against (backend.CheckIdentity). A reconcile that
// changed the reported user in between (a label conflict reports
// config.UserUnresolved) makes the shed not found to that caller.
func (b *Backend) lookupLocked(ctx context.Context, name string) (*state.Record, error) {
	r := b.records[name]
	if r == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	owner, user := ownership(r)
	if err := backend.CheckIdentity(ctx, name, owner, user, r.Shed.CreatedAt); err != nil {
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

// mapRefusal turns a provider error into what the API answers. A failure
// that carries a compute contract code is mapped by the code, through the
// contract's table (service.Codes), to a *config.ComputeRefusal with its
// status, code and reason; the cloud's quota and capacity keep their own
// sentinels. A failure with no code is mapped by its kind: unavailable or an
// invalid reply is COMPUTE_UNAVAILABLE. The server's own limit and a request
// the provider refused as malformed keep theirs. Other errors pass through.
func mapRefusal(err error) error {
	if err == nil {
		return nil
	}
	var cr *config.ComputeRefusal
	if errors.As(err, &cr) {
		return err
	}
	switch {
	case errors.Is(err, provider.ErrLimit):
		return fmt.Errorf("%w: %w", config.ErrServerLimitReachedSentinel, err)
	case errors.Is(err, provider.ErrQuota):
		return fmt.Errorf("%w: %w", config.ErrCloudQuotaExceededSentinel, err)
	case errors.Is(err, provider.ErrCapacity):
		return fmt.Errorf("%w: %w", config.ErrZoneCapacityUnavailableSentinel, err)
	}
	if code := provider.CodeOf(err); code != "" {
		if info, ok := service.Codes[code]; ok {
			switch info.ShedAPI.Code {
			case config.ErrShedNotFound:
				return fmt.Errorf("%w: %w", config.ErrShedNotFoundSentinel, err)
			case config.ErrShedAlreadyExists:
				return fmt.Errorf("%w: %w", config.ErrShedAlreadyExistsSentinel, err)
			case config.ErrBackendError:
				return err
			}
			return &config.ComputeRefusal{Status: info.ShedAPI.Status, Code: info.ShedAPI.Code, Reason: info.ShedAPI.Reason, Err: err}
		}
	}
	switch {
	case errors.Is(err, provider.ErrUnavailable), errors.Is(err, provider.ErrInvalidReply):
		return computeUnavailable(err)
	case errors.Is(err, provider.ErrDenied):
		return &config.ComputeRefusal{Status: http.StatusForbidden, Code: config.ErrComputeDenied, Err: err}
	case errors.Is(err, provider.ErrInvalid):
		// The service refused the request this server built (a 413, an
		// invalid body): the server's bug, not the caller's (mapping.json
		// payload_too_large and INVALID: 500 BACKEND_ERROR).
		return &config.ComputeRefusal{Status: http.StatusInternalServerError, Code: config.ErrBackendError, Err: err}
	}
	return err
}

// --- features the remote backend does not have ---

// ResetShed is not supported.
func (b *Backend) ResetShed(context.Context, string) (*config.Shed, error) {
	return nil, unsupported("reset")
}

// ListImages returns no images: a cloud image is the account service's, not
// managed through the API.
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
