package firecracker

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
)

// Client manages Firecracker VM instances.
type Client struct {
	cfg    *config.FirecrackerConfig
	netMgr *NetworkManager

	mu       sync.Mutex
	vms      map[string]*VM    // name -> VM
	usedCIDs map[uint32]string // CID -> name
	usedIPs  map[string]string // IP -> name
}

// NewClient creates a new Firecracker client.
func NewClient(cfg *config.FirecrackerConfig) (*Client, error) {
	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create network manager: %w", err)
	}

	client := &Client{
		cfg:      cfg,
		netMgr:   netMgr,
		vms:      make(map[string]*VM),
		usedCIDs: make(map[uint32]string),
		usedIPs:  make(map[string]string),
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

// AllocateCID allocates a new CID for a VM.
func (c *Client) AllocateCID() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()

	cid := c.cfg.VsockBaseCID
	for {
		if _, used := c.usedCIDs[cid]; !used {
			return cid
		}
		cid++
	}
}

// AllocateNetwork allocates a TAP device and IP address for a VM.
func (c *Client) AllocateNetwork(name string) (tapDevice, ipAddress string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build map of used indices from TAP device names
	usedIndices := make(map[int]bool)
	for ip := range c.usedIPs {
		// Extract index from IP (last octet - 2 gives us the index)
		// e.g., 172.30.0.2 -> index 0, 172.30.0.3 -> index 1
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			if lastOctet, err := strconv.Atoi(parts[3]); err == nil {
				index := lastOctet - 2 // .2 -> index 0, .3 -> index 1, etc.
				if index >= 0 {
					usedIndices[index] = true
				}
			}
		}
	}

	// Find available index
	index := c.netMgr.FindAvailableTAPIndex(usedIndices)
	tapDevice = c.netMgr.TAPDeviceName(index)
	ipAddress = c.netMgr.AllocateIP(index)

	return tapDevice, ipAddress, nil
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
	cid := c.AllocateCID()
	tapDevice, ipAddress, err := c.AllocateNetwork(req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate network: %w", err)
	}

	// Create TAP device
	if err := c.netMgr.CreateTAPDevice(tapDevice); err != nil {
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}

	// Copy rootfs
	rootfsPath, err := CopyRootfs(c.cfg.BaseRootfs, c.cfg.InstanceDir, req.Name)
	if err != nil {
		c.netMgr.DeleteTAPDevice(tapDevice)
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
		c.netMgr.DeleteTAPDevice(tapDevice)
		DeleteRootfs(c.cfg.InstanceDir, req.Name)
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Register instance
	c.RegisterInstance(req.Name, cid, ipAddress)

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
			meta.Save(c.cfg.InstanceDir)
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
			continue // Skip invalid sheds
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
		vm.Stop(context.Background())
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Store VM reference
	c.mu.Lock()
	c.vms[name] = vm
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
