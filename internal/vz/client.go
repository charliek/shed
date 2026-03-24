//go:build darwin
// +build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
)

// Client manages VZ VM instances.
type Client struct {
	cfg       *config.VZConfig
	serverCfg *config.ServerConfig

	mu  sync.Mutex
	vms map[string]*VM // name -> VM

	// Credential sync
	credWatcher     *vmutil.CredentialWatcher                   // host-side fsnotify watcher
	notifyListeners map[string]*vmutil.CredentialNotifyListener // name -> per-VM notification listener
}

// NewClient creates a new VZ client.
func NewClient(cfg *config.VZConfig, serverCfg *config.ServerConfig) (*Client, error) {
	if runtime.GOARCH != "arm64" {
		return nil, fmt.Errorf("vz backend currently supports macOS Apple Silicon (arm64) only")
	}

	client := &Client{
		cfg:             cfg,
		serverCfg:       serverCfg,
		vms:             make(map[string]*VM),
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}

	// Start host-side credential watcher for bidirectional sync
	if serverCfg != nil && len(serverCfg.Credentials) > 0 {
		client.credWatcher = vmutil.NewCredentialWatcher(serverCfg)
		if err := client.credWatcher.Start(context.Background()); err != nil {
			log.Printf("Warning: failed to start credential watcher: %v", err)
			client.credWatcher = nil
		}
	}

	return client, nil
}

// Close closes the client and releases resources.
func (c *Client) Close() error {
	// Stop all notification listeners
	c.mu.Lock()
	for name, nl := range c.notifyListeners {
		nl.Stop()
		delete(c.notifyListeners, name)
	}
	c.mu.Unlock()

	// Stop the credential watcher
	if c.credWatcher != nil {
		c.credWatcher.Stop()
	}

	return nil
}

// newAgentClient creates a vmutil.AgentClient for the given instance name.
func (c *Client) newAgentClient(name string) *vmutil.AgentClient {
	dialer := NewVZDialer(c.cfg.SocketDir, name)
	return vmutil.NewAgentClient(dialer, c.cfg.ConsolePort, c.cfg.HealthPort, c.cfg.NotifyPort)
}

