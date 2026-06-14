//go:build darwin
// +build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/backend/orchestrator"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/lockmap"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/retry"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmutil"
)

// mountRetryBackoffs is the per-attempt wait schedule wrapping the
// VirtioFS mount call. 3 attempts total, max ~2.5 s of added latency
// on the worst failure path — far cheaper than killing a 10 s create
// over a single transient agent RPC blip.
var mountRetryBackoffs = []time.Duration{500 * time.Millisecond, 2 * time.Second}

// Client manages VZ VM instances.
type Client struct {
	cfg       *config.VZConfig
	serverCfg *config.ServerConfig

	mu  sync.Mutex
	vms map[string]*VM // name -> VM

	// shedLocks serializes per-shed-name lifecycle operations. Originally
	// added for CreateShed-vs-CreateShed TOCTOU but the lock has broadened
	// to cover any operation that mutates a shed's on-disk state (Create,
	// Start, Stop, Delete, and CreateSnapshot of this shed as source).
	// Value type so the zero-value Client{} works in tests.
	shedLocks lockmap.NamedMutexMap

	// snapshotLocks serializes CreateSnapshot/DeleteSnapshot/CreateShed-
	// from-snapshot calls by snapshot name. Distinct keyspace from
	// shedLocks (snapshot names vs shed names).
	snapshotLocks lockmap.NamedMutexMap

	credMgr *vmutil.CredentialManager

	// egressMgr drives the optional egress-control proxy. nil ⇒ egress
	// disabled, in which case the ConfigureEgressProxy hook is a no-op.
	egressMgr *egress.Manager
}

// SetEgressManager attaches the egress-control proxy manager, called by
// shed-server at startup when egress is enabled. nil leaves egress off.
func (c *Client) SetEgressManager(m *egress.Manager) { c.egressMgr = m }

// NewClient creates a new VZ client.
func NewClient(cfg *config.VZConfig, serverCfg *config.ServerConfig, bridge *plugin.Bridge) (*Client, error) {
	if runtime.GOARCH != "arm64" {
		return nil, fmt.Errorf("vz backend currently supports macOS Apple Silicon (arm64) only")
	}

	client := &Client{
		cfg:       cfg,
		serverCfg: serverCfg,
		vms:       make(map[string]*VM),
		credMgr:   vmutil.NewCredentialManager(serverCfg, bridge, string(config.BackendVZ), vmutil.NewHealthTracker()),
	}

	return client, nil
}

// acquireCreateLock returns an unlock closure after taking the per-shed-name
// lifecycle mutex. Callers MUST defer the returned closure.
//
// Originally added to serialize CreateShed-vs-CreateShed for the same name,
// the lock has since broadened to cover any operation that mutates a shed's
// on-disk state (Create, Start, Stop, Delete, and CreateSnapshot of this
// shed as source). This closes TOCTOU races between, e.g., a snapshot of a
// stopped shed and a concurrent Start of the same shed.
//
// Lock-order rule: when both locks are needed (snapshot create or
// from-snapshot spawn), acquire snapshotLock BEFORE createLock to avoid
// AB-BA deadlock with other code paths.
func (c *Client) acquireCreateLock(name string) func() {
	return c.shedLocks.Acquire(name)
}

// Close closes the client and releases resources.
func (c *Client) Close() error {
	c.credMgr.Close()
	return nil
}

// newAgentClient creates a vmutil.AgentClient for the given instance name.
func (c *Client) newAgentClient(name string) *vmutil.AgentClient {
	dialer := NewVZDialer(c.cfg.SocketDir, name)
	return vmutil.NewAgentClient(dialer, c.cfg.ConsolePort, c.cfg.NotifyPort)
}

// preserveConsoleLog copies the failing shed's console.log into a sibling
// `logs/` directory before the caller wipes the instance dir. Safe to
// call on a failure path that doesn't yet know if vfkit ever started:
// missing logs are logged at debug-level and ignored.
func (c *Client) preserveConsoleLog(meta *Metadata) {
	dest, err := meta.PreserveConsoleLog(c.cfg.InstanceDir, filepath.Join(filepath.Dir(c.cfg.InstanceDir), "logs"))
	if err != nil {
		log.Printf("Warning: failed to preserve console log for %s: %v", meta.Name, err)
		return
	}
	if dest != "" {
		log.Printf("Preserved console log for failed shed %s at %s", meta.Name, dest)
	}
}

