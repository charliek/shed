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
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/lockmap"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmimage/clone"
	"github.com/charliek/shed/internal/vmutil"
)

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
}

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

// CreateShed creates a new VZ-based shed.
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

	// Serialize CreateShed calls for the same name so the existence check
	// below and CopyRootfs's os.Remove(dst) can't race two concurrent
	// creates into corrupting the first caller's rootfs.
	defer c.acquireCreateLock(req.Name)()

	if _, err := LoadMetadata(c.cfg.InstanceDir, req.Name); err == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}

	cpus := req.CPUs
	if cpus == 0 {
		cpus = c.cfg.DefaultCPUs
	}
	if cpus < 1 || cpus > config.MaxVZCPUs {
		return nil, fmt.Errorf("invalid cpus %d: must be between 1 and %d", cpus, config.MaxVZCPUs)
	}
	memoryMB := req.MemoryMB
	if memoryMB == 0 {
		memoryMB = c.cfg.DefaultMemoryMB
	}
	if memoryMB < 128 || memoryMB > config.MaxVZMemoryMB {
		return nil, fmt.Errorf("invalid memory_mb %d: must be between 128 and %d", memoryMB, config.MaxVZMemoryMB)
	}

	upperSizeBytes := req.UpperSizeBytes
	if upperSizeBytes == 0 {
		sz := c.cfg.UpperSizeDefault
		if sz == "" {
			sz = config.DefaultUpperSize
		}
		parsed, perr := config.ParseUpperSize(sz)
		if perr != nil {
			return nil, fmt.Errorf("invalid upper_size_default: %w", perr)
		}
		upperSizeBytes = parsed
	} else {
		if upperSizeBytes < config.MinUpperSizeBytes || upperSizeBytes > config.MaxUpperSizeBytes {
			return nil, fmt.Errorf("upper size out of range: must be between %dG and %dG",
				config.MinUpperSizeBytes/(1024*1024*1024), config.MaxUpperSizeBytes/(1024*1024*1024))
		}
	}

	var snapshotUpperSource, lowerDigest, lowerImageTag string
	if req.FromSnapshot != "" {
		snap, err := loadSnapshot(c.cfg.SnapshotsDir, req.FromSnapshot)
		if err != nil {
			return nil, err
		}
		if snap.Backend != config.BackendVZ {
			return nil, fmt.Errorf("%w: snapshot %q is for backend %q, server is %q",
				config.ErrSnapshotBackendMismatchSentinel, req.FromSnapshot, snap.Backend, config.BackendVZ)
		}
		if snap.LowerDigest == "" {
			return nil, fmt.Errorf("snapshot %q is missing lower_digest; recreate the snapshot from a current shed", req.FromSnapshot)
		}
		if !vmimage.BlobExists(c.cfg.ImagesDir, snap.LowerDigest) {
			ref := snap.SourceImage
			if ref == "" {
				ref = "(unknown)"
			}
			return nil, fmt.Errorf("snapshot %q references lower digest %s which is no longer cached; pull the original image (%s) first",
				req.FromSnapshot, vmimage.ShortDigest(snap.LowerDigest), ref)
		}
		snapshotUpperSource = SnapshotRootfsPath(c.cfg.SnapshotsDir, req.FromSnapshot)
		lowerDigest = snap.LowerDigest
		lowerImageTag = snap.SourceImage
	} else {
		var resolved config.ResolvedImage
		if req.Image != "" {
			var err error
			resolved, err = c.cfg.ResolveImage(req.Image)
			if err != nil {
				return nil, err
			}
		} else {
			resolved = c.cfg.ResolveBaseRootfs()
		}

		// Ensure image is available locally (pulls + converts Docker refs if needed).
		// We only need the digest — the rootfs path is rederived per-boot inside vm.go.
		_, ldigest, err := EnsureImage(ctx, resolved, c.cfg)
		if err != nil {
			return nil, err
		}
		if ldigest == "" {
			return nil, fmt.Errorf("image %q resolved to a path outside the blob store; the overlay model requires content-addressed images", resolved.Name)
		}
		lowerDigest = ldigest
		lowerImageTag = resolved.Name
	}

	backend.Progress(ctx, "rootfs", "Allocating writable upper layer...")
	upperPath, err := EnsureUpper(c.cfg.UppersDir, req.Name, upperSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create upper: %w", err)
	}
	if snapshotUpperSource != "" {
		// Replace the freshly allocated empty upper with a clone of the
		// snapshot's stored upper so the new shed inherits its parent's
		// writable contents.
		if err := os.Remove(upperPath); err != nil && !os.IsNotExist(err) {
			_ = DeleteUpper(c.cfg.UppersDir, req.Name)
			return nil, fmt.Errorf("failed to clear upper for snapshot clone: %w", err)
		}
		if _, err := clone.CloneFile(snapshotUpperSource, upperPath); err != nil {
			_ = DeleteUpper(c.cfg.UppersDir, req.Name)
			return nil, fmt.Errorf("failed to clone snapshot upper: %w", err)
		}
		if err := os.Chmod(upperPath, 0o644); err != nil {
			_ = DeleteUpper(c.cfg.UppersDir, req.Name)
			return nil, fmt.Errorf("failed to chmod cloned upper: %w", err)
		}
		// The metadata's UpperSizeBytes must reflect the *cloned* file's
		// actual size, not the freshly-allocated sparse size — otherwise
		// `shed system df` and reset/snapshot bookkeeping report the
		// pre-snapshot size for what is now a different file.
		if fi, statErr := os.Stat(upperPath); statErr == nil {
			upperSizeBytes = fi.Size()
		}
	}
	rootfsPath := upperPath

	meta := &Metadata{
		Name:           req.Name,
		Status:         config.StatusStopped,
		CreatedAt:      time.Now(),
		Backend:        config.BackendVZ,
		CPUs:           cpus,
		MemoryMB:       memoryMB,
		RootfsPath:     rootfsPath,
		UpperPath:      upperPath,
		UpperSizeBytes: upperSizeBytes,
		Repo:           req.Repo,
		LocalDir:       req.LocalDir,
		Image:          req.Image,
		LowerDigest:    lowerDigest,
		LowerImageTag:  lowerImageTag,
		FromSnapshot:   req.FromSnapshot,
	}

	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		if rmErr := DeleteUpper(c.cfg.UppersDir, req.Name); rmErr != nil {
			log.Printf("Warning: failed to delete upper for %s: %v", req.Name, rmErr)
		}
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Filter credentials to those with existing source directories.
	// Non-existent sources are skipped to avoid vfkit VirtioFS failures.
	dirCreds := vmutil.FilterExistingCredentials(c.serverCfg)

	vm := CreateVM(meta, c.cfg)
	vm.credentialShares = buildCredentialShares(dirCreds)

	backend.Progress(ctx, "vm", "Starting virtual machine...")
	if err := vm.Start(ctx); err != nil {
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		if rmErr := DeleteUpper(c.cfg.UppersDir, req.Name); rmErr != nil {
			log.Printf("Warning: failed to delete upper for %s: %v", req.Name, rmErr)
		}
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}

	meta.Status = config.StatusRunning
	meta.PID = vm.meta.PID
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if stopErr := vm.Stop(context.Background()); stopErr != nil {
			log.Printf("Warning: failed to stop VM: %v", stopErr)
		}
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		if rmErr := DeleteUpper(c.cfg.UppersDir, req.Name); rmErr != nil {
			log.Printf("Warning: failed to delete upper for %s: %v", req.Name, rmErr)
		}
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	c.mu.Lock()
	c.vms[req.Name] = vm
	c.mu.Unlock()

	agent := c.newAgentClient(meta.Name)

	// Mount VirtioFS workspace if local dir is configured
	if req.LocalDir != "" {
		backend.Progress(ctx, "mount", "Mounting local directory via VirtioFS...")
		if err := c.mountVirtioFSShare(ctx, agent, config.VirtioFSMountTag, config.WorkspacePath, false); err != nil {
			// VirtioFS mount is essential for --local-dir; fail the create
			if stopErr := vm.Stop(context.Background()); stopErr != nil {
				log.Printf("Warning: failed to stop VM after mount failure: %v", stopErr)
			}
			return nil, fmt.Errorf("VirtioFS mount failed for local dir %s: %w", req.LocalDir, err)
		}
	}

	// Mount credentials
	c.credMgr.SetupCredentials(ctx, agent, req.Name, dirCreds, c.mountVirtioFSCredential)

	// Clone repo if specified (skip when using local dir or spawning from snapshot;
	// snapshot rootfs is already provisioned).
	if req.Repo != "" && req.LocalDir == "" && req.FromSnapshot == "" {
		backend.Progress(ctx, "repo", "Cloning repository...")
		if err := vmutil.CloneRepo(ctx, agent, c.serverCfg, req.Repo); err != nil {
			log.Printf("Warning: failed to clone repo %s: %v", config.SanitizeRepoURL(req.Repo), err)
			// Generic SSE message by design: req.Repo can carry credentials
			// (e.g., https://user:pw@host/...) and the wrapped err from
			// git/ssh may include the URL too. Full detail is in the server
			// log above; SSE consumers get a stable, sanitized signal.
			backend.ProgressWarning(ctx, "repo", "Failed to clone repository (see server logs for details)")
		} else {
			backend.Progress(ctx, "repo", "Repository cloned")
		}
	}

	// Run provisioning. When spawning from a snapshot the rootfs is already
	// provisioned, so we skip the one-shot install hook but still run the
	// startup hook on every boot — matching StartShed's behavior. This means
	// a snapshot-spawned shed gets the same first-boot startup hook as if
	// the operator had started it manually.
	if !req.NoProvision {
		provisioner := vmutil.NewProvisioner(agent, req.Name)
		provisioner.SetOutput(os.Stdout, os.Stderr)
		cfg, err := provisioner.LoadConfig(ctx)
		if err != nil {
			log.Printf("Warning: failed to load provisioning config: %v", err)
		} else {
			runInstall := req.FromSnapshot == ""
			if err := provisioner.RunProvisioning(ctx, cfg, runInstall); err != nil {
				log.Printf("Warning: provisioning failed: %v", err)
			}
		}
	}

	return metadataToShed(meta, "127.0.0.1"), nil
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

	if err := meta.Delete(c.cfg.InstanceDir); err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	if err := DeleteUpper(c.cfg.UppersDir, name); err != nil {
		log.Printf("Warning: failed to delete upper layer for %s: %v", name, err)
	}

	return nil
}

