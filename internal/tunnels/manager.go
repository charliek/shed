package tunnels

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/config"
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

// BuildSSHArgs constructs the SSH command arguments for port forwarding.
func (m *Manager) BuildSSHArgs(shedName string, serverEntry *config.ServerEntry, ports []PortMapping) []string {
	knownHostsPath := config.GetKnownHostsPath()

	args := []string{
		"ssh",
		"-N", // No remote command
		"-T", // No pseudo-terminal
		"-p", strconv.Itoa(serverEntry.SSHPort),
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", fmt.Sprintf("ServerAliveInterval=%d", m.config.SSH.ServerAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", m.config.SSH.ServerAliveCountMax),
		"-o", fmt.Sprintf("ConnectTimeout=%d", m.config.SSH.ConnectTimeout),
		"-o", "ExitOnForwardFailure=yes",
	}

	for _, pm := range ports {
		args = append(args, "-L", fmt.Sprintf("%d:localhost:%d", pm.Local, pm.Remote))
	}

	args = append(args, shedName+"@"+serverEntry.Host)
	return args
}

// StartForeground starts a tunnel in the foreground using syscall.Exec.
// This replaces the current process.
func (m *Manager) StartForeground(shedName, serverName string, serverEntry *config.ServerEntry, ports []PortMapping, profile string) error {
	args := m.BuildSSHArgs(shedName, serverEntry, ports)

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	// Note: We don't save state for foreground tunnels since the process
	// replaces the current one and we can't track the PID

	if err := syscall.Exec(sshPath, args, os.Environ()); err != nil {
		return fmt.Errorf("failed to exec ssh: %w", err)
	}

	return nil
}

// StartBackground starts a tunnel as a background daemon.
func (m *Manager) StartBackground(shedName, serverName string, serverEntry *config.ServerEntry, ports []PortMapping, profile string) error {
	args := m.BuildSSHArgs(shedName, serverEntry, ports)

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	cmd := exec.Command(sshPath, args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create new session (detach from terminal)
	}

	// Open /dev/null for proper process detachment
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Stdin = devNull

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ssh: %w", err)
	}

	// Save state
	m.state.SetTunnel(shedName, TunnelEntry{
		ShedName:   shedName,
		Profile:    profile,
		PID:        cmd.Process.Pid,
		Ports:      ports,
		StartedAt:  time.Now(),
		ServerName: serverName,
	})

	if err := m.state.Save(); err != nil {
		// Rollback: kill the process and remove from in-memory state
		_ = m.killProcess(cmd.Process.Pid)
		m.state.RemoveTunnel(shedName)
		return fmt.Errorf("failed to save tunnel state: %w", err)
	}

	return nil
}

// Stop stops a tunnel for a shed.
func (m *Manager) Stop(shedName string) error {
	entry, ok := m.state.GetTunnel(shedName)
	if !ok {
		return fmt.Errorf("no tunnel found for shed %q", shedName)
	}

	// Process might already be dead, just clean up state regardless of error
	_ = m.killProcess(entry.PID)

	m.state.RemoveTunnel(shedName)
	return m.state.Save()
}

// StopAllTunnelsForShed stops all tunnels for a shed (used when shed is stopped/deleted).
func (m *Manager) StopAllTunnelsForShed(shedName string) error {
	entry, ok := m.state.GetTunnel(shedName)
	if !ok {
		return nil // No tunnel to stop
	}

	// Process might already be dead, just clean up state regardless of error
	_ = m.killProcess(entry.PID)

	m.state.RemoveTunnel(shedName)
	return m.state.Save()
}

// StopAll stops all tunnels.
func (m *Manager) StopAll() error {
	for shedName, entry := range m.state.GetAllTunnels() {
		// Process might already be dead, continue cleaning up regardless of error
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
// Returns a descriptive error if the port is in use.
func (m *Manager) CheckPortConflict(port int) error {
	// First check our own state for conflicts
	if shedName, entry := m.state.FindTunnelUsingPort(port); entry != nil {
		if m.isProcessAlive(entry.PID) {
			duration := time.Since(entry.StartedAt)
			return fmt.Errorf("port %d in use by tunnel %s:%s (started %s ago)",
				port, shedName, entry.Profile, formatDuration(duration))
		}
	}

	// Try to bind to the port to check if it's free
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

// killProcess sends SIGTERM to a process.
func (m *Manager) killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	return process.Signal(syscall.SIGTERM)
}

// isProcessAlive checks if a process is still running.
func (m *Manager) isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Signal 0 checks if process exists without sending a signal
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// formatDuration formats a duration in a human-readable way.
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
