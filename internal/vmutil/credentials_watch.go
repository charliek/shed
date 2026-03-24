package vmutil

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/charliek/shed/internal/config"
)

const (
	hostDebounceInterval = 500 * time.Millisecond
	echoCooldown         = 2 * time.Second
)

// CredentialWatcher watches host credential directories for changes and
// pushes them to all running VMs.
type CredentialWatcher struct {
	serverCfg  *config.ServerConfig
	watchedSet map[string]bool // credentials that should be watched (non-VirtioFS)
	watcher    *fsnotify.Watcher

	mu  sync.RWMutex
	vms map[string]*watchedVM

	echoMu        sync.Mutex
	echoCooldowns map[string]time.Time

	debounceMu sync.Mutex
	pending    map[string]bool
	timers     map[string]*time.Timer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type watchedVM struct {
	agent *AgentClient
	name  string
}

// NewCredentialWatcher creates a new host-side credential watcher.
func NewCredentialWatcher(serverCfg *config.ServerConfig) *CredentialWatcher {
	return &CredentialWatcher{
		serverCfg:     serverCfg,
		vms:           make(map[string]*watchedVM),
		echoCooldowns: make(map[string]time.Time),
		pending:       make(map[string]bool),
		timers:        make(map[string]*time.Timer),
	}
}

// Start begins watching host credential directories.
func (cw *CredentialWatcher) Start(ctx context.Context) error {
	cw.ctx, cw.cancel = context.WithCancel(ctx)

	var err error
	cw.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Only watch single-file (tar-transferred) credentials. Directory credentials
	// use VirtioFS live mounts and don't need host→VM push via tar.
	cw.watchedSet = make(map[string]bool)
	for name, mount := range cw.serverCfg.Credentials {
		if mount.ReadOnly {
			continue
		}
		info, err := os.Stat(mount.Source)
		if err != nil {
			log.Printf("Warning: failed to stat credential %q source %s: %v", name, mount.Source, err)
			continue
		}
		if info.IsDir() {
			continue
		}
		cw.watchedSet[name] = true
		if err := cw.addRecursiveWatch(mount.Source); err != nil {
			log.Printf("Warning: failed to watch credential %q source %s: %v", name, mount.Source, err)
		}
	}

	cw.wg.Add(1)
	go func() {
		defer cw.wg.Done()
		cw.run()
	}()

	return nil
}

// Stop stops watching and waits for completion.
func (cw *CredentialWatcher) Stop() {
	if cw.cancel != nil {
		cw.cancel()
	}
	if cw.watcher != nil {
		cw.watcher.Close()
	}
	cw.wg.Wait()
}

// RegisterVM registers a running VM for credential push notifications.
func (cw *CredentialWatcher) RegisterVM(name string, agent *AgentClient) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.vms[name] = &watchedVM{agent: agent, name: name}
}

// UnregisterVM removes a VM from credential push notifications.
func (cw *CredentialWatcher) UnregisterVM(name string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	delete(cw.vms, name)
}

// SuppressEcho marks that changes for a credential from a specific VM should
// not be pushed back to that VM for the echo cooldown period.
func (cw *CredentialWatcher) SuppressEcho(vmName, credName string) {
	cw.echoMu.Lock()
	defer cw.echoMu.Unlock()
	key := vmName + ":" + credName
	cw.echoCooldowns[key] = time.Now().Add(echoCooldown)
}

// echoPruneThreshold is the map size at which expired entries are pruned.
const echoPruneThreshold = 100

// isEchoSuppressed checks if pushing a credential to a VM should be skipped.
func (cw *CredentialWatcher) isEchoSuppressed(vmName, credName string) bool {
	cw.echoMu.Lock()
	defer cw.echoMu.Unlock()

	// Prune expired entries when the map grows past the threshold.
	if len(cw.echoCooldowns) > echoPruneThreshold {
		now := time.Now()
		for k, exp := range cw.echoCooldowns {
			if now.After(exp) {
				delete(cw.echoCooldowns, k)
			}
		}
	}

	key := vmName + ":" + credName
	expiry, ok := cw.echoCooldowns[key]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(cw.echoCooldowns, key)
		return false
	}
	return true
}

