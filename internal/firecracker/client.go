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

	// egressMgr drives the optional egress-control proxy. nil ⇒ egress
	// disabled, in which case the ConfigureEgressProxy hook is a no-op.
	egressMgr *egress.Manager

	// egressUserStore is the runtime user-profile store; nil ⇒ config profiles
	// only. Merged into resolution via userProfiles().
	egressUserStore *config.UserProfileStore

	// procCmdline reads the given PID's command line. Overridden in tests so
	// the resume liveness predicate can be exercised against synthetic
	// processes (vanished PIDs, reused PIDs, wedged reads).
	procCmdline func(ctx context.Context, pid int) (string, error)
}

// SetEgressManager attaches the egress-control proxy manager, called by
// shed-server at startup when egress is enabled. nil leaves egress off.
func (c *Client) SetEgressManager(m *egress.Manager) { c.egressMgr = m }

// SetEgressUserStore attaches the runtime user-profile store (mirrors
// SetEgressManager). nil ⇒ config profiles only.
func (c *Client) SetEgressUserStore(s *config.UserProfileStore) { c.egressUserStore = s }

// userProfiles snapshots the user-profile store for resolution (nil-safe).
func (c *Client) userProfiles() map[string]config.EgressProfile { return c.egressUserStore.List() }

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

		procCmdline: readProcCmdline,
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

// resumeInspectTimeout bounds a single process inspection during the startup
// resume walk. One unreadable or wedged process must never stall the rest of
// the walk. Shortened by tests; 2 s is the production default.
var resumeInspectTimeout = 2 * time.Second

// ResumeRunningInstances re-opens the per-VM message channel for every shed
// that is still running, after a shed-server restart (#315).
//
// The host dials the guest, never the reverse, and the only callers of
// SetupCredentials are the orchestrator's create and start steps — so a
// restart used to leave every running shed silently credential-broken (no
// plugin-bridge registration, no extension health) until it was stopped and
// started by hand.
//
// The walk never rewrites metadata: a running-but-dead record is skipped and
// left to GetShed's staleness path. It only spawns NotifyConn goroutines,
// which dial with backoff, so it is safe to run in the background.
func (c *Client) ResumeRunningInstances(ctx context.Context) {
	names, err := ListInstances(c.cfg.InstanceDir)
	if err != nil {
		log.Printf("Resume: failed to list instances: %v", err)
		return
	}

	for _, name := range names {
		if ctx.Err() != nil {
			log.Printf("Resume: walk cancelled before %q", name)
			return
		}
		c.resumeInstance(ctx, name)
	}
}

// resumeInstance resumes a single shed's message channel. Split out of the
// walk so the per-shed lifecycle lock is released on every iteration.
func (c *Client) resumeInstance(ctx context.Context, name string) {
	// Serialize against a concurrent StartShed/StopShed/DeleteShed of the
	// same shed, and load the metadata fresh under that lock — a pre-walk
	// snapshot could already be stale by the time we get here.
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		log.Printf("Resume: skipping %q with unreadable metadata: %v", name, err)
		return
	}
	if meta.Status != config.StatusRunning || meta.PID <= 0 {
		return
	}

	inspectCtx, cancel := context.WithTimeout(ctx, resumeInspectTimeout)
	defer cancel()

	alive, err := c.vmmServesInstance(inspectCtx, meta.PID, name)
	if err != nil {
		// Timed-out or unreadable inspection: skip this one and let the walk
		// carry on. A vanished PID lands here too, which is the desired
		// "running metadata, dead VM" outcome.
		log.Printf("Resume: skipping %q: could not inspect pid %d: %v", name, meta.PID, err)
		return
	}
	if !alive {
		return
	}

	c.credMgr.ResumeMessageChannel(name, c.newAgentClient(name))
}

// vmmServesInstance reports whether pid is a live Firecracker VMM serving
// THIS instance. Family alone ("is it some firecracker?") is not enough for
// resume: a recycled PID that happens to be another shed's firecracker would
// resurrect a dead record, so the instance's own api-sock argument has to be
// there too.
//
// The predicate itself lives in vm.go (vmmCmdlineServesInstance) because
// every pid-fallback path — Stop, Kill/delete, the zombie and
// stop-incomplete guards, IsRunning — has to ask the same question; this
// wrapper only adds the resume walk's cancellable, injectable cmdline read.
func (c *Client) vmmServesInstance(ctx context.Context, pid int, name string) (bool, error) {
	cmdline, err := c.inspectCmdline(ctx, pid)
	if err != nil {
		return false, err
	}
	return vmmCmdlineServesInstance(cmdline, instanceAPISocketPath(c.cfg.SocketDir, name)), nil
}

