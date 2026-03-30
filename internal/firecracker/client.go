//go:build linux
// +build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/vmutil"
)

// Client manages Firecracker VM instances.
type Client struct {
	cfg       *config.FirecrackerConfig
	serverCfg *config.ServerConfig
	netMgr    *NetworkManager

	mu       sync.Mutex
	vms      map[string]*VM    // name -> VM
	usedCIDs map[uint32]string // CID -> name
	usedIPs  map[string]string // IP -> name

	// Credential sync
	credWatcher *vmutil.CredentialWatcher // host-side fsnotify watcher

	// Message channels (generalized notify connections)
	messageChannels map[string]*vmutil.NotifyConn // name -> per-VM message channel
	pluginBridge    *plugin.Bridge
}

// NewClient creates a new Firecracker client.
func NewClient(cfg *config.FirecrackerConfig, serverCfg *config.ServerConfig, bridge *plugin.Bridge) (*Client, error) {
	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create network manager: %w", err)
	}

	client := &Client{
		cfg:             cfg,
		serverCfg:       serverCfg,
		netMgr:          netMgr,
		vms:             make(map[string]*VM),
		usedCIDs:        make(map[uint32]string),
		usedIPs:         make(map[string]string),
		messageChannels: make(map[string]*vmutil.NotifyConn),
		pluginBridge:    bridge,
	}

	// Load existing instances to populate CID and IP maps
	if err := client.loadExistingInstances(); err != nil {
		return nil, fmt.Errorf("failed to load existing instances: %w", err)
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

// Close closes the client and releases resources.
func (c *Client) Close() error {
	// Stop all message channels
	c.mu.Lock()
	for name, ch := range c.messageChannels {
		ch.Stop()
		if c.pluginBridge != nil {
			c.pluginBridge.UnregisterShed(name)
		}
		delete(c.messageChannels, name)
	}
	c.mu.Unlock()

	// Stop the credential watcher
	if c.credWatcher != nil {
		c.credWatcher.Stop()
	}

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
	return vmutil.NewAgentClient(dialer, c.cfg.ConsolePort, c.cfg.HealthPort, c.cfg.NotifyPort)
}

// CreateShed creates a new Firecracker-based shed.
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if req.LocalDir != "" {
		return nil, fmt.Errorf("--local-dir is not supported on the firecracker backend (planned for future release)")
	}

	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}

	if _, err := LoadMetadata(c.cfg.InstanceDir, req.Name); err == nil {
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}

	backend.Progress(ctx, "network", "Allocating network resources...")
	cid, err := c.AllocateCID(req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate CID: %w", err)
	}
	tapDevice, ipAddress, err := c.AllocateNetwork(req.Name)
	if err != nil {
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to allocate network: %w", err)
	}

	if err := c.netMgr.CreateTAPDevice(tapDevice); err != nil {
		c.ReleaseIP(ipAddress)
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}

	backend.Progress(ctx, "rootfs", "Copying root filesystem...")
	rootfsPath, err := CopyRootfs(c.cfg.BaseRootfs, c.cfg.InstanceDir, req.Name)
	if err != nil {
		if delErr := c.netMgr.DeleteTAPDevice(tapDevice); delErr != nil {
			log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, delErr)
		}
		c.ReleaseIP(ipAddress)
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to copy rootfs: %w", err)
	}

	cpus := req.CPUs
	if cpus == 0 {
		cpus = c.cfg.DefaultCPUs
	}
	memoryMB := req.MemoryMB
	if memoryMB == 0 {
		memoryMB = c.cfg.DefaultMemoryMB
	}

	meta := &Metadata{
		Name:       req.Name,
		Status:     config.StatusStopped,
		CreatedAt:  time.Now(),
		Backend:    config.BackendFirecracker,
		CID:        cid,
		IPAddress:  ipAddress,
		TAPDevice:  tapDevice,
		CPUs:       cpus,
		MemoryMB:   memoryMB,
		RootfsPath: rootfsPath,
		Repo:       req.Repo,
	}

	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if delErr := c.netMgr.DeleteTAPDevice(tapDevice); delErr != nil {
			log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, delErr)
		}
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		c.ReleaseIP(ipAddress)
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	c.RegisterInstance(req.Name, cid, ipAddress)

	vm, err := CreateVM(ctx, meta, c.cfg, c.netMgr)
	if err != nil {
		if delErr := c.netMgr.DeleteTAPDevice(tapDevice); delErr != nil {
			log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, delErr)
		}
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		c.UnregisterInstance(req.Name, cid, ipAddress)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	backend.Progress(ctx, "vm", "Starting virtual machine...")
	if err := vm.Start(ctx); err != nil {
		if delErr := c.netMgr.DeleteTAPDevice(tapDevice); delErr != nil {
			log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, delErr)
		}
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		c.UnregisterInstance(req.Name, cid, ipAddress)
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}

	meta.Status = config.StatusRunning
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if stopErr := vm.Stop(context.Background()); stopErr != nil {
			log.Printf("Warning: failed to stop VM: %v", stopErr)
		}
		if delErr := c.netMgr.DeleteTAPDevice(tapDevice); delErr != nil {
			log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, delErr)
		}
		if rmErr := meta.Delete(c.cfg.InstanceDir); rmErr != nil {
			log.Printf("Warning: failed to delete instance dir for %s: %v", req.Name, rmErr)
		}
		c.UnregisterInstance(req.Name, cid, ipAddress)
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
	c.startMessageChannel(req.Name, agent)

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
		ContainerID: fmt.Sprintf("fc-%s", meta.Name),
		Backend:     meta.Backend,
		IPAddress:   meta.IPAddress,
		CPUs:        meta.CPUs,
		MemoryMB:    meta.MemoryMB,
		PID:         meta.PID,
		RootfsPath:  meta.RootfsPath,
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
		ContainerID: fmt.Sprintf("fc-%s", meta.Name),
		Backend:     meta.Backend,
		IPAddress:   meta.IPAddress,
		CPUs:        meta.CPUs,
		MemoryMB:    meta.MemoryMB,
		PID:         meta.PID,
		RootfsPath:  meta.RootfsPath,
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
			c.stopMessageChannel(name)
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

	vm, err := CreateVM(ctx, meta, c.cfg, c.netMgr)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

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
	c.startMessageChannel(name, agent)

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
		ContainerID: fmt.Sprintf("fc-%s", meta.Name),
		Backend:     meta.Backend,
		IPAddress:   meta.IPAddress,
		CPUs:        meta.CPUs,
		MemoryMB:    meta.MemoryMB,
		PID:         meta.PID,
		RootfsPath:  meta.RootfsPath,
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
	c.stopMessageChannel(name)

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
	cfg, err := provisioner.LoadConfig(hookCtx)
	if err != nil {
		log.Printf("Warning: failed to load provision config for shutdown hook: %v", err)
	} else if cfg.HasShutdownHook() {
		provisioner.RunShutdownHook(hookCtx, cfg)
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
		ContainerID: fmt.Sprintf("fc-%s", meta.Name),
		Backend:     meta.Backend,
		IPAddress:   meta.IPAddress,
		CPUs:        meta.CPUs,
		MemoryMB:    meta.MemoryMB,
		PID:         meta.PID,
		RootfsPath:  meta.RootfsPath,
	}, nil
}