// run is the main event loop watching for file changes.
func (cw *CredentialWatcher) run() {
	for {
		select {
		case <-cw.ctx.Done():
			return

		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}

			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := cw.addRecursiveWatch(event.Name); err != nil {
						log.Printf("Failed to watch new directory %s: %v", event.Name, err)
					}
				}
			}

			credName := cw.resolveCredential(event.Name)
			if credName == "" {
				continue
			}

			cw.debounceSync(credName)

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Host credential watcher error: %v", err)
		}
	}
}

// addRecursiveWatch adds watches on a directory and all subdirectories.
func (cw *CredentialWatcher) addRecursiveWatch(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if path == root {
				return fmt.Errorf("cannot access credential root %s: %w", root, err)
			}
			return nil
		}
		if info.IsDir() {
			if watchErr := cw.watcher.Add(path); watchErr != nil {
				log.Printf("Warning: failed to add watch on %s: %v", path, watchErr)
			}
		}
		return nil
	})
}

// resolveCredential finds which credential a file path belongs to.
// Returns empty string if the path doesn't belong to any credential or
// if the relative path within the credential matches an exclude pattern.
func (cw *CredentialWatcher) resolveCredential(absPath string) string {
	var bestName string
	var bestLen int
	var bestMount config.MountConfig
	for name, mount := range cw.serverCfg.Credentials {
		if mount.ReadOnly {
			continue
		}
		// Skip credentials not in the watched set (e.g., VirtioFS live mounts)
		if cw.watchedSet != nil && !cw.watchedSet[name] {
			continue
		}
		if strings.HasPrefix(absPath, mount.Source+"/") || absPath == mount.Source {
			if len(mount.Source) > bestLen {
				bestName = name
				bestLen = len(mount.Source)
				bestMount = mount
			}
		}
	}
	if bestName == "" {
		return ""
	}
	// Check if the relative path matches an exclude pattern
	if len(bestMount.Exclude) > 0 && absPath != bestMount.Source {
		rel, err := filepath.Rel(bestMount.Source, absPath)
		if err == nil && bestMount.MatchesExclude(rel) {
			return ""
		}
	}
	return bestName
}

// debounceSync schedules a credential sync after the debounce interval.
func (cw *CredentialWatcher) debounceSync(credName string) {
	cw.debounceMu.Lock()
	defer cw.debounceMu.Unlock()

	cw.pending[credName] = true

	if t, ok := cw.timers[credName]; ok {
		t.Reset(hostDebounceInterval)
	} else {
		cw.timers[credName] = time.AfterFunc(hostDebounceInterval, func() {
			cw.syncCredentialToVMs(credName)
		})
	}
}

// syncCredentialToVMs pushes a credential to all registered VMs.
func (cw *CredentialWatcher) syncCredentialToVMs(credName string) {
	cw.debounceMu.Lock()
	delete(cw.pending, credName)
	delete(cw.timers, credName)
	cw.debounceMu.Unlock()

	mount, ok := cw.serverCfg.Credentials[credName]
	if !ok {
		return
	}

	cw.mu.RLock()
	vms := make([]*watchedVM, 0, len(cw.vms))
	for _, vm := range cw.vms {
		vms = append(vms, vm)
	}
	cw.mu.RUnlock()

	for _, vm := range vms {
		if cw.isEchoSuppressed(vm.name, credName) {
			log.Printf("[%s] Skipping echo push for credential %q", vm.name, credName)
			continue
		}

		ct := NewCredentialTransfer(vm.agent, cw.serverCfg)
		if err := ct.TransferCredential(cw.ctx, credName, mount); err != nil {
			log.Printf("[%s] Failed to push credential %q: %v", vm.name, credName, err)
		} else {
			log.Printf("[%s] Pushed credential %q", vm.name, credName)
		}
	}
}
