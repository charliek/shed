package vmutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// CredentialTransfer handles copying credentials from host to VM via the agent.
type CredentialTransfer struct {
	agent     *AgentClient
	serverCfg *config.ServerConfig
}

// NewCredentialTransfer creates a new CredentialTransfer.
func NewCredentialTransfer(agent *AgentClient, serverCfg *config.ServerConfig) *CredentialTransfer {
	return &CredentialTransfer{
		agent:     agent,
		serverCfg: serverCfg,
	}
}

// TransferAll transfers all configured credentials to the VM.
func (ct *CredentialTransfer) TransferAll(ctx context.Context) error {
	if ct.serverCfg == nil || len(ct.serverCfg.Credentials) == 0 {
		return nil
	}

	for name, mount := range ct.serverCfg.Credentials {
		if err := ct.TransferCredential(ctx, name, mount); err != nil {
			log.Printf("Warning: failed to transfer credential %q: %v", name, err)
		}
	}

	return nil
}

// TransferCredential transfers a single credential from host to VM.
func (ct *CredentialTransfer) TransferCredential(ctx context.Context, name string, mount config.MountConfig) error {
	source := mount.Source
	target := mount.Target

	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Credential %q source does not exist: %s", name, source)
			return nil
		}
		return fmt.Errorf("failed to stat source %s: %w", source, err)
	}

	tarData, err := ct.createTarArchive(source, info)
	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}

	extractDir := target
	if !info.IsDir() {
		extractDir = filepath.Dir(target)
	}

	sp := sudoPrefix(extractDir)
	mkdirCmd := fmt.Sprintf("%smkdir -p %q", sp, extractDir)
	mkdirOpts := backend.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", mkdirCmd},
		WorkingDir: "/",
		TTY:        false,
	}
	if err := ct.agent.Exec(ctx, mkdirOpts); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", extractDir, err)
	}

	if !info.IsDir() {
		sourceBasename := filepath.Base(source)
		targetBasename := filepath.Base(target)

		if sourceBasename != targetBasename {
			tempTarget := filepath.Join(extractDir, sourceBasename)
			if err := ct.extractTarInVM(ctx, tarData, extractDir); err != nil {
				return err
			}
			renameCmd := fmt.Sprintf("%smv %q %q", sp, tempTarget, target)
			renameOpts := backend.ExecOptions{
				Cmd:        []string{"/bin/sh", "-c", renameCmd},
				WorkingDir: "/",
				TTY:        false,
			}
			if err := ct.agent.Exec(ctx, renameOpts); err != nil {
				return fmt.Errorf("failed to rename %s to %s: %w", tempTarget, target, err)
			}
			return nil
		}
	}

	return ct.extractTarInVM(ctx, tarData, extractDir)
}

// createTarArchive creates a gzipped tar archive of the source path.
func (ct *CredentialTransfer) createTarArchive(source string, info os.FileInfo) ([]byte, error) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	if info.IsDir() {
		err := filepath.Walk(source, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			relPath, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}

			if relPath == "." {
				return nil
			}

			return ct.addToTar(tw, path, relPath, fi)
		})
		if err != nil {
			return nil, err
		}
	} else {
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

	header.Name = archivePath

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

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

// isSystemPath returns true if the path requires root privileges to write.
func isSystemPath(path string) bool {
	return strings.HasPrefix(path, "/etc/") ||
		strings.HasPrefix(path, "/usr/") ||
		strings.HasPrefix(path, "/var/") ||
		strings.HasPrefix(path, "/opt/") ||
		strings.HasPrefix(path, "/mnt/")
}

// sudoPrefix returns "sudo " for system paths, empty string otherwise.
func sudoPrefix(path string) string {
	if isSystemPath(path) {
		return "sudo "
	}
	return ""
}

// extractTarInVM extracts a gzipped tar archive in the VM via the agent.
func (ct *CredentialTransfer) extractTarInVM(ctx context.Context, tarData []byte, extractDir string) error {
	sp := sudoPrefix(extractDir)
	tarCmd := fmt.Sprintf("%star --no-same-owner -xzpf - -C %q", sp, extractDir)
	if sp != "" {
		tarCmd += fmt.Sprintf(" && %schown -R shed:shed %q", sp, extractDir)
	}

	var outputBuf strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", tarCmd},
		Stdin:      io.NopCloser(bytes.NewReader(tarData)),
		Stdout:     NopWriteCloser(&outputBuf),
		Stderr:     NopWriteCloser(&outputBuf),
		WorkingDir: "/",
		TTY:        false,
	}

	if err := ct.agent.Exec(ctx, opts); err != nil {
		return fmt.Errorf("failed to extract tar in VM: %w (output: %s)", err, outputBuf.String())
	}

	return nil
}
