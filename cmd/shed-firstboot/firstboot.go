//go:build linux

// Package main implements shed-firstboot, an early-boot oneshot that ensures
// the guest's identity matches the shed name passed via kernel cmdline.
// It runs before D-Bus, journald, sshd, and shed-agent so that machine-id
// and SSH host keys are correct before any service caches them.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// firstbootCfg lets tests override paths and the command runner.
type firstbootCfg struct {
	cmdlinePath   string
	machineIDPath string
	sshKeyGlob    string
	hostnamePath  string
	identityPath  string
	runCommand    func(name string, args ...string) error
}

// defaultCfg is the production filesystem layout.
func defaultCfg() firstbootCfg {
	return firstbootCfg{
		cmdlinePath:   "/proc/cmdline",
		machineIDPath: "/etc/machine-id",
		sshKeyGlob:    "/etc/ssh/ssh_host_*",
		hostnamePath:  "/etc/hostname",
		identityPath:  "/var/lib/shed/identity.json",
		runCommand:    runRealCommand,
	}
}

// identity is the schema persisted at /var/lib/shed/identity.json.
type identity struct {
	Name string `json:"name"`
}

// parseShedName extracts the shed.name= value from /proc/cmdline contents.
// Returns the value or an empty string if absent.
func parseShedName(cmdline []byte) string {
	for _, tok := range strings.Fields(string(cmdline)) {
		if name, ok := strings.CutPrefix(tok, "shed.name="); ok {
			return name
		}
	}
	return ""
}

// loadIdentity reads identity.json. Missing or malformed files are treated
// as "no recorded identity"; the caller will regen.
func loadIdentity(path string) (*identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var id identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, nil
	}
	return &id, nil
}

// saveIdentity writes identity.json atomically (tempfile + rename) so a
// crash mid-write never produces a malformed file the next boot would
// re-regen for.
func saveIdentity(path string, id *identity) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".identity-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// regenerateIdentity wipes machine-id, removes existing SSH host keys, sets
// the hostname, and re-derives the regenerated artifacts via systemd helpers.
// Failures are returned per step; callers decide whether to abort or log.
func regenerateIdentity(cfg firstbootCfg, name string) error {
	// machine-id: empty file makes systemd-machine-id-setup generate a fresh one.
	if err := os.WriteFile(cfg.machineIDPath, nil, 0o444); err != nil {
		return fmt.Errorf("clear machine-id: %w", err)
	}
	if err := cfg.runCommand("systemd-machine-id-setup"); err != nil {
		// Fall back to dbus-uuidgen if the systemd helper is unavailable.
		if err2 := cfg.runCommand("dbus-uuidgen", "--ensure="+cfg.machineIDPath); err2 != nil {
			return fmt.Errorf("regen machine-id (systemd-machine-id-setup: %v; dbus-uuidgen: %w)", err, err2)
		}
	}

	// SSH host keys: remove existing, regenerate via ssh-keygen -A.
	matches, _ := filepath.Glob(cfg.sshKeyGlob)
	for _, p := range matches {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove ssh host key %s: %w", p, err)
		}
	}
	if err := cfg.runCommand("ssh-keygen", "-A"); err != nil {
		return fmt.Errorf("regen ssh host keys: %w", err)
	}

	// Hostname: avoid hostnamectl (requires running D-Bus, which is not yet up).
	if err := os.WriteFile(cfg.hostnamePath, []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("write hostname: %w", err)
	}
	if err := cfg.runCommand("hostname", "-F", cfg.hostnamePath); err != nil {
		return fmt.Errorf("apply hostname: %w", err)
	}

	return nil
}

// shouldRegen reports whether identity must be regenerated, given the
// shed name from kernel cmdline and the current identity.json contents.
func shouldRegen(currentName string, recorded *identity) bool {
	if recorded == nil {
		return true
	}
	return recorded.Name != currentName
}

// runFirstboot is the top-level orchestrator. It returns nil on a clean
// no-op (idempotent boot), nil after a successful regen, or an error if
// any step fails.
func runFirstboot(cfg firstbootCfg) error {
	cmdline, err := os.ReadFile(cfg.cmdlinePath)
	if err != nil {
		return fmt.Errorf("read cmdline: %w", err)
	}

	name := parseShedName(cmdline)
	if name == "" {
		// Rootfs is running outside of shed (e.g., manual debug boot).
		// Don't touch identity files; the operator may have set them up
		// intentionally.
		return nil
	}

	recorded, err := loadIdentity(cfg.identityPath)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}

	if !shouldRegen(name, recorded) {
		return nil
	}

	if err := regenerateIdentity(cfg, name); err != nil {
		return fmt.Errorf("regen identity: %w", err)
	}

	if err := saveIdentity(cfg.identityPath, &identity{Name: name}); err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	return nil
}

// runRealCommand is the production command runner. Output goes to systemd's
// journal (inherited from the parent process's stdout/stderr).
func runRealCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
