//go:build linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/firecracker"
)

const (
	defaultFirecrackerVersion = "v1.14.1"
	firecrackerBinPath        = "/usr/local/bin/firecracker"
	jailerBinPath             = "/usr/local/bin/jailer"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Set up Firecracker infrastructure on this machine",
	Long: `Set up the Firecracker backend infrastructure for shed-server.

This command is idempotent and safe to re-run. It performs:
  - KVM and Docker availability checks
  - Firecracker binary download and installation
  - Directory creation for instances, images, and sockets
  - Bridge network creation and NAT configuration
  - Linux capability assignment for shed-server and firecracker

Requires root privileges.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this command must be run as root (try: sudo shed-server setup)")
	}

	// Step 1: Check KVM
	fmt.Println("=== Checking prerequisites ===")
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not found: KVM is required for Firecracker\n  Enable hardware virtualization in your BIOS/VM settings")
	}
	fmt.Println("KVM: available")

	// Step 2: Check Docker
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found in PATH: Docker is required for VM image management\n  Install Docker CE from https://docs.docker.com/engine/install/ubuntu/")
	}
	fmt.Println("Docker: available")

	// Step 3: Download Firecracker
	fmt.Println()
	fmt.Println("=== Firecracker ===")
	if err := ensureFirecracker(defaultFirecrackerVersion); err != nil {
		return fmt.Errorf("failed to install firecracker: %w", err)
	}

	// Step 4: Create directories
	fmt.Println()
	fmt.Println("=== Directories ===")
	dirs := []string{
		"/var/lib/shed/firecracker/instances",
		"/var/lib/shed/firecracker/images",
		"/var/run/shed/firecracker",
		"/etc/shed",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
		fmt.Printf("Directory: %s\n", dir)
	}

	// Step 5: Bridge network
	fmt.Println()
	fmt.Println("=== Network ===")
	bridgeName, bridgeCIDR, tapPrefix := loadBridgeConfig()

	nm, err := firecracker.NewNetworkManager(bridgeName, bridgeCIDR, tapPrefix)
	if err != nil {
		return fmt.Errorf("invalid network config: %w", err)
	}
	if err := nm.EnsureBridgeExists(); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}
	fmt.Printf("Bridge: %s (%s)\n", bridgeName, bridgeCIDR)

	// Step 6: NAT setup
	if err := nm.SetupNAT(); err != nil {
		return fmt.Errorf("failed to setup NAT: %w", err)
	}
	fmt.Println("NAT: IP forwarding and iptables rules configured")

	// Step 7: Set capabilities
	fmt.Println()
	fmt.Println("=== Capabilities ===")
	for _, path := range []string{firecrackerBinPath, "/usr/local/bin/shed-server"} {
		if _, err := os.Stat(path); err != nil {
			fmt.Printf("Skipping %s (not found)\n", path)
			continue
		}
		cmd := exec.Command("setcap", "cap_net_admin+eip", path)
		if err := cmd.Run(); err != nil {
			fmt.Printf("Warning: failed to set CAP_NET_ADMIN on %s: %v\n", path, err)
		} else {
			fmt.Printf("Set CAP_NET_ADMIN on %s\n", path)
		}
	}

	// Summary
	fmt.Println()
	fmt.Println("=== Setup complete ===")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit /etc/shed/server.yaml (if not already configured)")
	fmt.Println("  2. Pull VM images: sudo shed-server pull-images")
	fmt.Println("  3. Start the server: sudo systemctl start shed-server")
	fmt.Println("  4. Check status: sudo systemctl status shed-server")

	return nil
}

// loadBridgeConfig reads bridge configuration from server config if available,
// falling back to defaults.
func loadBridgeConfig() (bridgeName, bridgeCIDR, tapPrefix string) {
	bridgeName = "shed-br0"
	bridgeCIDR = "172.30.0.1/24"
	tapPrefix = "shed-tap"

	cfg, err := loadConfig()
	if err != nil {
		return
	}
	if cfg.Firecracker != nil {
		if cfg.Firecracker.BridgeName != "" {
			bridgeName = cfg.Firecracker.BridgeName
		}
		if cfg.Firecracker.BridgeCIDR != "" {
			bridgeCIDR = cfg.Firecracker.BridgeCIDR
		}
		if cfg.Firecracker.TAPPrefix != "" {
			tapPrefix = cfg.Firecracker.TAPPrefix
		}
	}
	return
}

// ensureFirecracker downloads and installs the firecracker and jailer binaries
// if they are not already installed at the expected version.
func ensureFirecracker(version string) error {
	// Check if already installed at the right version
	if out, err := exec.Command(firecrackerBinPath, "--version").CombinedOutput(); err == nil {
		if strings.Contains(string(out), strings.TrimPrefix(version, "v")) {
			fmt.Printf("Firecracker %s already installed\n", version)
			return ensureKernel(version)
		}
	}

	arch := runtime.GOARCH
	var fcArch string
	switch arch {
	case "amd64":
		fcArch = "x86_64"
	case "arm64":
		fcArch = "aarch64"
	default:
		return fmt.Errorf("unsupported architecture: %s", arch)
	}

	url := fmt.Sprintf("https://github.com/firecracker-microvm/firecracker/releases/download/%s/firecracker-%s-%s.tgz",
		version, version, fcArch)
	fmt.Printf("Downloading Firecracker %s for %s...\n", version, fcArch)

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Extract to temp directory
	tmpDir, err := os.MkdirTemp("", "shed-firecracker-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(resp.Body, tmpDir); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	// Find and install binaries
	fcPrefix := fmt.Sprintf("firecracker-%s-", version)
	jailerPrefix := fmt.Sprintf("jailer-%s-", version)

	var checksums map[string]string
	if data, err := findAndReadFile(tmpDir, "SHA256SUMS"); err == nil {
		checksums = parseSHA256Sums(data)
	}

	installed := 0
	err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		name := info.Name()

		// Skip debug symbols and yaml files
		if strings.HasSuffix(name, ".debug") || strings.HasSuffix(name, ".yaml") {
			return nil
		}

		var destPath string
		if strings.HasPrefix(name, fcPrefix) {
			destPath = firecrackerBinPath
		} else if strings.HasPrefix(name, jailerPrefix) {
			destPath = jailerBinPath
		} else {
			return nil
		}

		// Verify checksum if available
		if checksums != nil {
			if expected, ok := checksums[name]; ok {
				actual, hashErr := hashFile(path)
				if hashErr != nil {
					return fmt.Errorf("failed to hash %s: %w", name, hashErr)
				}
				if actual != expected {
					return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", name, expected, actual)
				}
				fmt.Printf("Checksum verified: %s\n", name)
			}
		}

		if err := copyFile(path, destPath, 0755); err != nil {
			return fmt.Errorf("failed to install %s: %w", destPath, err)
		}
		fmt.Printf("Installed: %s\n", destPath)
		installed++
		return nil
	})
	if err != nil {
		return err
	}

	if installed == 0 {
		return fmt.Errorf("no firecracker binaries found in archive")
	}

	return ensureKernel(version)
}

// ensureKernel downloads the CI fallback kernel if one doesn't exist.
func ensureKernel(version string) error {
	kernelPath := config.DefaultFirecrackerImagesDir + "/vmlinux"

	if _, err := os.Stat(kernelPath); err == nil {
		fmt.Printf("Kernel already exists at %s\n", kernelPath)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(kernelPath), 0755); err != nil {
		return err
	}

	arch := runtime.GOARCH
	var fcArch string
	switch arch {
	case "amd64":
		fcArch = "x86_64"
	case "arm64":
		fcArch = "aarch64"
	default:
		return fmt.Errorf("unsupported architecture: %s", arch)
	}

	// The v1.9 in the URL is the CI artifact path, not the Firecracker version.
	url := fmt.Sprintf("https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.9/%s/vmlinux-6.1.102", fcArch)
	fmt.Printf("Downloading CI kernel (fallback)...\n")
	fmt.Println("  (For full Docker support, build a custom kernel: ./scripts/build-firecracker-kernel.sh)")

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return fmt.Errorf("kernel download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kernel download failed: HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(kernelPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(kernelPath)
		return fmt.Errorf("kernel download failed: %w", err)
	}

	fmt.Printf("Downloaded kernel to %s\n", kernelPath)
	return nil
}

// extractTarGz extracts a .tar.gz stream to the given directory.
func extractTarGz(r io.Reader, dest string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dest, filepath.Clean(hdr.Name))

		// Prevent path traversal
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// findAndReadFile walks a directory looking for a file by name and returns its contents.
func findAndReadFile(dir, name string) ([]byte, error) {
	var result []byte
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == name {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			result = data
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("%s not found in %s", name, dir)
	}
	return result, nil
}

// parseSHA256Sums parses a SHA256SUMS file into a map of filename → hash.
func parseSHA256Sums(data []byte) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		hash := parts[0]
		name := strings.TrimPrefix(parts[1], "./")
		sums[name] = hash
	}
	return sums
}

// hashFile computes the SHA256 hash of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile copies a file from src to dst with the given permissions.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
