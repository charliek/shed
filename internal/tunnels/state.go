package tunnels

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charliek/shed/internal/config"
)

// TunnelState represents the runtime state of all tunnels.
type TunnelState struct {
	Tunnels map[string]TunnelEntry `json:"tunnels"`

	// Path to state file (not serialized)
	path string `json:"-"`
}

// TunnelEntry represents a running tunnel.
type TunnelEntry struct {
	ShedName   string        `json:"shed_name"`
	Profile    string        `json:"profile"`
	PID        int           `json:"pid"`
	Ports      []PortMapping `json:"ports"`
	StartedAt  time.Time     `json:"started_at"`
	ServerName string        `json:"server_name"`
}

// LoadTunnelState loads the tunnel state from disk.
func LoadTunnelState() (*TunnelState, error) {
	return LoadTunnelStateFromPath(config.GetTunnelStatePath())
}

// LoadTunnelStateFromPath loads tunnel state from a specific path.
func LoadTunnelStateFromPath(path string) (*TunnelState, error) {
	// Acquire lock for reading
	lock := NewFileLock(path)
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("failed to acquire state lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	state := &TunnelState{
		Tunnels: make(map[string]TunnelEntry),
		path:    path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, fmt.Errorf("failed to read tunnel state: %w", err)
	}

	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("failed to parse tunnel state: %w", err)
	}

	if state.Tunnels == nil {
		state.Tunnels = make(map[string]TunnelEntry)
	}

	state.path = path
	return state, nil
}

// Save writes the tunnel state to disk.
func (s *TunnelState) Save() error {
	return s.SaveToPath(s.path)
}

// SaveToPath writes the tunnel state to a specific path.
func (s *TunnelState) SaveToPath(path string) error {
	// Acquire lock for writing
	lock := NewFileLock(path)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("failed to acquire state lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to save state file: %w", err)
	}

	s.path = path
	return nil
}

// GetTunnel returns the tunnel entry for a shed.
func (s *TunnelState) GetTunnel(shedName string) (TunnelEntry, bool) {
	entry, ok := s.Tunnels[shedName]
	return entry, ok
}

// SetTunnel sets the tunnel entry for a shed.
func (s *TunnelState) SetTunnel(shedName string, entry TunnelEntry) {
	s.Tunnels[shedName] = entry
}

// RemoveTunnel removes the tunnel entry for a shed.
func (s *TunnelState) RemoveTunnel(shedName string) {
	delete(s.Tunnels, shedName)
}

// GetAllTunnels returns all tunnel entries.
func (s *TunnelState) GetAllTunnels() map[string]TunnelEntry {
	return s.Tunnels
}

// FindTunnelUsingPort returns the shed name using a specific local port.
func (s *TunnelState) FindTunnelUsingPort(port int) (string, *TunnelEntry) {
	for shedName, entry := range s.Tunnels {
		for _, pm := range entry.Ports {
			if pm.Local == port {
				entryCopy := entry
				return shedName, &entryCopy
			}
		}
	}
	return "", nil
}
