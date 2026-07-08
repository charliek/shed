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

	tunnels, err := readTunnels(path)
	if err != nil {
		return nil, err
	}
	return &TunnelState{Tunnels: tunnels, path: path}, nil
}

// Save writes the in-memory tunnel state to disk, replacing the file's
// contents. It is not transactional — for concurrent-safe partial mutations
// (the normal case), use Update instead.
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

	if err := writeTunnels(path, s.Tunnels); err != nil {
		return err
	}
	s.path = path
	return nil
}

// Update atomically applies mutate to the on-disk tunnel state under a single
// lock acquisition (load → mutate → save). Reloading inside the lock prevents a
// long-lived process (e.g. a background tunnel daemon) from clobbering entries
// written by other processes since it last loaded — the bug that lets one
// daemon's shutdown silently delete another shed's tunnel entry. On success the
// in-memory map is refreshed to the persisted result.
//
// mutate runs while the state lock is held; it must only touch the supplied map
// and must not call other state methods (they would deadlock on the same lock).
func (s *TunnelState) Update(mutate func(tunnels map[string]TunnelEntry)) error {
	lock := NewFileLock(s.path)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("failed to acquire state lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	tunnels, err := readTunnels(s.path)
	if err != nil {
		return err
	}
	mutate(tunnels)
	if err := writeTunnels(s.path, tunnels); err != nil {
		return err
	}
	s.Tunnels = tunnels
	return nil
}

// readTunnels reads and parses the tunnel map from path. The caller must hold
// the state lock. A missing file yields an empty (non-nil) map.
func readTunnels(path string) (map[string]TunnelEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]TunnelEntry), nil
		}
		return nil, fmt.Errorf("failed to read tunnel state: %w", err)
	}

	var state TunnelState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse tunnel state: %w", err)
	}
	if state.Tunnels == nil {
		state.Tunnels = make(map[string]TunnelEntry)
	}
	return state.Tunnels, nil
}

// writeTunnels atomically writes the tunnel map to path via a temp file +
// rename. The caller must hold the state lock.
func writeTunnels(path string, tunnels map[string]TunnelEntry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := json.MarshalIndent(&TunnelState{Tunnels: tunnels}, "", "  ")
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
	return nil
}

// GetTunnel returns the tunnel entry for a shed.
func (s *TunnelState) GetTunnel(shedName string) (TunnelEntry, bool) {
	entry, ok := s.Tunnels[shedName]
	return entry, ok
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