// CreateShed creates a new VZ-based shed.
//
// This wrapper handles the parts the orchestrator does NOT own
// (per-shed-name locking, existence-check, orphan-upper sweep, the
// --image vs --from-snapshot exclusivity check). Per-step lifecycle
// logic lives in vzCreator (see orchestrator.go) and is invoked by
// orchestrator.CreateShed. See `internal/backend/orchestrator/create.go`
// for the BackendCreator contract.
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}

	if req.FromSnapshot != "" {
		if req.Image != "" || req.Repo != "" {
			return nil, fmt.Errorf("%w: --from-snapshot cannot be combined with --image or --repo", config.ErrInvalidShedRequestSentinel)
		}
	}

	// Lock-order: snapshotLock(source) BEFORE createLock(new shed). This
	// blocks DeleteSnapshot of the source between loadSnapshot and the
	// reflink read, and matches CreateSnapshot's order so no AB-BA cycle.
	if req.FromSnapshot != "" {
		defer c.acquireSnapshotLock(req.FromSnapshot)()
	}

	// Serialize CreateShed calls for the same name so the existence
	// check below and CopyRootfs's os.Remove(dst) can't race two
	// concurrent creates into corrupting the first caller's rootfs.
	defer c.acquireCreateLock(req.Name)()

	if _, err := LoadMetadata(c.cfg.InstanceDir, req.Name); err == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	} else if !errors.Is(err, ErrInstanceNotFound) {
		// Non-not-found errors (corrupt metadata, I/O, permission)
		// must propagate — falling through here would sweep the
		// existing upper and overwrite a possibly-real shed. The
		// pre-orchestrator code silently fell through; CodeRabbit's
		// review of PR #139 flagged this. Loud failure is correct.
		return nil, fmt.Errorf("failed to read metadata for %s: %w", req.Name, err)
	}

	// Sweep an orphan upper from a previously crashed create. We're
	// guaranteed at this point that no metadata.json claims this name
	// (the LoadMetadata check above returned NotFound) and we hold the
	// per-name create lock, so anything still sitting at
	// uppers/<name>/ is leftover state from a half-completed run.
	// Without this, EnsureUpper's strict-rejection would force the
	// operator to manually `rm -rf` to recover.
	if _, err := os.Stat(UpperPath(c.cfg.UppersDir, req.Name)); err == nil {
		log.Printf("CreateShed %s: sweeping orphan upper from a prior crashed create", req.Name)
		if err := DeleteUpper(c.cfg.UppersDir, req.Name); err != nil {
			return nil, fmt.Errorf("failed to sweep orphan upper for %s: %w", req.Name, err)
		}
	}

	return orchestrator.CreateShed(ctx, &vzCreator{c: c}, req)
}

// GetShed returns a shed by name.
func (c *Client) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}

	status := meta.Status
	if status == config.StatusRunning {
		vm := &VM{meta: meta, cfg: c.cfg}
		if !vm.IsRunning() {
			meta.Status = config.StatusStopped
			meta.PID = 0
			if err := meta.Save(c.cfg.InstanceDir); err != nil {
				log.Printf("Warning: failed to save updated metadata for %q: %v", name, err)
			}
			status = config.StatusStopped
			// Clean up stale health state for the stopped VM
			if ht := c.credMgr.HealthTracker(); ht != nil {
				ht.Remove(name)
			}
		}
	}

	var ipAddress string
	if status == config.StatusRunning {
		ipAddress = "127.0.0.1"
	}

	shed := metadataToShed(meta, ipAddress)
	shed.Status = status // may differ from meta.Status after staleness check

	// Populate health data from in-memory tracker for running VMs.
	if status == config.StatusRunning {
		if ht := c.credMgr.HealthTracker(); ht != nil {
			if hs, ok := ht.Get(name); ok {
				shed.LastHealthy = &hs.LastSeen
				shed.StartedAt = &hs.AgentStartedAt
				if len(hs.Extensions) > 0 {
					shed.Extensions = make(map[string]config.ExtensionHealthInfo, len(hs.Extensions))
					for ns, eh := range hs.Extensions {
						shed.Extensions[ns] = config.ExtensionHealthInfo{Guest: eh.Guest, Host: eh.Host}
					}
				}
			}
		}
	}

	return shed, nil
}

// ListSheds returns all sheds.
func (c *Client) ListSheds(ctx context.Context) ([]config.Shed, error) {
	names, err := ListInstances(c.cfg.InstanceDir)
	if err != nil {
		return nil, err
	}

	var sheds []config.Shed
	for _, name := range names {
		shed, err := c.GetShed(ctx, name)
		if err != nil {
			log.Printf("Warning: skipping invalid shed %q: %v", name, err)
			continue
		}
		sheds = append(sheds, *shed)
	}

	return sheds, nil
}

