package vmutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// maxCredentialFileSize is the maximum allowed size for a single file in a
// credential tar archive.
const maxCredentialFileSize = 10 * 1024 * 1024 // 10 MB

// maxCredentialArchiveSize is the maximum allowed total size for a credential
// tar archive streamed from the VM.
const maxCredentialArchiveSize = 50 * 1024 * 1024 // 50 MB

// CredentialNotifyListener maintains a persistent connection to a VM agent's
// notification port, receives FileChanged messages, and pulls changed files
// back to the host.
type CredentialNotifyListener struct {
	agent       *AgentClient
	credentials map[string]config.MountConfig
	watcher     *CredentialWatcher
	name        string
}

// NewCredentialNotifyListener creates a new notification listener.
// The credentials map should contain only tar-transferred credentials that need
// bidirectional sync. VirtioFS-mounted credentials are live mounts and must not
// be included — syncing them via tar causes permission errors and data races.
func NewCredentialNotifyListener(agent *AgentClient, credentials map[string]config.MountConfig, watcher *CredentialWatcher) *CredentialNotifyListener {
	return &CredentialNotifyListener{
		agent:       agent,
		credentials: credentials,
		watcher:     watcher,
	}
}

// SetName sets the listener name for logging. Must be called before PullChangedFiles.
func (nl *CredentialNotifyListener) SetName(name string) {
	nl.name = name
}

// PullChangedFiles pulls the specified changed credential files from the VM
// to the host. This is called when the agent reports credential file changes.
func (nl *CredentialNotifyListener) PullChangedFiles(credName string, files []string) error {
	mount, ok := nl.credentials[credName]
	if !ok {
		return fmt.Errorf("unknown credential %q", credName)
	}

	target := mount.Target

	var escapedFiles []string
	for _, f := range files {
		cleaned := filepath.Clean(f)
		if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			log.Printf("[%s] Skipping suspicious path: %s", nl.name, f)
			continue
		}
		if mount.MatchesExclude(cleaned) {
			continue
		}
		escapedFiles = append(escapedFiles, cleaned)
	}

	if len(escapedFiles) == 0 {
		return nil
	}

	// Ephemeral files (e.g. SQLite WAL, git lock files) may be deleted between
	// the fsnotify event and this tar invocation. --ignore-failed-read makes tar
	// skip missing files instead of exiting fatally with code 2.
	args := []string{"tar", "--ignore-failed-read", "-czf", "-", "-C", target}
	args = append(args, escapedFiles...)

	tarBuf := &limitedBuffer{max: maxCredentialArchiveSize}
	var stderrBuf bytes.Buffer
	opts := backend.ExecOptions{
		Cmd:        args,
		Stdout:     NopWriteCloser(tarBuf),
		Stderr:     NopWriteCloser(&stderrBuf),
		WorkingDir: "/",
		TTY:        false,
	}

	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	if err := nl.agent.Exec(execCtx, opts); err != nil {
		// With --ignore-failed-read, tar exits 1 when files vanish between
		// notification and archival. This is expected for ephemeral files
		// (WAL, lock files) and the archive still contains surviving files.
		var exitErr *ExitError
		if errors.As(err, &exitErr) && exitErr.Code == 1 {
			log.Printf("[%s] tar exited with code 1 for %q (some files missing), continuing with partial archive", nl.name, credName)
		} else {
			return fmt.Errorf("failed to tar changed files: %w (stderr: %s)", err, stderrBuf.String())
		}
	}

	// All notified files may have been removed before tar ran (e.g. a
	// transient WAL file that was checkpointed away). Nothing to extract.
	if tarBuf.buf.Len() == 0 {
		return nil
	}

	if nl.watcher != nil {
		nl.watcher.SuppressEcho(nl.name, credName)
	}

	source := mount.Source
	if err := extractTarToHost(tarBuf.buf.Bytes(), source); err != nil {
		return fmt.Errorf("failed to extract to host: %w", err)
	}

	return nil
}

// limitedBuffer wraps a bytes.Buffer and caps total bytes written.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	if lb.buf.Len()+len(p) > lb.max {
		return 0, fmt.Errorf("credential archive exceeds %d byte limit", lb.max)
	}
	return lb.buf.Write(p)
}

// securePath validates that the resolved target path stays within destDir.
//
// Note: there is a TOCTOU window between the symlink resolution and the
// subsequent file operation.  This is acceptable because the destination
// directories are server-configured credential paths under the host
// operator's control, not user-writable locations.
func securePath(destDir, cleanName string) (string, error) {
	targetPath := filepath.Join(destDir, cleanName)
	parentDir := filepath.Dir(targetPath)
	resolvedParent, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		resolvedParent, err = filepath.EvalSymlinks(destDir)
		if err != nil {
			return "", fmt.Errorf("cannot resolve destination: %w", err)
		}
		targetPath = filepath.Join(resolvedParent, cleanName)
	} else {
		targetPath = filepath.Join(resolvedParent, filepath.Base(targetPath))
	}

	resolvedDest, _ := filepath.EvalSymlinks(destDir)
	if !strings.HasPrefix(targetPath, resolvedDest+string(filepath.Separator)) && targetPath != resolvedDest {
		return "", fmt.Errorf("path escapes destination: %s", cleanName)
	}
	return targetPath, nil
}

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

		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			continue
		}

		targetPath, err := securePath(destDir, cleanName)
		if err != nil {
			log.Printf("Skipping path %q: %v", header.Name, err)
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size > maxCredentialFileSize {
				log.Printf("Skipping oversized file %q: %d bytes exceeds %d limit", header.Name, header.Size, maxCredentialFileSize)
				continue
			}
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
			continue
		}
	}

	return nil
}
