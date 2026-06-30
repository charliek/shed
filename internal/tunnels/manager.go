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
func (m *Manager) StartTunnels(target ConnectTarget, shedName string, ports []PortMapping) ([]*Tunnel, error) {
	client := NewConnectClient(target)
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
	return m.state.Update(func(tunnels map[string]TunnelEntry) {
		tunnels[shedName] = TunnelEntry{
			ShedName:   shedName,
			Profile:    profile,
			PID:        pid,
			Ports:      ports,
			StartedAt:  time.Now(),
			ServerName: serverName,
		}
	})
}

// killAndRemove removes a shed's tunnel entry and signals its daemon. The PID
// is read and the entry deleted inside one locked read-modify-write, so the
// process we signal is exactly the entry we remove — a concurrent replacement
// can't leave us killing the old daemon while deleting the new one's state.
// Returns whether a tunnel was found.
func (m *Manager) killAndRemove(shedName string) (bool, error) {
	var pid int
	found := false
	if err := m.state.Update(func(tunnels map[string]TunnelEntry) {
		if e, ok := tunnels[shedName]; ok {
			pid, found = e.PID, true
			delete(tunnels, shedName)
		}
	}); err != nil {
		return false, err
	}
	if found {
		_ = m.killProcess(pid)
	}
	return found, nil
}

// Stop stops a tunnel for a shed by signaling its daemon process and removing
// its state entry. It errors if no tunnel is recorded for the shed.
func (m *Manager) Stop(shedName string) error {
	found, err := m.killAndRemove(shedName)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no tunnel found for shed %q", shedName)
	}
	return nil
}

// StopAllTunnelsForShed stops any tunnel for a specific shed, succeeding even
// when none is running.
func (m *Manager) StopAllTunnelsForShed(shedName string) error {
	_, err := m.killAndRemove(shedName)
	return err
}

// RemoveOwnedTunnel removes a shed's tunnel entry only if it is still owned by
// the given PID. A background daemon calls this on shutdown (with its own PID)
// so it never deletes an entry that a --replace swapped in for a newer daemon.
func (m *Manager) RemoveOwnedTunnel(shedName string, pid int) error {
	return m.state.Update(func(tunnels map[string]TunnelEntry) {
		if e, ok := tunnels[shedName]; ok && e.PID == pid {
			delete(tunnels, shedName)
		}
	})
}

// StopAll stops all tunnels.
func (m *Manager) StopAll() error {
	var pids []int
	err := m.state.Update(func(tunnels map[string]TunnelEntry) {
		for shedName, entry := range tunnels {
			pids = append(pids, entry.PID)
			delete(tunnels, shedName)
		}
	})
	if err != nil {
		return err
	}
	for _, pid := range pids {
		_ = m.killProcess(pid)
	}
	return nil
}

// CheckHealth checks if a tunnel process is still alive.
func (m *Manager) CheckHealth(shedName string) (bool, error) {
	entry, ok := m.state.GetTunnel(shedName)
	if !ok {
		return false, nil
	}
	return m.isProcessAlive(entry.PID), nil
}

// CleanupDeadTunnels removes state entries for dead processes. It re-reads the
// state under lock so a concurrently-started tunnel is preserved.
func (m *Manager) CleanupDeadTunnels() error {
	return m.state.Update(func(tunnels map[string]TunnelEntry) {
		for shedName, entry := range tunnels {
			if !m.isProcessAlive(entry.PID) {
				delete(tunnels, shedName)
			}
		}
	})
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