// DeleteShed removes a shed.
func (c *Client) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return err
	}

	if meta.Status == config.StatusRunning {
		// Use the lock-aware variant so we don't deadlock on the per-shed
		// mutex we already hold (sync.Mutex is non-reentrant).
		if _, err := c.stopShedLocked(ctx, meta); err != nil {
			log.Printf("Warning: stop failed during delete of %s: %v", name, err)
			// stopShedLocked failed — clean up resources it would have released
			c.credMgr.StopListener(name)
			c.mu.Lock()
			delete(c.vms, name)
			c.mu.Unlock()
			if meta.PID > 0 {
				if !isVfkitProcess(meta.PID) {
					log.Printf("Warning: PID %d is not a vfkit process, skipping SIGKILL during delete of %s", meta.PID, name)
				} else {
					_ = syscall.Kill(meta.PID, syscall.SIGKILL)
					if !waitForProcessExit(meta.PID, 2*time.Second) {
						log.Printf("Warning: PID %d did not exit within timeout during delete of %s", meta.PID, name)
					}
				}
			}
		}
	}

	// Free this shed's egress port reservation. Stop only closes the listener
	// and keeps the port reserved (for restart); delete frees it. No-op when
	// egress is disabled or this shed had none.
	if c.egressMgr != nil {
		_ = c.egressMgr.Release(name)
	}

	if err := meta.Delete(c.cfg.InstanceDir); err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	if err := DeleteUpper(c.cfg.UppersDir, name); err != nil {
		log.Printf("Warning: failed to delete upper layer for %s: %v", name, err)
	}

	return nil
}

// StartShed starts a stopped shed via the BackendStarter orchestrator.
// The per-shed create lock is acquired here (the orchestrator does
// not own it — same contract as CreateShed). All platform-specific
// behavior lives in vzStarter; see internal/backend/orchestrator/
// start.go for the lifecycle contract.
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	defer c.acquireCreateLock(name)()
	return orchestrator.StartShed(ctx, &vzStarter{c: c}, orchestrator.StartRequest{Name: name})
}

// StopShed stops a running shed.
func (c *Client) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}

	return c.stopShedLocked(ctx, meta)
}

// stopShedLocked performs the stop logic assuming the caller already holds
// acquireCreateLock(meta.Name). This split exists so DeleteShed can stop a
// running shed without re-entering the non-reentrant per-shed mutex it is
// already holding. The public StopShed acquires the lock and delegates here.
func (c *Client) stopShedLocked(ctx context.Context, meta *Metadata) (*config.Shed, error) {
	if meta.Status != config.StatusRunning {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotRunningSentinel, meta.Name)
	}

	// Stop notification listener before shutting down
	c.credMgr.StopListener(meta.Name)

	// Close this shed's egress proxy listener (frees the per-shed port). No-op
	// when egress is disabled or this shed has none. Never touches host docker.
	if c.egressMgr != nil {
		if err := c.egressMgr.Remove(meta.Name); err != nil {
			log.Printf("Warning: failed to remove egress listener for %s: %v", meta.Name, err)
		}
	}

	agent := c.newAgentClient(meta.Name)

	// Run shutdown hook before stopping the VM
	vmutil.RunShutdownSequence(ctx, agent, meta.Name, meta.LandingDir, c.cfg.StopTimeout.Duration(), os.Stdout, os.Stderr)

	// Ask the guest to flush its dirty buffers to the virtio-blk
	// devices before vfkit terminates. Without this, post-stop
	// snapshots and any host-side read of upper.ext4 see a stale
	// pre-sync state — vfkit does not synchronously flush on SIGTERM.
	vmutil.SyncFilesystems(ctx, agent, c.cfg.StopTimeout.Duration())

	// Get or create VM handle
	c.mu.Lock()
	vm := c.vms[meta.Name]
	c.mu.Unlock()

	if vm == nil {
		vm = &VM{meta: meta, cfg: c.cfg}
	}

	if err := vm.Stop(ctx); err != nil {
		return nil, fmt.Errorf("failed to stop VM: %w", err)
	}

	// vm.Stop's post-SIGKILL waitForProcessExit swallows its timeout
	// and returns nil even when vfkit refused to die (uninterruptible
	// I/O, kernel hang, etc). Verify before flipping status — otherwise
	// the next StartShed would find PID=0 in metadata and silently
	// spawn a second vfkit under the same name.
	if meta.PID > 0 && vmutil.IsProcessAlive(meta.PID) && isVfkitProcess(meta.PID) {
		return nil, fmt.Errorf("%w: %s (pid %d)", config.ErrStopIncompleteSentinel, meta.Name, meta.PID)
	}

	meta.Status = config.StatusStopped
	meta.PID = 0
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	c.mu.Lock()
	delete(c.vms, meta.Name)
	c.mu.Unlock()

	return metadataToShed(meta, ""), nil
}

