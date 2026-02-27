//go:build linux
// +build linux

package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// CredentialNotifyListener maintains a persistent connection to a VM agent's
// notification port, receives FileChanged messages, and pulls changed files
// back to the host.
type CredentialNotifyListener struct {
	vsock     *VsockClient
	serverCfg *config.ServerConfig
	watcher   *CredentialWatcher // host-side watcher for echo suppression
	name      string             // VM name

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewCredentialNotifyListener creates a new notification listener.
func NewCredentialNotifyListener(vsock *VsockClient, serverCfg *config.ServerConfig, watcher *CredentialWatcher) *CredentialNotifyListener {
	return &CredentialNotifyListener{
		vsock:     vsock,
		serverCfg: serverCfg,
		watcher:   watcher,
	}
}

// Start begins listening for credential change notifications from the VM.
func (nl *CredentialNotifyListener) Start(ctx context.Context, name string) {
	nl.name = name
	nl.ctx, nl.cancel = context.WithCancel(ctx)
	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.run()
	}()
}

// Stop stops the notification listener and waits for it to finish.
func (nl *CredentialNotifyListener) Stop() {
	nl.cancel()
	nl.wg.Wait()
}

// run is the main loop that connects and reconnects to the agent.
func (nl *CredentialNotifyListener) run() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-nl.ctx.Done():
			return
		default:
		}

		err := nl.connectAndListen()
		if err != nil {
			select {
			case <-nl.ctx.Done():
				return
			default:
				log.Printf("[%s] Notify connection error: %v, reconnecting in %v", nl.name, err, backoff)
				select {
				case <-time.After(backoff):
				case <-nl.ctx.Done():
					return
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}
}

// connectAndListen connects to the agent's notify port, sends setup, and listens.
func (nl *CredentialNotifyListener) connectAndListen() error {
	conn, err := nl.vsock.dialWithContext(nl.ctx, nl.vsock.notifyPort)
	if err != nil {
		return fmt.Errorf("failed to dial notify port: %w", err)
	}
	defer conn.Close()

	// Build credential mappings (only writable ones)
	credentials := make(map[string]string)
	for name, mount := range nl.serverCfg.Credentials {
		if mount.ReadOnly {
			continue
		}
		credentials[name] = mount.Target
	}

	if len(credentials) == 0 {
		log.Printf("[%s] No writable credentials to watch", nl.name)
		// Block until context is done
		<-nl.ctx.Done()
		return nil
	}

	// Send setup message
	setup := agentproto.NotifySetupMessage{
		Credentials: credentials,
	}
	setupData, err := json.Marshal(setup)
	if err != nil {
		return fmt.Errorf("failed to marshal setup message: %w", err)
	}
	if err := agentproto.WriteMessage(conn, agentproto.MsgTypeNotifySetup, setupData); err != nil {
		return fmt.Errorf("failed to send setup message: %w", err)
	}

	log.Printf("[%s] Notify connection established, watching %d credentials", nl.name, len(credentials))

	// Close the connection when context is canceled so ReadMessage unblocks.
	go func() {
		<-nl.ctx.Done()
		conn.Close()
	}()

	// Listen for FileChanged messages
	for {
		msgType, data, err := agentproto.ReadMessage(conn)
		if err != nil {
			if nl.ctx.Err() != nil {
				return nil // graceful shutdown
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("connection closed by agent")
			}
			return fmt.Errorf("read error: %w", err)
		}

		if msgType != agentproto.MsgTypeFileChanged {
			log.Printf("[%s] Unexpected message type on notify connection: 0x%02x", nl.name, msgType)
			continue
		}

		var changed agentproto.FileChangedMessage
		if err := json.Unmarshal(data, &changed); err != nil {
			log.Printf("[%s] Failed to unmarshal FileChanged: %v", nl.name, err)
			continue
		}

		log.Printf("[%s] Credential %q changed: %v", nl.name, changed.Credential, changed.Files)

		// Pull changed files from VM to host
		if err := nl.pullChangedFiles(changed.Credential, changed.Files); err != nil {
			log.Printf("[%s] Failed to pull changed files for %q: %v", nl.name, changed.Credential, err)
		}
	}
}

// pullChangedFiles extracts specific files from the VM and writes them to the host.
func (nl *CredentialNotifyListener) pullChangedFiles(credName string, files []string) error {
	mount, ok := nl.serverCfg.Credentials[credName]
	if !ok {
		return fmt.Errorf("unknown credential %q", credName)
	}

	target := mount.Target

	// Build tar command for just the changed files
	var escapedFiles []string
	for _, f := range files {
		// Sanitize file paths to prevent path traversal
		cleaned := filepath.Clean(f)
		if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			log.Printf("[%s] Skipping suspicious path: %s", nl.name, f)
			continue
		}
		escapedFiles = append(escapedFiles, cleaned)
	}

	if len(escapedFiles) == 0 {
		return nil
	}

	// Create tar of just the changed files inside the VM
	args := []string{"tar", "-czf", "-", "-C", target}
	args = append(args, escapedFiles...)

	var tarBuf, stderrBuf bytes.Buffer
	opts := backend.ExecOptions{
		Cmd:        args,
		Stdout:     &nopWriteCloser{&tarBuf},
		Stderr:     &nopWriteCloser{&stderrBuf},
		WorkingDir: "/",
		TTY:        false,
	}

	if err := nl.vsock.Exec(nl.ctx, opts); err != nil {
		return fmt.Errorf("failed to tar changed files: %w (stderr: %s)", err, stderrBuf.String())
	}

	// Notify the watcher to suppress echo for this credential from this VM
	if nl.watcher != nil {
		nl.watcher.SuppressEcho(nl.name, credName)
	}

	// Extract tar to host source directory
	source := mount.Source
	if err := extractTarToHost(tarBuf.Bytes(), source); err != nil {
		return fmt.Errorf("failed to extract to host: %w", err)
	}

	return nil
}

// maxCredentialFileSize is the maximum allowed size for a single file in a
// credential tar archive. Credential files are small (keys, tokens, configs);
// this limit prevents decompression bombs.
const maxCredentialFileSize = 10 * 1024 * 1024 // 10 MB

// extractTarToHost extracts a gzipped tar archive to the given host directory.
func extractTarToHost(tarData []byte, destDir string) error {
	gzr, err := gzip.NewReader(bytes.NewReader(tarData))
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}

		// Sanitize path
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			continue // skip suspicious paths
		}

		targetPath := filepath.Join(destDir, cleanName)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, maxCredentialFileSize)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// skip — credential directories should not contain symlinks
			continue
		}
	}

	return nil
}
