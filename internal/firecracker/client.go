//go:build linux
// +build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
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
}

// NewClient creates a new Firecracker client.
func NewClient(cfg *config.FirecrackerConfig, serverCfg *config.ServerConfig) (*Client, error) {
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

// Close closes the client and releases resources.
func (c *Client) Close() error {
	return nil
}

// Config returns the Firecracker configuration.
func (c *Client) Config() *config.FirecrackerConfig {
	return c.cfg
}

// MaxVsockCID is the maximum valid CID for vsock connections.
// CID 0 and 1 are reserved, and the maximum is 2^32-1, but we use a
// reasonable upper bound to prevent resource exhaustion.
const MaxVsockCID uint32 = 65535

// AllocateCID allocates and reserves a new CID for a VM.
// Returns an error if all CIDs in the range [VsockBaseCID, MaxVsockCID] are exhausted.
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
// The IP is marked as used immediately to prevent race conditions with parallel allocations.
func (c *Client) AllocateNetwork(name string) (tapDevice, ipAddress string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build map of used indices by computing offset from the gateway IP.
	// AllocateIP(index) returns gateway + index + 1, so the reverse is:
	//   index = allocatedIP - gateway - 1
	usedIndices := make(map[int]bool)
	gatewayIP := net.ParseIP(c.netMgr.Gateway()).To4()
	gatewayInt := ipToUint32(gatewayIP)
	for ip := range c.usedIPs {
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			continue
		}
		index := int(ipToUint32(parsed) - gatewayInt - 1)
		if index >= 0 {
			usedIndices[index] = true
		}
	}

	// Find available index
	index, err := c.netMgr.FindAvailableTAPIndex(usedIndices)
	if err != nil {
		return "", "", fmt.Errorf("failed to find available TAP index: %w", err)
	}
	tapDevice = c.netMgr.TAPDeviceName(index)
	ipAddress, err = c.netMgr.AllocateIP(index)
	if err != nil {
		return "", "", err
	}

	// Mark IP as used immediately to prevent race conditions with parallel allocations
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
// This is idempotent - safe to call if already registered (e.g., IP was pre-registered by AllocateNetwork).
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

// CreateShed creates a new Firecracker-based shed.
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	// Validate name
	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}

	// Check if instance already exists
	if _, err := LoadMetadata(c.cfg.InstanceDir, req.Name); err == nil {
		return nil, fmt.Errorf("shed %q already exists", req.Name)
	}

	// Allocate resources
	cid, err := c.AllocateCID(req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate CID: %w", err)
	}
	tapDevice, ipAddress, err := c.AllocateNetwork(req.Name)
	if err != nil {
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to allocate network: %w", err)
	}

	// Create TAP device
	if err := c.netMgr.CreateTAPDevice(tapDevice); err != nil {
		c.ReleaseIP(ipAddress)
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}

	// Copy rootfs
	rootfsPath, err := CopyRootfs(c.cfg.BaseRootfs, c.cfg.InstanceDir, req.Name)
	if err != nil {
		if delErr := c.netMgr.DeleteTAPDevice(tapDevice); delErr != nil {
			log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, delErr)
		}
		c.ReleaseIP(ipAddress)
		c.ReleaseCID(cid)
		return nil, fmt.Errorf("failed to copy rootfs: %w", err)
	}

	// Determine CPU and memory
	cpus := req.CPUs
	if cpus == 0 {
		cpus = c.cfg.DefaultCPUs
	}
	memoryMB := req.MemoryMB
	if memoryMB == 0 {
		memoryMB = c.cfg.DefaultMemoryMB
	}

	// Create metadata
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

	// Save metadata
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

	// Register instance
	c.RegisterInstance(req.Name, cid, ipAddress)

	// Create and start VM
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

	// Update metadata to running
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

	// Store VM reference
	c.mu.Lock()
	c.vms[req.Name] = vm
	c.mu.Unlock()

	// Get vsock client for setup operations
	vsockPath := filepath.Join(c.cfg.SocketDir, fmt.Sprintf("%s.vsock", meta.Name))
	vsockClient := NewVsockClient(vsockPath, c.cfg.ConsolePort, c.cfg.HealthPort)

	// Transfer credentials
	if c.serverCfg != nil {
		credTransfer := NewCredentialTransfer(vsockClient, c.serverCfg)
		if err := credTransfer.TransferAll(ctx); err != nil {
			log.Printf("Warning: credential transfer failed: %v", err)
			// Continue - credentials are optional
		}
	}

	// Clone repo if specified
	if req.Repo != "" {
		if err := c.cloneRepo(ctx, vsockClient, req.Repo); err != nil {
			log.Printf("Warning: failed to clone repo %s: %v", req.Repo, err)
			// Continue - repo clone failure is not fatal, shed is still usable
		}
	}

	// Run provisioning
	if !req.NoProvision {
		provisioner := NewProvisioner(vsockClient, req.Name)
		provisioner.SetOutput(os.Stdout, os.Stderr)
		cfg, err := provisioner.LoadConfig(ctx)
		if err != nil {
			log.Printf("Warning: failed to load provisioning config: %v", err)
		} else {
			if err := provisioner.RunProvisioning(ctx, cfg, true); err != nil {
				log.Printf("Warning: provisioning failed: %v", err)
				// Continue - provisioning failure is not fatal
			}
		}
	}

	return &config.Shed{
		Name:        meta.Name,
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("fc-%s", meta.Name), // Pseudo container ID for compatibility
		Backend:     meta.Backend,
	}, nil
}