// DialService opens a TCP connection to a service port inside a running VZ shed.
// It dials the vsock TCP proxy port and performs a CONNECT handshake.
func (c *Client) DialService(ctx context.Context, name string, port uint16) (net.Conn, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}
	if meta.Status != config.StatusRunning {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotRunningSentinel, name)
	}

	dialer := NewVZDialer(c.cfg.SocketDir, name)
	return dialer.DialService(ctx, c.cfg.TCPProxyPort, port)
}

// SetShedEgress applies a new egress profile selection to a shed. On a running
// shed the listener is re-pushed and the guest env re-injected live; on a
// stopped shed it persists and applies on next start. A selection that resolves
// to nothing (e.g. "off") clears egress.
func (c *Client) SetShedEgress(ctx context.Context, name string, profiles []string) (*config.Shed, error) {
	if c.egressMgr == nil {
		return nil, fmt.Errorf("egress control is not enabled on this server")
	}
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}

	specs, err := c.serverCfg.Egress.ResolveProfiles(profiles)
	if err != nil {
		return nil, fmt.Errorf("egress: resolve profiles: %w", err)
	}
	if len(specs) == 0 {
		return c.clearShedEgressLocked(ctx, meta)
	}

	if meta.Status == config.StatusRunning {
		gateway, subnet := c.egressGatewaySubnet()
		agent := c.newAgentClient(name)
		port, token, err := vmutil.ApplyEgressLive(ctx, c.egressMgr, agent, name, meta.EgressPort, meta.EgressToken, gateway, subnet, specs)
		if err != nil {
			return nil, fmt.Errorf("egress: apply: %w", err)
		}
		meta.EgressPort = port
		meta.EgressToken = token
	}
	meta.EgressProfiles = profiles
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("egress: save metadata: %w", err)
	}
	return c.GetShed(ctx, name)
}

// ClearShedEgress turns egress off for a shed (live on a running shed).
func (c *Client) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	if c.egressMgr == nil {
		return nil, fmt.Errorf("egress control is not enabled on this server")
	}
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}
	return c.clearShedEgressLocked(ctx, meta)
}

func (c *Client) clearShedEgressLocked(ctx context.Context, meta *Metadata) (*config.Shed, error) {
	if meta.Status == config.StatusRunning {
		agent := c.newAgentClient(meta.Name)
		_ = vmutil.ClearEgressLive(ctx, c.egressMgr, agent, meta.Name)
	} else {
		_ = c.egressMgr.Release(meta.Name) // egress off → free the port reservation
	}
	meta.EgressProfiles = nil
	meta.EgressPort = 0
	meta.EgressToken = ""
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("egress: save metadata: %w", err)
	}
	return c.GetShed(ctx, meta.Name)
}

// metadataToShed converts VZ metadata to a config.Shed response.
func metadataToShed(meta *Metadata, ipAddress string) *config.Shed {
	return &config.Shed{
		Name:           meta.Name,
		Status:         meta.Status,
		CreatedAt:      meta.CreatedAt,
		Repo:           meta.Repo,
		ContainerID:    fmt.Sprintf("vz-%s", meta.Name),
		Backend:        meta.Backend,
		IPAddress:      ipAddress,
		CPUs:           meta.CPUs,
		MemoryMB:       meta.MemoryMB,
		PID:            meta.PID,
		RootfsPath:     meta.RootfsPath,
		ProjectMounts:  meta.ProjectMounts,
		LandingDir:     meta.LandingDir,
		Image:          meta.Image,
		ImageDigest:    meta.LowerDigest,
		FromSnapshot:   meta.FromSnapshot,
		EgressProfiles: meta.EgressProfiles,
		EgressPort:     meta.EgressPort,
		EgressToken:    meta.EgressToken,
	}
}

