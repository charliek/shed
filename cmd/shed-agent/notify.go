//go:build linux
// +build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// credentialWatcher watches credential directories for changes and sends
// FileChanged notifications to the host over the notification connection.
type credentialWatcher struct {
	// credentials maps credential name → target path in the VM
	credentials map[string]string
	// excludes maps credential name → exclude glob patterns
	excludes map[string][]string
	conn     net.Conn
	writeMu  sync.Mutex

	watcher *fsnotify.Watcher

	// debounce state: per-credential pending file sets
	mu      sync.Mutex
	pending map[string]map[string]bool // credential → set of relative paths
	timers  map[string]*time.Timer     // credential → debounce timer
}

const debounceInterval = 500 * time.Millisecond

// newCredentialWatcher creates a watcher for the given credential paths.
func newCredentialWatcher(conn net.Conn, credentials map[string]string, excludes map[string][]string) (*credentialWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if excludes == nil {
		excludes = make(map[string][]string)
	}

	cw := &credentialWatcher{
		credentials: credentials,
		excludes:    excludes,
		conn:        conn,
		watcher:     watcher,
		pending:     make(map[string]map[string]bool),
		timers:      make(map[string]*time.Timer),
	}

	return cw, nil
}

// start begins watching all credential directories. Blocks until done is closed
// or an unrecoverable error occurs.
func (cw *credentialWatcher) start(done <-chan struct{}) {
	// Build reverse lookup: path → credential name
	pathToName := make(map[string]string)
	for name, target := range cw.credentials {
		pathToName[target] = name
		if err := cw.addRecursiveWatch(target); err != nil {
			log.Printf("Warning: failed to watch credential %q at %s: %v", name, target, err)
		}
	}

	for {
		select {
		case <-done:
			cw.watcher.Close()
			return

		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}

			// Only care about writes and creates
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			// Find which credential this path belongs to
			credName, relPath := cw.resolveCredential(event.Name, pathToName)
			if credName == "" {
				continue
			}

			// If a new directory was created, watch it too
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := cw.addRecursiveWatch(event.Name); err != nil {
						log.Printf("Failed to watch new directory %s: %v", event.Name, err)
					}
				}
			}

			cw.debounceSend(credName, relPath)

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error: %v", err)
		}
	}
}

// addRecursiveWatch adds watches on the target and all subdirectories.
func (cw *credentialWatcher) addRecursiveWatch(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if path == root {
				return fmt.Errorf("cannot access credential root %s: %w", root, err)
			}
			return nil // skip inaccessible subdirectories
		}
		if info.IsDir() {
			if watchErr := cw.watcher.Add(path); watchErr != nil {
				log.Printf("Warning: failed to add watch on %s: %v", path, watchErr)
			}
		}
		return nil
	})
}

// resolveCredential finds the credential name and relative path for an absolute file path.
// It picks the longest matching prefix to avoid ambiguity when credential paths share a prefix.
func (cw *credentialWatcher) resolveCredential(absPath string, pathToName map[string]string) (string, string) {
	var bestName, bestTarget string
	for target, name := range pathToName {
		if strings.HasPrefix(absPath, target+"/") {
			if len(target) > len(bestTarget) {
				bestName = name
				bestTarget = target
			}
		}
		// Exact match (single file credential)
		if absPath == target {
			return name, filepath.Base(absPath)
		}
	}
	if bestName != "" {
		rel, _ := filepath.Rel(bestTarget, absPath)
		return bestName, rel
	}
	return "", ""
}

// debounceSend accumulates changed files and sends after debounce interval.
func (cw *credentialWatcher) debounceSend(credName, relPath string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.pending[credName] == nil {
		cw.pending[credName] = make(map[string]bool)
	}
	cw.pending[credName][relPath] = true

	// Reset or create debounce timer
	if t, ok := cw.timers[credName]; ok {
		t.Reset(debounceInterval)
	} else {
		cw.timers[credName] = time.AfterFunc(debounceInterval, func() {
			cw.flushCredential(credName)
		})
	}
}

// flushCredential sends the accumulated file changes for a credential.
func (cw *credentialWatcher) flushCredential(credName string) {
	cw.mu.Lock()
	files := cw.pending[credName]
	delete(cw.pending, credName)
	delete(cw.timers, credName)
	cw.mu.Unlock()

	if len(files) == 0 {
		return
	}

	// Ephemeral files (SQLite WAL, git lock files, etc.) may be deleted
	// between the fsnotify event and this flush. Filter them out to avoid
	// sending the host a list of files that no longer exist.
	// Also skip files matching exclude patterns.
	target := cw.credentials[credName]
	patterns := cw.excludes[credName]
	var fileList []string
	for f := range files {
		if agentMatchesExclude(f, patterns) {
			continue
		}
		if _, err := os.Stat(filepath.Join(target, f)); err != nil {
			continue
		}
		fileList = append(fileList, f)
	}
	if len(fileList) == 0 {
		return
	}

	msg := FileChangedMessage{
		Credential: credName,
		Files:      fileList,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal FileChanged message: %v", err)
		return
	}

	cw.writeMu.Lock()
	err = writeMessage(cw.conn, MsgTypeFileChanged, data)
	cw.writeMu.Unlock()
	if err != nil {
		log.Printf("Failed to send FileChanged notification: %v", err)
	} else {
		log.Printf("Sent FileChanged notification: credential=%s files=%v", credName, fileList)
	}
}

// handleNotifyConnection handles a persistent notification connection from the host.
// It reads a NotifySetupMessage, starts fsnotify watchers, and sends FileChanged
// messages when credential files are modified.
func handleNotifyConnection(conn net.Conn) {
	defer conn.Close()

	// Read setup message
	msgType, data, err := readMessage(conn)
	if err != nil {
		if err != io.EOF {
			log.Printf("Failed to read notify setup message: %v", err)
		}
		return
	}

	if msgType != MsgTypeNotifySetup {
		log.Printf("Unexpected message type on notify port: 0x%02x (expected NotifySetup)", msgType)
		return
	}

	var setup NotifySetupMessage
	if err := json.Unmarshal(data, &setup); err != nil {
		log.Printf("Failed to unmarshal NotifySetup message: %v", err)
		return
	}

	if len(setup.Credentials) == 0 {
		log.Printf("NotifySetup: no credentials to watch")
		return
	}

	log.Printf("NotifySetup: watching %d credentials", len(setup.Credentials))
	for name, path := range setup.Credentials {
		log.Printf("  %s → %s", name, path)
	}

	cw, err := newCredentialWatcher(conn, setup.Credentials, setup.Excludes)
	if err != nil {
		log.Printf("Failed to create credential watcher: %v", err)
		return
	}

	// Block until connection closes (host reads from conn will fail on disconnect)
	done := make(chan struct{})
	go func() {
		// Read from connection to detect disconnect
		buf := make([]byte, 1)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				close(done)
				return
			}
		}
	}()

	cw.start(done)
	log.Printf("Notify connection closed")
}

// agentMatchesExclude reports whether relPath matches any of the given glob patterns.
func agentMatchesExclude(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}
	}
	return false
}
