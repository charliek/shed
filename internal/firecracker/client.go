//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/backend/orchestrator"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/lockmap"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/retry"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmutil"
)

// mountRetryBackoffs is the per-attempt wait schedule wrapping the
// in-guest 9P mount call. 3 attempts total, max ~2.5 s of added
// latency on the worst failure path — far cheaper than killing a
// 10 s create over a single transient agent RPC blip. Tighter than
// the registry-pull schedule (1s/4s) because mount errors fail fast
// and a slow retry just wastes wall-clock.
var mountRetryBackoffs = []time.Duration{500 * time.Millisecond, 2 * time.Second}

// Client manages Firecracker VM instances.
type Client struct {
	cfg       *config.FirecrackerConfig
	serverCfg *config.ServerConfig
	netMgr    *NetworkManager

	mu        sync.Mutex
	vms       map[string]*VM         // name -> VM
	usedCIDs  map[uint32]string      // CID -> name
	usedIPs   map[string]string      // IP -> name
	p9Servers map[string][]*P9Server // name -> active 9P servers for this VM

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

	// Credential sync
	credMgr *vmutil.CredentialManager
}

// NewClient creates a new Firecracker client.
func NewClient(cfg *config.FirecrackerConfig, serverCfg *config.ServerConfig, bridge *plugin.Bridge) (*Client, error) {
	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create network manager: %w", err)
	}

	client := &Client{
		cfg:       cfg,
		serverCfg: serverCfg,
		netMgr:    netMgr,
		vms:       make(map[string]*VM),
		usedCIDs:  make(map[uint32]string),
		usedIPs:   make(map[string]string),
		p9Servers: make(map[string][]*P9Server),
		credMgr:   vmutil.NewCredentialManager(serverCfg, bridge, string(config.BackendFirecracker), vmutil.NewHealthTracker()),
	}

	// Load existing instances to populate CID and IP maps
	if err := client.loadExistingInstances(); err != nil {
		return nil, fmt.Errorf("failed to load existing instances: %w", err)
	}

	return client, nil
}

// loadExistingInstances loads metadata for existing instances.
func (c *Client) loadExistingInstances() error {
	names, err := ListInstances(c.cfg.InstanceDir)
	if err != nil {
		return err
	}

	for _, name := range names {
		meta, err := LoadMetadata(c.cfg.InstanceDir, name)
		if err != nil {
			log.Printf("Warning: skipping instance %q with invalid metadata: %v", name, err)
			continue
		}

		c.usedCIDs[meta.CID] = name
		c.usedIPs[meta.IPAddress] = name
	}

	return nil
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
	// Stop all P9 servers for all VMs
	c.mu.Lock()
	for name := range c.p9Servers {
		for _, srv := range c.p9Servers[name] {
			if err := srv.Close(); err != nil {
				log.Printf("Warning: failed to close P9 server for %s: %v", name, err)
			}
		}
	}
	c.p9Servers = make(map[string][]*P9Server)
	c.mu.Unlock()

	c.credMgr.Close()
	return nil
}

// Config returns the Firecracker configuration.
func (c *Client) Config() *config.FirecrackerConfig {
	return c.cfg
}

// MaxVsockCID is the maximum valid CID for vsock connections.
const MaxVsockCID uint32 = 65535

// AllocateCID allocates and reserves a new CID for a VM.
func (c *Client) AllocateCID(name string) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cid := c.cfg.VsockBaseCID
	for cid <= MaxVsockCID {
		if _, used := c.usedCIDs[cid]; !used {
			c.usedCIDs[cid] = name
			return cid, nil
		}
		cid++
	}
	return 0, fmt.Errorf("all CIDs exhausted (checked %d to %d)", c.cfg.VsockBaseCID, MaxVsockCID)
}

