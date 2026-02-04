package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// CredentialTransfer handles copying credentials from host to VM via vsock.
// Since Firecracker doesn't support virtiofs or 9p filesystem passthrough,
// credentials are copied at create/start time rather than mounted live.
type CredentialTransfer struct {
	vsock     *VsockClient
	serverCfg *config.ServerConfig
}

// NewCredentialTransfer creates a new CredentialTransfer.
func NewCredentialTransfer(vsock *VsockClient, serverCfg *config.ServerConfig) *CredentialTransfer {
	return &CredentialTransfer{
		vsock:     vsock,
		serverCfg: serverCfg,
	}
}

// TransferAll transfers all configured credentials to the VM.
// Errors are logged but don't fail the operation (matches Docker behavior of optional mounts).
func (ct *CredentialTransfer) TransferAll(ctx context.Context) error {
	if ct.serverCfg == nil || len(ct.serverCfg.Credentials) == 0 {
		return nil
	}

	for name, mount := range ct.serverCfg.Credentials {
		if err := ct.transferCredential(ctx, name, mount); err != nil {
			// Log warning but continue (matches Docker behavior of optional mounts)
			log.Printf("Warning: failed to transfer credential %q: %v", name, err)
		}
	}

	return nil
}

// transferCredential transfers a single credential from host to VM.
func (ct *CredentialTransfer) transferCredential(ctx context.Context, name string, mount config.MountConfig) error {
	source := mount.Source
	target := mount.Target

	// Check if source exists
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Credential %q source does not exist: %s", name, source)
			return nil // Not an error, just skip
		}
		return fmt.Errorf("failed to stat source %s: %w", source, err)
	}

	// Create tar archive of the source
	tarData, err := ct.createTarArchive(source, info)
	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}

	// Determine extraction directory and create it
	extractDir := target
	if !info.IsDir() {
		// For files, extract to the parent directory
		extractDir = filepath.Dir(target)
	}

	// First, ensure the target directory exists in the VM
	mkdirCmd := fmt.Sprintf("mkdir -p %q", extractDir)
	mkdirOpts := backend.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", mkdirCmd},
		WorkingDir: "/",
		TTY:        false,
	}
	if err := ct.vsock.Exec(ctx, mkdirOpts); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", extractDir, err)
	}

	// For files, we need to handle renaming if the target basename differs from source
	if !info.IsDir() {
		sourceBasename := filepath.Base(source)
		targetBasename := filepath.Base(target)

		if sourceBasename != targetBasename {
			// Extract to temp location then rename
			tempTarget := filepath.Join(extractDir, sourceBasename)
			if err := ct.extractTarInVM(ctx, tarData, extractDir); err != nil {
				return err
			}
			// Rename if source and target basenames differ
			renameCmd := fmt.Sprintf("mv %q %q", tempTarget, target)
			renameOpts := backend.ExecOptions{
				Cmd:        []string{"/bin/sh", "-c", renameCmd},
				WorkingDir: "/",
				TTY:        false,
			}
			if err := ct.vsock.Exec(ctx, renameOpts); err != nil {
				return fmt.Errorf("failed to rename %s to %s: %w", tempTarget, target, err)
			}
			return nil
		}
	}

	// Extract tar archive in the VM
	return ct.extractTarInVM(ctx, tarData, extractDir)
}

// createTarArchive creates a gzipped tar archive of the source path.
func (ct *CredentialTransfer) createTarArchive(source string, info os.FileInfo) ([]byte, error) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	if info.IsDir() {
		// Walk directory and add all files
		err := filepath.Walk(source, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// Get relative path
			relPath, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}

			// Skip the root directory itself
			if relPath == "." {
				return nil
			}

			return ct.addToTar(tw, path, relPath, fi)
		})
		if err != nil {
			return nil, err
		}
	} else {
		// Single file - use basename as the name in the archive
		if err := ct.addToTar(tw, source, filepath.Base(source), info); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// addToTar adds a file or directory entry to the tar archive.
func (ct *CredentialTransfer) addToTar(tw *tar.Writer, sourcePath, archivePath string, fi os.FileInfo) error {
	// Handle symlinks
	var link string
	if fi.Mode()&os.ModeSymlink != 0 {
		var err error
		link, err = os.Readlink(sourcePath)
		if err != nil {
			return err
		}
	}

	header, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}

	// Use the relative/archive path
	header.Name = archivePath

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	// Write file content if it's a regular file
	if fi.Mode().IsRegular() {
		f, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		defer f.Close()

		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
	}

	return nil
}

// MaxBase64CommandSize is the maximum size for base64-encoded data passed via shell arguments.
// Shell argument limits vary by system (128KB-256KB typically), so we use a conservative limit.
const MaxBase64CommandSize = 100 * 1024 // 100KB

// extractTarInVM extracts a gzipped tar archive in the VM via vsock.
func (ct *CredentialTransfer) extractTarInVM(ctx context.Context, tarData []byte, extractDir string) error {
	// Encode tar data as base64 to pass through command line safely
	// This avoids issues with stdin piping through vsock
	encoded := base64.StdEncoding.EncodeToString(tarData)

	// Check if encoded data exceeds shell argument limits
	if len(encoded) > MaxBase64CommandSize {
		return fmt.Errorf("credential data too large: %d bytes encoded exceeds %d byte limit", len(encoded), MaxBase64CommandSize)
	}

	// Decode and extract in one command, then fix ownership
	// The -p flag preserves permissions, but we need to fix ownership
	// because files extracted from tar have original owner/group which
	// may not exist or be appropriate in the VM
	tarCmd := fmt.Sprintf("echo %s | base64 -d | tar -xzpf - -C %q && chown -R root:root %q", encoded, extractDir, extractDir)

	var outputBuf strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", tarCmd},
		Stdout:     &nopWriteCloser{&outputBuf},
		Stderr:     &nopWriteCloser{&outputBuf},
		WorkingDir: "/",
		TTY:        false,
	}

	if err := ct.vsock.Exec(ctx, opts); err != nil {
		return fmt.Errorf("failed to extract tar in VM: %w (output: %s)", err, outputBuf.String())
	}

	return nil
}