// mountVirtioFSShare mounts a VirtioFS share inside the guest VM.
// Apple's VirtioFS transparently maps UIDs, so no chown is needed.
func (c *Client) mountVirtioFSShare(ctx context.Context, agent *vmutil.AgentClient, mountTag, target string, readOnly bool) error {
	mountOpts := "rw"
	if readOnly {
		mountOpts = "ro"
	}
	// Use positional parameters to avoid shell interpolation of target/tag values.
	mountCmd := `modprobe virtiofs 2>/dev/null; mkdir -p "$1" && mount -t virtiofs -o "$2" "$3" "$1"`
	opts := backend.ExecOptions{
		Cmd:    []string{"sudo", "sh", "-c", mountCmd, "sh", target, mountOpts, mountTag},
		Stdout: vmutil.NopWriteCloser(io.Discard),
		Stderr: vmutil.NopWriteCloser(os.Stderr),
		TTY:    false,
	}
	if err := agent.Exec(ctx, opts); err != nil {
		return fmt.Errorf("virtiofs mount failed for %s at %s: %w", mountTag, target, err)
	}
	return nil
}

// mountVirtioFSShareWithRetry wraps mountVirtioFSShare in a bounded
// retry envelope. Used by the create/start paths where a transient
// agent RPC blip during the mount would otherwise kill an entire
// 10-second VM bring-up. Credential mounts (mountVirtioFSCredential)
// don't go through here because the credential manager already
// log-and-continues on failure — extra latency before that log isn't
// a useful tradeoff.
func (c *Client) mountVirtioFSShareWithRetry(ctx context.Context, agent *vmutil.AgentClient, mountTag, target string, readOnly bool) error {
	return retry.Do(ctx, "mount virtiofs "+mountTag, mountRetryBackoffs, nil, func() error {
		return c.mountVirtioFSShare(ctx, agent, mountTag, target, readOnly)
	})
}

// buildCredentialShares creates VirtioFS share entries from classified credentials.
func buildCredentialShares(creds map[string]config.MountConfig) []credentialVirtioFS {
	shares := make([]credentialVirtioFS, 0, len(creds))
	for name, mount := range creds {
		shares = append(shares, credentialVirtioFS{
			SourceDir: mount.Source,
			MountTag:  config.CredentialMountTag(name),
		})
	}
	return shares
}

// mountVirtioFSCredential mounts a single directory credential via VirtioFS.
// This implements vmutil.DirMountFunc for the VZ backend.
func (c *Client) mountVirtioFSCredential(ctx context.Context, agent *vmutil.AgentClient, name string, mount config.MountConfig) error {
	mountTag := config.CredentialMountTag(name)
	return c.mountVirtioFSShare(ctx, agent, mountTag, mount.Target, mount.ReadOnly)
}

// ResetShed nukes the per-shed upper and recreates it as a fresh
// sparse, unformatted ext4-sized file. The shed must be stopped.
// Project mounts (--local-dir / --add-dir) are mounted post-boot via
// VirtioFS from outside the overlay so they are not affected by this
// operation; the home directory (on the upper) is wiped.
func (c *Client) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}
	if meta.Status != config.StatusStopped {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotStoppedSentinel, name)
	}

	// Refuse to wipe the upper if the lower blob is gone — see the
	// matching guard in firecracker/client.go for rationale.
	if meta.LowerDigest != "" && !vmimage.BlobExists(c.cfg.ImagesDir, meta.LowerDigest) {
		ref := meta.LowerImageTag
		if ref == "" {
			ref = "(unknown)"
		}
		return nil, fmt.Errorf("cannot reset %s: lower image blob %s is no longer cached; pull or rebuild %s before resetting",
			name, vmimage.ShortDigest(meta.LowerDigest), ref)
	}

	size := meta.UpperSizeBytes
	if size <= 0 {
		sz := c.cfg.UpperSizeDefault
		if sz == "" {
			sz = config.DefaultUpperSize
		}
		parsed, perr := config.ParseUpperSize(sz)
		if perr != nil {
			return nil, fmt.Errorf("invalid upper_size_default during reset: %w", perr)
		}
		size = parsed
	}

	if err := DeleteUpper(c.cfg.UppersDir, name); err != nil {
		return nil, fmt.Errorf("failed to delete upper during reset: %w", err)
	}
	upperPath, err := EnsureUpper(c.cfg.UppersDir, name, size)
	if err != nil {
		return nil, fmt.Errorf("failed to recreate upper during reset: %w", err)
	}

	meta.UpperPath = upperPath
	meta.RootfsPath = upperPath
	meta.UpperSizeBytes = size
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("failed to persist metadata after reset: %w", err)
	}

	log.Printf("reset shed %s: upper recreated at %s (%d bytes)", name, upperPath, size)
	return metadataToShed(meta, ""), nil
}