// GetShed returns a shed by name.
func (c *Client) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		return nil, err
	}

	// Check if VM is actually running
	status := meta.Status
	if status == config.StatusRunning {
		vm := &VM{meta: meta, cfg: c.cfg}
		if !vm.IsRunning() {
			// Update status if VM died
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
		return err
	}

	// Stop if running
	if meta.Status == config.StatusRunning {
		if _, err := c.StopShed(ctx, name); err != nil {
			return fmt.Errorf("failed to stop shed: %w", err)
		}
	}

	// Delete TAP device
	if err := c.netMgr.DeleteTAPDevice(meta.TAPDevice); err != nil {
		// Log but continue
		log.Printf("Warning: failed to delete TAP device %s: %v", meta.TAPDevice, err)
	}

	// Unregister instance
	c.UnregisterInstance(name, meta.CID, meta.IPAddress)

	// Delete instance directory (includes rootfs and metadata)
	if err := meta.Delete(c.cfg.InstanceDir); err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	return nil
}

// StartShed starts a stopped shed.
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		return nil, err
	}

	if meta.Status == config.StatusRunning {
		vm := &VM{meta: meta, cfg: c.cfg}
		if vm.IsRunning() {
			return nil, fmt.Errorf("shed %q is already running", name)
		}
		// VM died, reset status
		meta.Status = config.StatusStopped
		meta.PID = 0
	}

	// Create and start VM
	vm, err := CreateVM(ctx, meta, c.cfg, c.netMgr)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	if err := vm.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}

	// Update metadata
	meta.Status = config.StatusRunning
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		if stopErr := vm.Stop(context.Background()); stopErr != nil {
			log.Printf("Warning: failed to stop VM: %v", stopErr)
		}
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Store VM reference
	c.mu.Lock()
	c.vms[name] = vm
	c.mu.Unlock()

	// Get vsock client for setup operations
	vsockPath := filepath.Join(c.cfg.SocketDir, fmt.Sprintf("%s.vsock", meta.Name))
	vsockClient := NewVsockClient(vsockPath, c.cfg.ConsolePort, c.cfg.HealthPort)

	// Refresh credentials on start
	if c.serverCfg != nil {
		credTransfer := NewCredentialTransfer(vsockClient, c.serverCfg)
		if err := credTransfer.TransferAll(ctx); err != nil {
			log.Printf("Warning: credential transfer failed: %v", err)
			// Continue - credentials are optional
		}
	}

	// Run startup hook only (not install)
	provisioner := NewProvisioner(vsockClient, name)
	provisioner.SetOutput(os.Stdout, os.Stderr)
	cfg, err := provisioner.LoadConfig(ctx)
	if err != nil {
		log.Printf("Warning: failed to load provisioning config: %v", err)
	} else {
		if err := provisioner.RunProvisioning(ctx, cfg, false); err != nil {
			log.Printf("Warning: startup hook failed: %v", err)
			// Continue - hook failure is not fatal
		}
	}

	return &config.Shed{
		Name:        meta.Name,
		Status:      meta.Status,
		CreatedAt:   meta.CreatedAt,
		Repo:        meta.Repo,
		ContainerID: fmt.Sprintf("fc-%s", meta.Name),
		Backend:     meta.Backend,
	}, nil
}

// StopShed stops a running shed.
func (c *Client) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		return nil, err
	}

	if meta.Status != config.StatusRunning {
		return nil, fmt.Errorf("shed %q is not running", name)
	}

	// Get or create VM handle
	c.mu.Lock()
	vm := c.vms[name]
	c.mu.Unlock()

	if vm == nil {
		vm = &VM{meta: meta, cfg: c.cfg}
	}

	// Stop VM
	if err := vm.Stop(ctx); err != nil {
		return nil, fmt.Errorf("failed to stop VM: %w", err)
	}

	// Update metadata
	meta.Status = config.StatusStopped
	meta.PID = 0
	if err := meta.Save(c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Remove VM reference
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
	}, nil
}

// GetNetworkEndpoint returns the IP address for a shed.
func (c *Client) GetNetworkEndpoint(ctx context.Context, name string) (string, error) {
	meta, err := LoadMetadata(c.cfg.InstanceDir, name)
	if err != nil {
		return "", err
	}

	return meta.IPAddress, nil
}

// cloneRepo clones a git repository into the VM's workspace.
func (c *Client) cloneRepo(ctx context.Context, vsock *VsockClient, repo string) error {
	// Build environment variables for git
	env := c.buildEnvForGit()

	// Capture output for logging
	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"git", "clone", repo, "."},
		Env:        env,
		Stdout:     &nopWriteCloser{io.MultiWriter(&output, os.Stdout)},
		Stderr:     &nopWriteCloser{io.MultiWriter(&output, os.Stderr)},
		WorkingDir: config.WorkspacePath,
		TTY:        false,
	}

	if err := vsock.Exec(ctx, opts); err != nil {
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

	// Add configured environment variables (GIT_SSH_COMMAND, etc.)
	for key, value := range c.serverCfg.EnvVars {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}