// AllocateNetwork allocates a TAP device and IP address for a VM.
func (c *Client) AllocateNetwork(name string) (tapDevice, ipAddress string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	usedIndices := make(map[int]bool)
	gatewayIP := net.ParseIP(c.netMgr.Gateway()).To4()
	gatewayInt, err := ipToUint32(gatewayIP)
	if err != nil {
		return "", "", fmt.Errorf("invalid gateway IP: %w", err)
	}
	for ip := range c.usedIPs {
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			continue
		}
		parsedInt, err := ipToUint32(parsed)
		if err != nil {
			continue
		}
		index := int(parsedInt - gatewayInt - 1)
		if index >= 0 {
			usedIndices[index] = true
		}
	}

	index, err := c.netMgr.FindAvailableTAPIndex(usedIndices)
	if err != nil {
		return "", "", fmt.Errorf("failed to find available TAP index: %w", err)
	}
	tapDevice = c.netMgr.TAPDeviceName(index)
	ipAddress, err = c.netMgr.AllocateIP(index)
	if err != nil {
		return "", "", err
	}

	c.usedIPs[ipAddress] = name

	return tapDevice, ipAddress, nil
}

// ReleaseCID releases a reserved CID.
func (c *Client) ReleaseCID(cid uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.usedCIDs, cid)
}

// ReleaseIP releases a reserved IP address.
func (c *Client) ReleaseIP(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.usedIPs, ip)
}

// RegisterInstance registers a CID and IP as used by an instance.
func (c *Client) RegisterInstance(name string, cid uint32, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.usedCIDs[cid] = name
	c.usedIPs[ip] = name
}

// UnregisterInstance removes a CID and IP from the used maps.
func (c *Client) UnregisterInstance(name string, cid uint32, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.usedCIDs, cid)
	delete(c.usedIPs, ip)
}

// newAgentClient creates a vmutil.AgentClient for the given instance name.
func (c *Client) newAgentClient(name string) *vmutil.AgentClient {
	vsockPath := filepath.Join(c.cfg.SocketDir, fmt.Sprintf("%s.vsock", name))
	dialer := NewFirecrackerDialer(vsockPath)
	return vmutil.NewAgentClient(dialer, c.cfg.ConsolePort, c.cfg.NotifyPort)
}

// resolvePathOwner returns the UID and GID of the given path's owner.
// This is the most reliable way to determine the target UID/GID: the owner
// of the directory being shared IS the correct target, regardless of how
// the server was launched (sudo, systemd, direct).
func resolvePathOwner(hostPath string) (uid, gid int) {
	info, err := os.Stat(hostPath)
	if err != nil {
		return 1000, 1000 // fallback to default shed user
	}
	stat := info.Sys().(*syscall.Stat_t)
	uid, gid = int(stat.Uid), int(stat.Gid)
	// Root-owned directories trigger passthrough mode in NewP9Server
	// (no UID remapping), which causes credential mounts to appear as
	// root inside the guest. Fall back to the shed user UID so the
	// remapping attacher activates and the guest sees correct ownership.
	if uid == 0 {
		return 1000, 1000
	}
	return uid, gid
}

// startP9Server creates, starts, and registers a P9 server for a VM.
// It resolves the target UID/GID from the host path's owner for remapping.
func (c *Client) startP9Server(name, bridgeIP, hostPath, mountPath string, readOnly bool) (*P9Server, error) {
	targetUID, targetGID := resolvePathOwner(hostPath)
	srv, err := NewP9Server(bridgeIP, hostPath, mountPath, readOnly, targetUID, targetGID)
	if err != nil {
		return nil, fmt.Errorf("create P9 server for %s: %w", hostPath, err)
	}
	srv.Start()

	c.mu.Lock()
	c.p9Servers[name] = append(c.p9Servers[name], srv)
	c.mu.Unlock()

	return srv, nil
}

// stopP9Servers stops all P9 servers for a VM and removes them from the map.
// This is nil-safe: after server restart, loadExistingInstances does not
// restore P9 servers, so this may find an empty/nil slice.
func (c *Client) stopP9Servers(name string) {
	c.mu.Lock()
	servers := c.p9Servers[name]
	delete(c.p9Servers, name)
	c.mu.Unlock()

	for _, srv := range servers {
		if err := srv.Close(); err != nil {
			log.Printf("Warning: failed to close P9 server for %s: %v", name, err)
		}
	}
}

