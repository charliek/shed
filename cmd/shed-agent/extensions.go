//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/sdk"
	"gopkg.in/yaml.v3"
)

// extensionManifest describes an extension available in the image.
// Loaded from YAML files in /etc/shed-extensions.d/.
type extensionManifest struct {
	Namespace   string `yaml:"namespace"`
	SystemdUnit string `yaml:"systemd_unit"`
	Description string `yaml:"description,omitempty"`
}

// extensionState tracks the runtime state of an enabled extension.
type extensionState struct {
	manifest    extensionManifest
	guestStatus string // plugin.ExtGuest* constants
	hostStatus  string // plugin.ExtHost* constants
}

// loadManifests reads extension manifests from the given directory.
// Returns a map keyed by namespace. Missing directory returns an empty map (not an error).
// Invalid manifests are logged and skipped.
func loadManifests(dir string) map[string]extensionManifest {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		log.Printf("Warning: failed to read extension manifest directory %s: %v", dir, err)
		return nil
	}

	manifests := make(map[string]extensionManifest)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Warning: failed to read manifest %s: %v", path, err)
			continue
		}
		var m extensionManifest
		if err := yaml.Unmarshal(data, &m); err != nil {
			log.Printf("Warning: failed to parse manifest %s: %v", path, err)
			continue
		}
		// Validate required fields
		if m.Namespace == "" {
			log.Printf("Warning: manifest %s has empty namespace, skipping", path)
			continue
		}
		if m.SystemdUnit == "" {
			log.Printf("Warning: manifest %s has empty systemd_unit, skipping", path)
			continue
		}
		if _, exists := manifests[m.Namespace]; exists {
			log.Printf("Warning: duplicate namespace %q in manifest %s, skipping", m.Namespace, path)
			continue
		}
		manifests[m.Namespace] = m
	}
	return manifests
}

// enableExtensions activates systemd units for the given extension namespaces.
// Idempotent: skips extensions that are already active.
func (s *Server) enableExtensions(enabled []string) {
	manifests := loadManifests(s.manifestDir)
	if len(manifests) == 0 && len(enabled) > 0 {
		log.Printf("No extension manifests found in %s", s.manifestDir)
		return
	}

	s.extensionsMu.Lock()
	defer s.extensionsMu.Unlock()

	// Initialize the extensions map on first call
	if s.extensions == nil {
		s.extensions = make(map[string]*extensionState)
	}

	for _, ns := range enabled {
		// Skip if already activated
		if _, exists := s.extensions[ns]; exists {
			continue
		}

		manifest, ok := manifests[ns]
		if !ok {
			log.Printf("Warning: no manifest found for extension namespace %q", ns)
			continue
		}

		state := &extensionState{
			manifest:    manifest,
			guestStatus: plugin.ExtGuestStopped,
			hostStatus:  plugin.ExtHostUnknown,
		}

		// Check if already active (e.g., from a previous handshake or manual start)
		if isUnitActive(manifest.SystemdUnit) {
			state.guestStatus = plugin.ExtGuestRunning
			s.extensions[ns] = state
			log.Printf("Extension %s: already active (%s)", ns, manifest.SystemdUnit)
			continue
		}

		// Enable and start the systemd unit
		log.Printf("Extension %s: enabling %s", ns, manifest.SystemdUnit)
		if err := systemctlEnableNow(manifest.SystemdUnit); err != nil {
			log.Printf("Extension %s: failed to enable %s: %v", ns, manifest.SystemdUnit, err)
			state.guestStatus = plugin.ExtGuestFailed
		} else {
			state.guestStatus = plugin.ExtGuestRunning
			log.Printf("Extension %s: enabled and started", ns)
		}
		s.extensions[ns] = state
	}

	// Start extension health checking exactly once, even across reconnects.
	if len(s.extensions) > 0 {
		s.healthCheckOnce.Do(func() {
			go s.runExtensionHealthChecks(s.ctx)
		})
	}
}

// collectExtensionHealth returns the current extension health map for inclusion
// in heartbeat payloads. Returns nil if no extensions are enabled.
func (s *Server) collectExtensionHealth() map[string]plugin.ExtensionHealth {
	s.extensionsMu.RLock()
	defer s.extensionsMu.RUnlock()

	if len(s.extensions) == 0 {
		return nil
	}

	result := make(map[string]plugin.ExtensionHealth, len(s.extensions))
	for ns, state := range s.extensions {
		result[ns] = plugin.ExtensionHealth{
			Guest: state.guestStatus,
			Host:  state.hostStatus,
		}
	}
	return result
}

// runExtensionHealthChecks periodically checks each enabled extension's health.
// Guest check: is the systemd unit active?
// Host check: can we reach the host agent end-to-end via bus ping?
func (s *Server) runExtensionHealthChecks(ctx context.Context) {
	// Use a bus client pointing at our own HTTP endpoint for host pings.
	busClient := sdk.NewBusClient(
		fmt.Sprintf("http://127.0.0.1:%d/v1/publish", s.httpPort),
		5*time.Second, // HTTP client timeout; per-ping uses shorter context timeout
	)

	// Run one check immediately so the next heartbeat has accurate data
	// rather than reporting host=unknown until the first ticker fires.
	s.checkExtensions(ctx, busClient)

	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkExtensions(ctx, busClient)
		}
	}
}

// checkExtensions runs health checks for all enabled extensions in parallel.
func (s *Server) checkExtensions(ctx context.Context, busClient *sdk.BusClient) {
	s.extensionsMu.RLock()
	extensions := make(map[string]*extensionState, len(s.extensions))
	maps.Copy(extensions, s.extensions)
	s.extensionsMu.RUnlock()

	type result struct {
		ns    string
		guest string
		host  string
	}

	results := make(chan result, len(extensions))
	var wg sync.WaitGroup

	for ns, state := range extensions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := result{ns: ns}

			// Guest check: is the systemd unit active?
			if isUnitActive(state.manifest.SystemdUnit) {
				r.guest = plugin.ExtGuestRunning
			} else if isUnitFailed(state.manifest.SystemdUnit) {
				r.guest = plugin.ExtGuestFailed
			} else {
				r.guest = plugin.ExtGuestStopped
			}

			// Host check: only if guest is running
			if r.guest == plugin.ExtGuestRunning {
				pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				if err := busClient.Ping(pingCtx, ns, 2*time.Second); err != nil {
					r.host = plugin.ExtHostUnreachable
				} else {
					r.host = plugin.ExtHostConnected
				}
			} else {
				r.host = plugin.ExtHostUnknown
			}

			results <- r
		}()
	}

	wg.Wait()
	close(results)

	s.extensionsMu.Lock()
	for r := range results {
		if state, ok := s.extensions[r.ns]; ok {
			state.guestStatus = r.guest
			state.hostStatus = r.host
		}
	}
	s.extensionsMu.Unlock()
}

// systemctlTimeout is the maximum time to wait for a systemctl command.
const systemctlTimeout = 10 * time.Second

// isUnitActive checks if a systemd unit is currently active.
func isUnitActive(unit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}

// isUnitFailed checks if a systemd unit has failed.
func isUnitFailed(unit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "is-failed", "--quiet", unit)
	return cmd.Run() == nil
}

// systemctlEnableNow enables and starts a systemd unit.
func systemctlEnableNow(unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "enable", "--now", unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