// inspectCmdline reads a PID's command line via the injected reader,
// falling back to the real one for zero-value Clients.
func (c *Client) inspectCmdline(ctx context.Context, pid int) (string, error) {
	read := c.procCmdline
	if read == nil {
		read = readProcCmdline
	}
	return read(ctx, pid)
}

// readProcCmdline reads /proc/<pid>/cmdline. The contents are NUL-separated
// argv, which is fine for the substring checks the resume predicate makes.
// A missing or unreadable process returns an error ⇒ not alive.
//
// The read runs on its own goroutine so the context can actually cut it
// short. os.ReadFile is not cancellable, and reading this file takes the
// target task's mmap_lock — a VMM wedged in uninterruptible sleep can hold
// that, which is precisely the case the walk's per-inspection timeout exists
// to survive. Without the goroutine the timeout would be decorative here and
// the wedged-inspection test would be passing only because its fake reader is
// more interruptible than the real one.
//
// A read that never returns leaks this goroutine and its fd for the life of
// the process. That is the deliberate trade, and it is bounded: the resume
// walk runs ONCE per server start and inspects each instance at most once, so
// the worst case is one parked goroutine per running-shed record on a host
// where every /proc read wedges — not unbounded growth over time. A stalled
// resume walk, by contrast, costs every shed behind it its credentials.
func readProcCmdline(ctx context.Context, pid int) (string, error) {
	// Checked up front as well as in the select below. The select alone is a
	// race in principle — Go picks uniformly among ready cases, so an already
	// cancelled context could still lose to a completed read — even though in
	// practice it wins (200 runs with this check removed stayed green, because
	// the reader goroutine has not been scheduled yet at select time). This
	// turns "happens to win" into a guarantee.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1) // buffered: the goroutine never blocks on a timed-out read
	go func() {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		done <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if r.err != nil {
			return "", r.err
		}
		return string(r.data), nil
	}
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

// Close closes the client and releases resources. It runs only at daemon exit.
//
// Every shed's 9P servers are closed here even though their VMs keep running
// (KillMode=process): that is the documented #369 exception to "close a shed's
// 9P servers only once its VMM is gone". The guest's 9P transport ends with
// the daemon process regardless, so force-closing the connections first
// changes nothing for the guest; it only keeps the daemon's own shutdown from
// waiting out a drain per mount per running shed.
//
// The servers are copied into a fresh snapshot under c.mu and closed (in
// parallel) after releasing it. A server registered after the snapshot — a
// create racing daemon exit — dies with the process.
func (c *Client) Close() error {
	c.mu.Lock()
	var servers []*P9Server
	for _, set := range c.p9Servers {
		servers = append(servers, set...)
	}
	c.p9Servers = make(map[string][]*P9Server)
	c.mu.Unlock()

	closeP9ServerSet(servers)

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
//
// Callers close a shed's servers only once its VMM is verifiably gone (see
// vmmTeardown), when a failed mount is unwinding, or on DeleteShed's pid-0
// fallback (nothing to verify): Close force-closes the guest's connections,
// so closing them under a live guest kills its mounts.
func (c *Client) stopP9Servers(name string) {
	c.mu.Lock()
	servers := c.p9Servers[name]
	delete(c.p9Servers, name)
	c.mu.Unlock()

	closeP9ServerSet(servers)
}

// closeP9ServerSet closes servers concurrently and waits for all of them, so a
// shed with N mounts closes in the time of its slowest server rather than the
// sum (each Close is bounded by its drain backstop). Errors are logged per
// server and never returned: a close failure leaves nothing to retry.
func closeP9ServerSet(servers []*P9Server) {
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *P9Server) {
			defer wg.Done()
			if err := srv.Close(); err != nil {
				log.Printf("Warning: failed to close P9 server for %s: %v", srv.hostPath, err)
			}
		}(srv)
	}
	wg.Wait()
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

// egressGatewaySubnet returns the gateway IP + subnet CIDR the guest reaches the
// host (proxy) on. For FC these come from the bridge network manager.
func (c *Client) egressGatewaySubnet() (gateway, subnet string) {
	return c.netMgr.Gateway(), c.cfg.BridgeCIDR
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

	specs, err := c.serverCfg.Egress.ResolveProfiles(profiles, c.userProfiles())
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
		running, verifyErr := vm.IsRunning()
		switch {
		case verifyErr != nil:
			// UNKNOWN identity (see isThisVMsProcess): the pid is alive but
			// we could not read whose it is. Keep the recorded status —
			// rewriting it to Stopped here would strip a live shed's PID and
			// let the next start spawn a second firecracker over it.
			log.Printf("Warning: GetShed %q: cannot verify pid %d, keeping recorded status: %v", name, meta.PID, verifyErr)
		case !running:
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
func (c *Client) DeleteShed(ctx context.Context, name string) error {
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return err
	}

	if meta.Status == config.StatusRunning {
		backend.Status(ctx, "Terminating virtual machine...")
		// Use the lock-aware variant so we don't deadlock on the per-shed
		// mutex we already hold (sync.Mutex is non-reentrant).
		if _, err := c.stopShedLocked(ctx, meta, stopDestroy); err != nil {
			log.Printf("Warning: stop failed during delete of %s: %v", name, err)
			// stopShedLocked failed — clean up resources it would have released.
			// The 9P servers are NOT among them: they close below, once the
			// VMM is established gone, and stay up with everything else when
			// it is not.
			c.credMgr.StopListener(name)
			c.mu.Lock()
			delete(c.vms, name)
			c.mu.Unlock()
			// EXCEPTION, pid <= 0: with no recorded pid there is nothing to
			// verify, so — as before #284 — a failed teardown of a pid-0
			// VMM (vm.Kill's StopVMM fallback erroring) still falls through
			// and releases the TAP and the upper, and now also closes its
			// 9P servers. "9P servers close only once the VMM is gone" holds
			// on every verifiable (pid > 0) path below.
			if meta.PID > 0 {
				// Pin the pid before asking whose it is, then signal through
				// that handle (pidfd on Linux) — never by number after a
				// separate check.
				vm := &VM{meta: meta, cfg: c.cfg}
				proc, owns, verifyErr := vm.recordedPIDHandle()
				switch {
				case verifyErr != nil:
					// UNKNOWN identity: refuse to carry on. Everything below
					// this branch releases the TAP, the CID/IP reservation, the
					// upper and the 9P servers, which would strand a VMM that
					// may well still be running.
					return fmt.Errorf("cannot verify pid %d while deleting %s; refusing to release its resources: %w", meta.PID, name, verifyErr)
				case !owns:
					log.Printf("Warning: PID %d is not %s's firecracker VMM (recycled pid?), skipping SIGKILL during delete of %s", meta.PID, name, name)
				default:
					_ = proc.Signal(syscall.SIGKILL)
					if !waitForProcessExit(meta.PID, 2*time.Second) {
						// The VMM survived SIGKILL. Refuse, exactly as for
						// UNKNOWN identity: releasing its TAP, reservations,
						// upper and 9P servers would strand a live VMM. A
						// retried delete succeeds once it is gone.
						return fmt.Errorf("%w: %s (pid %d did not exit after SIGKILL); refusing to release its resources", config.ErrStopIncompleteSentinel, name, meta.PID)
					}
				}
			}
		}
	}

	// Every path that reaches here releases the shed: Stopped metadata, a
	// Running teardown that succeeded, or a fallback whose SIGKILL landed
	// (or that found the recorded pid recycled, or unset — the pid-0
	// exception above; both released before too). Close the shed's 9P
	// servers immediately before its TAP/CID/IP/upper go. A Stopped shed
	// can still hold servers: an unexpected VMM exit that GetShed
	// reconciled to Stopped never closed them.
	c.stopP9Servers(name)

	backend.Status(ctx, "Releasing network...")
	if err := c.netMgr.DeleteTAPDevice(meta.TAPDevice); err != nil {
		log.Printf("Warning: failed to delete TAP device %s: %v", meta.TAPDevice, err)
	}

	c.UnregisterInstance(name, meta.CID, meta.IPAddress)

	// Free this shed's egress port reservation. Stop only closes the listener
	// and keeps the port reserved (for restart); delete frees it. No-op when
	// egress is disabled or this shed had none.
	if c.egressMgr != nil {
		_ = c.egressMgr.Release(name)
	}

	backend.Status(ctx, "Removing volume...")
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
// stopMode selects stopShedLocked's teardown: stopGraceful is StopShed's clean,
// restartable path; stopDestroy is delete's fast SIGKILL path (see the
// stopDestroy case for the project-mount sync exception). stopGraceful is the
// zero value, so a forgotten mode defaults to the safe graceful path.
type stopMode int

const (
	stopGraceful stopMode = iota
	stopDestroy
)

func (c *Client) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}

	return c.stopShedLocked(ctx, meta, stopGraceful)
}

// stopShedLocked performs the stop logic assuming the caller already holds
// acquireCreateLock(meta.Name). This split exists so DeleteShed can stop a
// running shed without re-entering the non-reentrant per-shed mutex it is
// already holding. The public StopShed acquires the lock and delegates here.
//
// The shed's 9P servers outlive the guest work (#284): the shutdown hook and
// the sync run with the host mounts still live, and the servers close only
// once the VMM is verifiably gone — whatever vm.Stop / vm.Kill returned. On a
// path where the VMM may still be alive they stay registered (its mounts keep
// working, metadata still says Running), and the next stop or delete closes
// them.
func (c *Client) stopShedLocked(ctx context.Context, meta *Metadata, mode stopMode) (*config.Shed, error) {
	if meta.Status != config.StatusRunning {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotRunningSentinel, meta.Name)
	}

	// The pid the teardown targets, pinned before anything below can touch
	// the metadata. The post-teardown verdict inspects exactly this pid.
	pid := meta.PID
	vmmGone := false
	defer func() {
		// Runs on every return once the verdict is in, including a
		// meta.Save failure after the VMM is already gone.
		if vmmGone {
			c.stopP9Servers(meta.Name)
		}
	}()

	// Stop notification listener before shutting down
	c.credMgr.StopListener(meta.Name)

	// Close this shed's egress proxy listener (frees the per-shed port). No-op
	// when egress is disabled or this shed has none. Never touches host docker.
	if c.egressMgr != nil {
		if err := c.egressMgr.Remove(meta.Name); err != nil {
			log.Printf("Warning: failed to remove egress listener for %s: %v", meta.Name, err)
		}
	}

	// Get or create VM handle
	c.mu.Lock()
	vm := c.vms[meta.Name]
	c.mu.Unlock()

	if vm == nil {
		vm = &VM{meta: meta, cfg: c.cfg}
	}

	var teardownErr error
	switch mode {
	case stopGraceful:
		agent := c.newAgentClient(meta.Name)
		// Run the shutdown hook, then ask the guest to flush dirty buffers to
		// the virtio-blk devices before firecracker terminates. Without this,
		// post-stop snapshots and any host-side read of upper.ext4 see a stale
		// pre-sync state. Both run while the 9P servers are still up, so a
		// hook that writes to a host mount, and the sync's flush of it, land.
		vmutil.RunShutdownSequence(ctx, agent, meta.Name, meta.LandingDir, c.cfg.StopTimeout.Duration(), os.Stdout, os.Stderr)
		vmutil.SyncFilesystems(ctx, agent, c.cfg.StopTimeout.Duration())
		if err := vm.Stop(ctx); err != nil {
			teardownErr = fmt.Errorf("failed to stop VM: %w", err)
		}
	case stopDestroy:
		// Delete discards the upper, so the shutdown hook and the graceful VM
		// shutdown are wasted work — SIGKILL immediately. EXCEPTION: if the shed
		// has any WRITABLE host-backed mount (--local-dir/--add-dir OR a
		// configured server `mounts:` entry), flush the guest (bounded) first so
		// a raw kill doesn't lose unsynced writes to that host data. A shed with
		// no writable host mount skips the agent entirely (the fast path).
		if config.HasWritableHostMount(meta.ProjectMounts, c.serverCfg) {
			agent := c.newAgentClient(meta.Name)
			vmutil.SyncFilesystems(ctx, agent, c.cfg.StopTimeout.Duration())
		}
		if err := vm.Kill(ctx); err != nil {
			teardownErr = fmt.Errorf("failed to kill VM: %w", err)
		}
	}

	// One liveness/identity probe, after the teardown whatever it returned.
	// It decides both whether the 9P servers may close and the post-check
	// below — the pid is never inspected twice.
	verdict := c.vmmTeardown(meta.Name, pid, teardownErr)
	vmmGone = verdict.gone
	if teardownErr != nil {
		return nil, teardownErr
	}

	// The VM teardown above (vm.Stop or vm.Kill) swallows its post-SIGKILL
	// waitForProcessExit timeout and returns nil even when firecracker refused
	// to die. Verify before flipping status — otherwise the next StartShed
	// would find PID=0 in metadata and silently spawn a second firecracker
	// under the same name.
	if verdict.verifyErr != nil {
		// UNKNOWN identity: do not flip the metadata to Stopped/PID=0.
		// The caller (stop or delete) must fail loudly instead of
		// releasing resources a live VMM may still hold.
		return nil, fmt.Errorf("cannot verify pid %d after stopping %s: %w", pid, meta.Name, verdict.verifyErr)
	}
	if verdict.alive {
		return nil, fmt.Errorf("%w: %s (pid %d)", config.ErrStopIncompleteSentinel, meta.Name, pid)
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

// vmmVerdict is what one post-teardown probe of a shed's recorded VMM pid
// established.
type vmmVerdict struct {
	// gone: the VMM is verifiably gone, so the shed's 9P servers may close.
	gone bool
	// alive: the pid is live and is still this shed's VMM.
	alive bool
	// verifyErr: the pid is live but whose process it is could not be read
	// (UNKNOWN identity, see isThisVMsProcess). Never "gone".
	verifyErr error
}

// vmmTeardown probes a shed's VMM after vm.Stop / vm.Kill returned
// teardownErr — whatever it returned — and reports whether it is gone.
//
//   - pid > 0: gone iff the process is dead, or alive but definitively not
//     this shed's VMM (a recycled pid). UNKNOWN identity is not gone.
//   - pid <= 0: there is no pid to inspect, and the SDK handle's own
//     StopVMM / machine.Wait path is the only evidence: gone iff the
//     teardown returned nil.
//
// stopShedLocked and the start/create rollback (registerStopVMCleanups) share
// it so both close a shed's 9P servers on exactly the same evidence.
func (c *Client) vmmTeardown(name string, pid int, teardownErr error) vmmVerdict {
	if pid <= 0 {
		return vmmVerdict{gone: teardownErr == nil}
	}
	if !vmutil.IsProcessAlive(pid) {
		return vmmVerdict{gone: true}
	}
	owns, err := isThisVMsProcess(pid, instanceAPISocketPath(c.cfg.SocketDir, name))
	switch {
	case err != nil:
		return vmmVerdict{verifyErr: err}
	case owns:
		return vmmVerdict{alive: true}
	default:
		return vmmVerdict{gone: true}
	}
}

// DialService opens a connection to a service port inside a running Firecracker
// shed by routing through the guest agent's TCP proxy on vsock, exactly like VZ.
//
// Historically this dialed the VM's routable bridge IP directly. That could not
// reach guest services bound to 127.0.0.1 (e.g. the rc hub on 127.0.0.1:1029),
// so those degraded to RC_HUB_UNAVAILABLE on Firecracker. Routing through the
// tcpproxy (the guest agent dials the target on 127.0.0.1) gives parity with VZ:
// connect/{port} now reaches loopback-bound guest services, and the peer address
// the guest sees becomes 127.0.0.1. Services bound only to the bridge IP become
// unreachable via connect/{port} — none exist in-tree; sshd and user services
// bind 0.0.0.0, which the proxy reaches via loopback all the same.
//
// Semantic delta vs the old bridge dial: half-close/EOF does not propagate
// through the tcpproxy until full close (guest-side tcpproxy CloseWrite asserts
// *net.TCPConn, which fails for the vsock.Conn it holds). This matches VZ; a
// guest-side fix is deferred (it needs an image rebuild).
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

	vsockPath := filepath.Join(c.cfg.SocketDir, fmt.Sprintf("%s.vsock", name))
	dialer := NewFirecrackerDialer(vsockPath)
	return vmutil.DialService(ctx, dialer, c.cfg.TCPProxyPort, port)
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