// mount9PInGuest executes the mount-9p subcommand inside the guest VM via the
// agent exec channel. The mount command runs with sudo since syscall.Mount
// requires root privileges.
func (c *Client) mount9PInGuest(ctx context.Context, agent *vmutil.AgentClient, serverAddr, target string, readOnly bool, tag string) error {
	cmd := []string{"sudo", "/usr/local/bin/shed-agent", "mount-9p",
		"--addr", serverAddr,
		"--target", target,
		"--tag", tag,
	}
	if readOnly {
		cmd = append(cmd, "--readonly")
	}

	opts := backend.ExecOptions{
		Cmd:    cmd,
		Stdout: vmutil.NopWriteCloser(os.Stderr), // mount output goes to server stderr for debugging
		Stderr: vmutil.NopWriteCloser(os.Stderr),
		TTY:    false,
	}
	if err := agent.Exec(ctx, opts); err != nil {
		return fmt.Errorf("mount-9p exec failed for %s at %s: %w", serverAddr, target, err)
	}
	return nil
}

// mount9PInGuestWithRetry wraps mount9PInGuest in a bounded retry
// envelope. Used by the create/start paths where a transient agent
// RPC blip during the mount would otherwise kill an entire 10-second
// VM bring-up. Credential mounts (mount9PCredentialFunc) don't go
// through here because the credential manager already
// log-and-continues on failure — extra latency before that log isn't
// a useful tradeoff.
func (c *Client) mount9PInGuestWithRetry(ctx context.Context, agent *vmutil.AgentClient, serverAddr, target string, readOnly bool, tag string) error {
	return retry.Do(ctx, "mount 9p "+tag, mountRetryBackoffs, nil, func() error {
		return c.mount9PInGuest(ctx, agent, serverAddr, target, readOnly, tag)
	})
}

// metadataToShed converts Firecracker metadata to a config.Shed.
// mount9PCredentialFunc returns a DirMountFunc that binds the shed name for
// P9 server bookkeeping. The credential name from SetupCredentials is used
// for mount tags, while shedName is used for p9Servers map registration so
// that stopP9Servers(shedName) correctly finds and cleans up all servers.
func (c *Client) mount9PCredentialFunc(shedName string) vmutil.DirMountFunc {
	return func(ctx context.Context, agent *vmutil.AgentClient, credName string, mount config.MountConfig) error {
		bridgeIP := c.netMgr.Gateway()
		srv, err := c.startP9Server(shedName, bridgeIP, mount.Source, mount.Target, mount.ReadOnly)
		if err != nil {
			return fmt.Errorf("start 9P server for credential %q: %w", credName, err)
		}

		tag := config.CredentialMountTag(credName)
		if err := c.mount9PInGuest(ctx, agent, srv.Addr(), mount.Target, mount.ReadOnly, tag); err != nil {
			// Clean up the P9 server to avoid leaked listeners
			srv.Close()
			// Remove from the p9Servers slice
			c.mu.Lock()
			servers := c.p9Servers[shedName]
			for i, s := range servers {
				if s == srv {
					c.p9Servers[shedName] = append(servers[:i], servers[i+1:]...)
					break
				}
			}
			c.mu.Unlock()
			return fmt.Errorf("mount 9P credential %q: %w", credName, err)
		}

		return nil
	}
}

