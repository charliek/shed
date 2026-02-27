//go:build linux
// +build linux

package firecracker

import (
	"context"
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
// pushes them to all running VMs. It handles echo suppression to avoid
// pushing changes back to the VM that originated them.
type CredentialWatcher struct {
	serverCfg *config.ServerConfig
	watcher   *fsnotify.Watcher

	// Registered VMs for pushing changes
	mu  sync.RWMutex
	vms map[string]*watchedVM // VM name → VM info

	// Echo suppression: tracks which VM+credential combos to skip
	echoMu     sync.Mutex
	echoCooldowns map[string]time.Time // "vmName:credName" → expiry time

	// Debounce state
	debounceMu sync.Mutex
	pending    map[string]bool          // credential names with pending changes
	timers     map[string]*time.Timer   // credential name → debounce timer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type watchedVM struct {
	vsock *VsockClient
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

	// Watch writable credential source directories
	for name, mount := range cw.serverCfg.Credentials {
		if mount.ReadOnly {
			continue
		}
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
	cw.cancel()
	if cw.watcher != nil {
		cw.watcher.Close() // unblocks run() select on Events/Errors channels
	}
	cw.wg.Wait()
}

// RegisterVM registers a running VM for credential push notifications.
func (cw *CredentialWatcher) RegisterVM(name string, vsock *VsockClient) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.vms[name] = &watchedVM{vsock: vsock, name: name}
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

// isEchoSuppressed checks if pushing a credential to a VM should be skipped.
func (cw *CredentialWatcher) isEchoSuppressed(vmName, credName string) bool {
	cw.echoMu.Lock()
	defer cw.echoMu.Unlock()
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

			// If a new directory was created, watch it
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := cw.watcher.Add(event.Name); err != nil {
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
// It picks the longest matching source path to avoid ambiguity when credential paths share a prefix.
func (cw *CredentialWatcher) resolveCredential(absPath string) string {
	var bestName string
	var bestLen int
	for name, mount := range cw.serverCfg.Credentials {
		if mount.ReadOnly {
			continue
		}
		if strings.HasPrefix(absPath, mount.Source+"/") || absPath == mount.Source {
			if len(mount.Source) > bestLen {
				bestName = name
				bestLen = len(mount.Source)
			}
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

		ct := NewCredentialTransfer(vm.vsock, cw.serverCfg)
		if err := ct.transferCredential(cw.ctx, credName, mount); err != nil {
			log.Printf("[%s] Failed to push credential %q: %v", vm.name, credName, err)
		} else {
			log.Printf("[%s] Pushed credential %q", vm.name, credName)
		}
	}
}
