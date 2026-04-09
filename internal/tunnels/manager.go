package tunnels

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Manager handles tunnel lifecycle operations.
type Manager struct {
	config *TunnelConfig
	state  *TunnelState
}

// NewManager creates a new tunnel manager.
func NewManager() (*Manager, error) {
	cfg, err := LoadTunnelConfig()
	if err != nil {
		return nil, err
	}

	state, err := LoadTunnelState()
	if err != nil {
		return nil, err
	}

	return &Manager{
		config: cfg,
		state:  state,
	}, nil
}

// NewManagerWithConfig creates a manager with a specific config (for testing).
func NewManagerWithConfig(cfg *TunnelConfig, state *TunnelState) *Manager {
	return &Manager{
		config: cfg,
		state:  state,
	}
}

// Config returns the tunnel configuration.
func (m *Manager) Config() *TunnelConfig {
	return m.config
}

// State returns the tunnel state.
func (m *Manager) State() *TunnelState {
	return m.state
}

// StartTunnels starts tunnels for all port mappings using the Connect API.
// Returns the active tunnels that the caller should manage (stop on shutdown).
func (m *Manager) StartTunnels(serverAddr, shedName string, ports []PortMapping) ([]*Tunnel, error) {
	client := NewConnectClient(serverAddr)
	var tunnels []*Tunnel

	for _, pm := range ports {
		tun := NewTunnel(client, shedName, pm)
		if err := tun.Start(); err != nil {
			// Clean up already-started tunnels
			for _, started := range tunnels {
				started.Stop()
			}
			return nil, err
		}
		tunnels = append(tunnels, tun)
	}

	return tunnels, nil
}

// SaveBackground saves the state for a background tunnel daemon.
func (m *Manager) SaveBackground(shedName, serverName, profile string, pid int, ports []PortMapping) error {
	m.state.SetTunnel(shedName, TunnelEntry{
		ShedName:   shedName,
		Profile:    profile,
		PID:        pid,
		Ports:      ports,
		StartedAt:  time.Now(),
		ServerName: serverName,
	})
	return m.state.Save()
}

// Stop stops a tunnel for a shed by signaling its daemon process.
func (m *Manager) Stop(shedName string) error {
	entry, ok := m.state.GetTunnel(shedName)
	if !ok {
		return fmt.Errorf("no tunnel found for shed %q", shedName)
	}

	_ = m.killProcess(entry.PID)
	m.state.RemoveTunnel(shedName)
	return m.state.Save()
}

// StopAllTunnelsForShed stops any tunnel for a specific shed.
func (m *Manager) StopAllTunnelsForShed(shedName string) error {
	entry, ok := m.state.GetTunnel(shedName)
	if !ok {
		return nil
	}
	_ = m.killProcess(entry.PID)
	m.state.RemoveTunnel(shedName)
	return m.state.Save()
}

// StopAll stops all tunnels.
func (m *Manager) StopAll() error {
	for shedName, entry := range m.state.GetAllTunnels() {
		_ = m.killProcess(entry.PID)
		m.state.RemoveTunnel(shedName)
	}
	return m.state.Save()
}

// CheckHealth checks if a tunnel process is still alive.
func (m *Manager) CheckHealth(shedName string) (bool, error) {
	entry, ok := m.state.GetTunnel(shedName)
	if !ok {
		return false, nil
	}
	return m.isProcessAlive(entry.PID), nil
}

// CleanupDeadTunnels removes state entries for dead processes.
func (m *Manager) CleanupDeadTunnels() error {
	changed := false
	for shedName, entry := range m.state.GetAllTunnels() {
		if !m.isProcessAlive(entry.PID) {
			m.state.RemoveTunnel(shedName)
			changed = true
		}
	}
	if changed {
		return m.state.Save()
	}
	return nil
}

// CheckPortConflict checks if a local port is available.
func (m *Manager) CheckPortConflict(port int) error {
	if shedName, entry := m.state.FindTunnelUsingPort(port); entry != nil {
		if m.isProcessAlive(entry.PID) {
			duration := time.Since(entry.StartedAt)
			return fmt.Errorf("port %d in use by tunnel %s:%s (started %s ago)",
				port, shedName, entry.Profile, formatDuration(duration))
		}
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port %d is already in use. Check with: lsof -i :%d", port, port)
	}
	listener.Close()
	return nil
}

// CheckPortConflicts checks multiple ports for conflicts.
func (m *Manager) CheckPortConflicts(ports []PortMapping) error {
	for _, pm := range ports {
		if err := m.CheckPortConflict(pm.Local); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}

func (m *Manager) isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