// CreateShed creates a new VZ-based shed.
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}

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

	rootfsSource := c.cfg.BaseRootfs
	if req.Image != "" {
		resolved, err := c.cfg.ResolveImage(req.Image)
		if err != nil {
			return nil, err
		}
		rootfsSource = resolved
	}

	backend.Progress(ctx, "rootfs", "Copying root filesystem...")
	rootfsPath, err := CopyRootfs(rootfsSource, c.cfg.InstanceDir, req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to copy rootfs: %w", err)
	}

	meta := &Metadata{
		Name:       req.Name,
		Status:     config.StatusStopped,
		CreatedAt:  time.Now(),
		Backend:    config.BackendVZ,
		CPUs:       cpus,
		MemoryMB:   memoryMB,
		RootfsPath: rootfsPath,
		Repo:       req.Repo,
		LocalDir:   req.LocalDir,
		Image:      req.Image,
	}

	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Classify credentials before VM creation so VirtioFS devices are
	// included in the vfkit command-line arguments at launch time.
	var virtioFSCreds, tarOnlyCreds map[string]config.MountConfig
	if c.serverCfg != nil && len(c.serverCfg.Credentials) > 0 {
		virtioFSCreds, tarOnlyCreds = classifyCredentials(c.serverCfg.Credentials)
	}

	vm := CreateVM(meta, c.cfg)
	vm.credentialShares = buildCredentialShares(virtioFSCreds)

	backend.Progress(ctx, "vm", "Starting virtual machine...")
	if err := vm.Start(ctx); err != nil {
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
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

	// Mount and transfer credentials
	c.setupCredentials(ctx, agent, req.Name, virtioFSCreds, tarOnlyCreds)

	// Clone repo if specified (skip when using local dir)
	if req.Repo != "" && req.LocalDir == "" {
		backend.Progress(ctx, "repo", "Cloning repository...")
		if err := c.cloneRepo(ctx, agent, req.Repo); err != nil {
			log.Printf("Warning: failed to clone repo %s: %v", req.Repo, err)
		}
	}

	// Run provisioning
	if !req.NoProvision {
		provisioner := vmutil.NewProvisioner(agent, req.Name)
		provisioner.SetOutput(os.Stdout, os.Stderr)
		cfg, err := provisioner.LoadConfig(ctx)
		if err != nil {
			log.Printf("Warning: failed to load provisioning config: %v", err)
		} else {
			if err := provisioner.RunProvisioning(ctx, cfg, true); err != nil {
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
		}
	}

	var ipAddress string
	if status == config.StatusRunning {
		ipAddress = "127.0.0.1"
	}

	shed := metadataToShed(meta, ipAddress)
	shed.Status = status // may differ from meta.Status after staleness check
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
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return err
	}

	if meta.Status == config.StatusRunning {
		if _, err := c.StopShed(ctx, name); err != nil {
			log.Printf("Warning: stop failed during delete of %s: %v", name, err)
			// StopShed failed — clean up resources it would have released
			c.stopNotifyListener(name)
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

	return nil
}

// StartShed starts a stopped shed.
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
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

	// Classify credentials before VM creation so VirtioFS devices are
	// included in the vfkit command-line arguments at launch time.
	var virtioFSCreds, tarOnlyCreds map[string]config.MountConfig
	if c.serverCfg != nil && len(c.serverCfg.Credentials) > 0 {
		virtioFSCreds, tarOnlyCreds = classifyCredentials(c.serverCfg.Credentials)
	}

	vm := CreateVM(meta, c.cfg)
	vm.credentialShares = buildCredentialShares(virtioFSCreds)

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

	// Mount and transfer credentials
	c.setupCredentials(ctx, agent, name, virtioFSCreds, tarOnlyCreds)

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

	// Stop notification listener before shutting down
	c.stopNotifyListener(name)

	// Run shutdown hook before stopping the VM
	stopTimeout := c.cfg.StopTimeout.Duration()
	hookBudget := stopTimeout / 2
	if hookBudget > 30*time.Second {
		hookBudget = 30 * time.Second
	}

	agent := c.newAgentClient(meta.Name)
	provisioner := vmutil.NewProvisioner(agent, name)
	provisioner.SetOutput(os.Stdout, os.Stderr)

	hookCtx, hookCancel := context.WithTimeout(ctx, hookBudget)
	defer hookCancel()
	provCfg, err := provisioner.LoadConfig(hookCtx)
	if err != nil {
		log.Printf("Warning: failed to load provision config for shutdown hook: %v", err)
	} else if provCfg.HasShutdownHook() {
		provisioner.RunShutdownHook(hookCtx, provCfg)
	}

	// Get or create VM handle
	c.mu.Lock()
	vm := c.vms[name]
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
	delete(c.vms, name)
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

// metadataToShed converts VZ metadata to a config.Shed response.
func metadataToShed(meta *Metadata, ipAddress string) *config.Shed {
	return &config.Shed{
		Name:        meta.Name,
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("vz-%s", meta.Name),
		Backend:     meta.Backend,
		IPAddress:   ipAddress,
		CPUs:        meta.CPUs,
		MemoryMB:    meta.MemoryMB,
		PID:         meta.PID,
		RootfsPath:  meta.RootfsPath,
		LocalDir:    meta.LocalDir,
		Image:       meta.Image,
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

// classifyCredentials splits credentials into VirtioFS-eligible (directories)
// and tar-only (single files). Missing sources are logged and skipped.
func classifyCredentials(credentials map[string]config.MountConfig) (virtioFS map[string]config.MountConfig, tarOnly map[string]config.MountConfig) {
	virtioFS = make(map[string]config.MountConfig)
	tarOnly = make(map[string]config.MountConfig)

	for name, mount := range credentials {
		info, err := os.Stat(mount.Source)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("Credential %q source does not exist, skipping: %s", name, mount.Source)
			} else {
				log.Printf("Warning: failed to stat credential %q source %s: %v", name, mount.Source, err)
			}
			continue
		}

		if info.IsDir() {
			virtioFS[name] = mount
		} else {
			tarOnly[name] = mount
		}
	}

	return virtioFS, tarOnly
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

// setupCredentials mounts VirtioFS credential shares, transfers tar-only credentials,
// and starts the notify listener if needed.
func (c *Client) setupCredentials(ctx context.Context, agent *vmutil.AgentClient, shedName string, virtioFSCreds, tarOnlyCreds map[string]config.MountConfig) {
	// Mount VirtioFS credential shares (directory credentials)
	if len(virtioFSCreds) > 0 {
		backend.Progress(ctx, "credentials", "Mounting credentials via VirtioFS...")
		if err := c.mountAllCredentialVirtioFS(ctx, agent, virtioFSCreds); err != nil {
			log.Printf("Warning: VirtioFS credential mount failed: %v", err)
		}
	}

	// Transfer single-file credentials via tar (VirtioFS only shares directories)
	if len(tarOnlyCreds) > 0 {
		backend.Progress(ctx, "credentials", "Transferring file credentials...")
		credTransfer := vmutil.NewCredentialTransfer(agent, c.serverCfg)
		for name, mount := range tarOnlyCreds {
			if err := credTransfer.TransferCredential(ctx, name, mount); err != nil {
				log.Printf("Warning: failed to transfer credential %q: %v", name, err)
			}
		}
	}

	// Start credential notification listener only if there are writable tar-only credentials.
	// VirtioFS credentials don't need the notify/watch sync infrastructure.
	if hasWritableTarCredentials(tarOnlyCreds) {
		c.startNotifyListener(shedName, agent, tarOnlyCreds)
	}
}

// mountAllCredentialVirtioFS mounts all VirtioFS credential shares inside the guest.
func (c *Client) mountAllCredentialVirtioFS(ctx context.Context, agent *vmutil.AgentClient, creds map[string]config.MountConfig) error {
	for name, mount := range creds {
		mountTag := config.CredentialMountTag(name)
		if err := c.mountVirtioFSShare(ctx, agent, mountTag, mount.Target, mount.ReadOnly); err != nil {
			return err
		}
	}
	return nil
}

// hasWritableTarCredentials reports whether any tar-only credentials are writable.
func hasWritableTarCredentials(tarOnly map[string]config.MountConfig) bool {
	for _, mount := range tarOnly {
		if !mount.ReadOnly {
			return true
		}
	}
	return false
}

// cloneRepo clones a git repository into the VM's workspace.
func (c *Client) cloneRepo(ctx context.Context, agent *vmutil.AgentClient, repo string) error {
	env := c.buildEnvForGit()

	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"git", "clone", repo, "."},
		Env:        env,
		Stdout:     vmutil.NopWriteCloser(io.MultiWriter(&output, os.Stdout)),
		Stderr:     vmutil.NopWriteCloser(io.MultiWriter(&output, os.Stderr)),
		WorkingDir: config.WorkspacePath,
		TTY:        false,
	}

	if err := agent.Exec(ctx, opts); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	return nil
}

// buildEnvForGit builds environment variables for git operations.
func (c *Client) buildEnvForGit() []string {
	var env []string

	if c.serverCfg == nil {
		return env
	}

	for key, value := range c.serverCfg.EnvVars {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}

// startNotifyListener starts a credential notification listener for a VM.
// tarOnlyCreds contains only tar-transferred credentials that need bidirectional
// sync. VirtioFS credentials are excluded — they are live mounts.
func (c *Client) startNotifyListener(name string, agent *vmutil.AgentClient, tarOnlyCreds map[string]config.MountConfig) {
	if len(tarOnlyCreds) == 0 {
		return
	}

	listener := vmutil.NewCredentialNotifyListener(agent, tarOnlyCreds, c.credWatcher)
	listener.Start(context.Background(), name)

	// Register VM with the credential watcher for host->VM pushes
	if c.credWatcher != nil {
		c.credWatcher.RegisterVM(name, agent)
	}

	c.mu.Lock()
	c.notifyListeners[name] = listener
	c.mu.Unlock()
}

// stopNotifyListener stops the credential notification listener for a VM.
func (c *Client) stopNotifyListener(name string) {
	c.mu.Lock()
	nl := c.notifyListeners[name]
	delete(c.notifyListeners, name)
	c.mu.Unlock()

	if nl != nil {
		nl.Stop()
	}

	if c.credWatcher != nil {
		c.credWatcher.UnregisterVM(name)
	}
}
