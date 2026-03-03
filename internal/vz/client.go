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

	backend.Progress(ctx, "rootfs", "Copying root filesystem...")
	rootfsPath, err := CopyRootfs(c.cfg.BaseRootfs, c.cfg.InstanceDir, req.Name)
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
	}

	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	vm := CreateVM(meta, c.cfg)

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

	// Transfer credentials
	if c.serverCfg != nil {
		backend.Progress(ctx, "credentials", "Transferring credentials...")
		credTransfer := vmutil.NewCredentialTransfer(agent, c.serverCfg)
		if err := credTransfer.TransferAll(ctx); err != nil {
			log.Printf("Warning: credential transfer failed: %v", err)
		}
	}

	// Start credential notification listener for bidirectional sync
	c.startNotifyListener(req.Name, agent)

	// Clone repo if specified
	if req.Repo != "" {
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

	return &config.Shed{
		Name:        meta.Name,
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("vz-%s", meta.Name),
		Backend:     meta.Backend,
	}, nil
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

	return &config.Shed{
		Name:        meta.Name,
		Status:      status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("vz-%s", meta.Name),
		Backend:     meta.Backend,
	}, nil
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

	vm := CreateVM(meta, c.cfg)

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

	// Refresh credentials on start
	if c.serverCfg != nil {
		credTransfer := vmutil.NewCredentialTransfer(agent, c.serverCfg)
		if err := credTransfer.TransferAll(ctx); err != nil {
			log.Printf("Warning: credential transfer failed: %v", err)
		}
	}

	// Start credential notification listener for bidirectional sync
	c.startNotifyListener(name, agent)

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

	return &config.Shed{
		Name:        meta.Name,
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("vz-%s", meta.Name),
		Backend:     meta.Backend,
	}, nil
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

	return &config.Shed{
		Name:        meta.Name,
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("vz-%s", meta.Name),
		Backend:     meta.Backend,
	}, nil
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
func (c *Client) startNotifyListener(name string, agent *vmutil.AgentClient) {
	if c.serverCfg == nil {
		return
	}

	hasWritable := false
	for _, mount := range c.serverCfg.Credentials {
		if !mount.ReadOnly {
			hasWritable = true
			break
		}
	}
	if !hasWritable {
		return
	}

	listener := vmutil.NewCredentialNotifyListener(agent, c.serverCfg, c.credWatcher)
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