// StartShed starts a stopped shed.
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	defer c.acquireCreateLock(name)()

	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return nil, err
	}

	if meta.Status == config.StatusRunning {
		vm := &VM{meta: meta, cfg: c.cfg}
		if vm.IsRunning() {
			return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyRunningSentinel, name)
		}
		meta.Status = config.StatusStopped
		meta.PID = 0
	}

	// Filter credentials to those with existing source directories.
	dirCreds := vmutil.FilterExistingCredentials(c.serverCfg)

	vm := CreateVM(meta, c.cfg)
	vm.credentialShares = buildCredentialShares(dirCreds)

	if err := vm.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}

	meta.Status = config.StatusRunning
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if stopErr := vm.Stop(context.Background()); stopErr != nil {
			log.Printf("Warning: failed to stop VM: %v", stopErr)
		}
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	c.mu.Lock()
	c.vms[name] = vm
	c.mu.Unlock()

	agent := c.newAgentClient(meta.Name)

	// Re-mount VirtioFS workspace on start (mount doesn't persist across reboots)
	if meta.LocalDir != "" {
		if err := c.mountVirtioFSShare(ctx, agent, config.VirtioFSMountTag, config.WorkspacePath, false); err != nil {
			// VirtioFS mount is essential for --local-dir; stop the VM and fail
			if stopErr := vm.Stop(context.Background()); stopErr != nil {
				log.Printf("Warning: failed to stop VM after mount failure: %v", stopErr)
			}
			return nil, fmt.Errorf("VirtioFS mount failed on start for local dir %s: %w", meta.LocalDir, err)
		}
	}

	// Mount credentials
	c.credMgr.SetupCredentials(ctx, agent, name, dirCreds, c.mountVirtioFSCredential)

	// Run startup hook only (not install)
	provisioner := vmutil.NewProvisioner(agent, name)
	provisioner.SetOutput(os.Stdout, os.Stderr)
	cfg, err := provisioner.LoadConfig(ctx)
	if err != nil {
		log.Printf("Warning: failed to load provisioning config: %v", err)
	} else {
		if err := provisioner.RunProvisioning(ctx, cfg, false); err != nil {
			log.Printf("Warning: startup hook failed: %v", err)
		}
	}

	return metadataToShed(meta, "127.0.0.1"), nil
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

	// Run shutdown hook before stopping the VM
	vmutil.RunShutdownSequence(ctx, c.newAgentClient(meta.Name), meta.Name, c.cfg.StopTimeout.Duration(), os.Stdout, os.Stderr)

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

// GetNetworkEndpoint returns the network endpoint for a shed.
// VZ uses NAT, so the endpoint is always localhost.
func (c *Client) GetNetworkEndpoint(ctx context.Context, name string) (string, error) {
	_, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return "", fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return "", err
	}

	return "127.0.0.1", nil
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

// metadataToShed converts VZ metadata to a config.Shed response.
func metadataToShed(meta *Metadata, ipAddress string) *config.Shed {
	return &config.Shed{
		Name:         meta.Name,
		Status:       meta.Status,
		CreatedAt:    meta.CreatedAt,
		Repo:         meta.Repo,
		ContainerID:  fmt.Sprintf("vz-%s", meta.Name),
		Backend:      meta.Backend,
		IPAddress:    ipAddress,
		CPUs:         meta.CPUs,
		MemoryMB:     meta.MemoryMB,
		PID:          meta.PID,
		RootfsPath:   meta.RootfsPath,
		LocalDir:     meta.LocalDir,
		Image:        meta.Image,
		FromSnapshot: meta.FromSnapshot,
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
// /workspace is mounted post-boot via VirtioFS from outside the
// overlay so it is not affected by this operation.
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