func metadataToShed(meta *Metadata) *config.Shed {
	return &config.Shed{
		Name:           meta.Name,
		Status:         meta.Status,
		CreatedAt:      meta.CreatedAt,
		Repo:           meta.Repo,
		ContainerID:    fmt.Sprintf("fc-%s", meta.Name),
		Backend:        meta.Backend,
		IPAddress:      meta.IPAddress,
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

// CreateShed creates a new Firecracker-based shed.
//
// This wrapper handles the parts the orchestrator does NOT own
// (per-shed-name locking, existence-check, orphan-upper sweep, the
// --image vs --from-snapshot exclusivity check). Per-step lifecycle
// logic lives in fcCreator (see orchestrator.go) and is invoked by
// orchestrator.CreateShed.
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
		// Same safety guard as VZ — see internal/vz/client.go for the
		// CodeRabbit-flagged regression history.
		return nil, fmt.Errorf("failed to read metadata for %s: %w", req.Name, err)
	}

	// Sweep an orphan upper from a previously crashed create. We're
	// guaranteed at this point that no metadata.json claims this name
	// (the LoadMetadata check above returned NotFound) and we hold the
	// per-name create lock, so anything still sitting at
	// uppers/<name>/ is leftover state from a half-completed run.
	if _, err := os.Stat(UpperPath(c.cfg.UppersDir, req.Name)); err == nil {
		log.Printf("CreateShed %s: sweeping orphan upper from a prior crashed create", req.Name)
		if err := DeleteUpper(c.cfg.UppersDir, req.Name); err != nil {
			return nil, fmt.Errorf("failed to sweep orphan upper for %s: %w", req.Name, err)
		}
	}

	return orchestrator.CreateShed(ctx, &fcCreator{c: c}, req)
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

	shed := metadataToShed(meta)
	shed.Status = status

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
			c.stopP9Servers(name)
			c.mu.Lock()
			delete(c.vms, name)
			c.mu.Unlock()
			if meta.PID > 0 {
				if !isFirecrackerProcess(meta.PID) {
					log.Printf("Warning: PID %d is not a Firecracker process, skipping SIGKILL during delete of %s", meta.PID, name)
				} else {
					_ = syscall.Kill(meta.PID, syscall.SIGKILL)
					if !waitForProcessExit(meta.PID, 2*time.Second) {
						log.Printf("Warning: PID %d did not exit within timeout during delete of %s", meta.PID, name)
					}
				}
			}
		}
	}

	if err := c.netMgr.DeleteTAPDevice(meta.TAPDevice); err != nil {
		log.Printf("Warning: failed to delete TAP device %s: %v", meta.TAPDevice, err)
	}

	c.UnregisterInstance(name, meta.CID, meta.IPAddress)

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
// behavior lives in fcStarter; see internal/backend/orchestrator/
// start.go for the lifecycle contract.
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	defer c.acquireCreateLock(name)()
	return orchestrator.StartShed(ctx, &fcStarter{c: c}, orchestrator.StartRequest{Name: name})
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

	// Stop P9 servers before shutting down
	c.stopP9Servers(meta.Name)

	agent := c.newAgentClient(meta.Name)

	// Run shutdown hook before stopping the VM
	vmutil.RunShutdownSequence(ctx, agent, meta.Name, meta.LandingDir, c.cfg.StopTimeout.Duration(), os.Stdout, os.Stderr)

	// Ask the guest to flush dirty buffers to the virtio-blk devices
	// before firecracker terminates. Without this, post-stop snapshots
	// and any host-side read of upper.ext4 see a stale pre-sync state.
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
	// and returns nil even when firecracker refused to die. Verify
	// before flipping status — otherwise the next StartShed would find
	// PID=0 in metadata and silently spawn a second firecracker under
	// the same name.
	if meta.PID > 0 && vmutil.IsProcessAlive(meta.PID) && isFirecrackerProcess(meta.PID) {
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

	return metadataToShed(meta), nil
}

// DialService opens a TCP connection to a service port inside a running Firecracker shed.
// Firecracker VMs have routable bridge IPs, so this dials directly.
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

	addr := net.JoinHostPort(meta.IPAddress, strconv.FormatUint(uint64(port), 10))
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// ResetShed nukes the per-shed upper and recreates it as a fresh
// sparse, unformatted ext4-sized file. The shed must be stopped.
// Project mounts (--local-dir / --add-dir) are mounted post-boot via
// 9P from outside the overlay so they are not affected by this
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

	// Refuse to wipe the upper if the lower blob is gone — the shed
	// is unbootable either way, but `shed reset` succeeding without
	// the lower would silently destroy uncommitted in-VM work for a
	// shed that can't be started afterwards. Operators want a clear
	// "pull the image first" signal.
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
	return metadataToShed(meta), nil
}