// GetNetworkEndpoint returns the IP address for a shed.
func (c *Client) GetNetworkEndpoint(ctx context.Context, name string) (string, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return "", fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
		}
		return "", err
	}

	return meta.IPAddress, nil
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

// startMessageChannel starts the generalized message channel for a VM.
func (c *Client) startMessageChannel(name string, agent *vmutil.AgentClient) {
	// Build credential setup if there are writable credentials
	var credSetup *plugin.CredentialSetupPayload
	var credChangeFn func(string, []string)
	if c.serverCfg != nil {
		creds := make(map[string]string)
		excludes := make(map[string][]string)
		for credName, mount := range c.serverCfg.Credentials {
			if !mount.ReadOnly {
				creds[credName] = mount.Target
				if len(mount.Exclude) > 0 {
					excludes[credName] = mount.Exclude
				}
			}
		}
		if len(creds) > 0 {
			credSetup = &plugin.CredentialSetupPayload{
				Credentials: creds,
				Excludes:    excludes,
			}
			credNL := vmutil.NewCredentialNotifyListener(agent, c.serverCfg.Credentials, c.credWatcher)
			credNL.SetName(name)
			credChangeFn = func(credName string, files []string) {
				if err := credNL.PullChangedFiles(credName, files); err != nil {
					log.Printf("[%s] Failed to pull credential changes for %s: %v", name, credName, err)
				}
			}
		}
	}

	// Create the combined message handler
	handler := vmutil.NewMessageHandler(credSetup, credChangeFn, func(env *plugin.Envelope) {
		if c.pluginBridge != nil {
			if err := c.pluginBridge.PublishToHost(name, env); err != nil {
				log.Printf("[%s] Failed to publish plugin message: %v", name, err)
			}
		}
	})

	conn := vmutil.NewNotifyConn(agent.Dialer(), agent.NotifyPort(), name)

	// Register VM with the credential watcher for host->VM pushes
	if c.credWatcher != nil && credSetup != nil {
		c.credWatcher.RegisterVM(name, agent)
	}

	// Register with plugin bridge before starting the connection to avoid
	// a race where messages arrive before the shed is enriched with metadata.
	if c.pluginBridge != nil {
		serverName := ""
		if c.serverCfg != nil {
			serverName = c.serverCfg.Name
		}
		c.pluginBridge.RegisterShed(name, &plugin.ShedConn{
			Name:    name,
			Backend: string(config.BackendFirecracker),
			Server:  serverName,
			Send:    handler.SendPluginMessage,
		})
	}

	// Start the message connection after registration is complete.
	conn.Start(context.Background(), handler)

	c.mu.Lock()
	c.messageChannels[name] = conn
	c.mu.Unlock()
}

// stopMessageChannel stops the message channel for a VM.
func (c *Client) stopMessageChannel(name string) {
	c.mu.Lock()
	ch := c.messageChannels[name]
	delete(c.messageChannels, name)
	c.mu.Unlock()

	if ch != nil {
		ch.Stop()
	}

	if c.pluginBridge != nil {
		c.pluginBridge.UnregisterShed(name)
	}

	if c.credWatcher != nil {
		c.credWatcher.UnregisterVM(name)
	}
}
