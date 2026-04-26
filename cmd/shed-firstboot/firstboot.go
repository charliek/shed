//go:build linux

// Package main implements shed-firstboot, an early-boot oneshot that ensures
// the guest's identity matches the shed name passed via kernel cmdline.
// It runs before D-Bus, journald, sshd, and shed-agent so SSH host keys
// and the hostname are correct before any service caches them.
//
// machine-id is intentionally NOT touched here: the rootfs Dockerfile
// symlinks /etc/machine-id to /run/machine-id (transient tmpfs), so PID 1
// generates a fresh value at every VM boot and nothing persists to disk.
// Doing the regen at the firstboot layer would fight that mechanism (the
// `systemd-machine-id-setup` command pulls from /var/lib/dbus/machine-id
// when /etc/machine-id is empty, and the symlink chain handles the same
// concern more cleanly via systemd's transient machine-id machinery).
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
	cmdlinePath  string
	sshKeyGlob   string
	hostnamePath string
	identityPath string
	runCommand   func(name string, args ...string) error
}

// defaultCfg is the production filesystem layout.
func defaultCfg() firstbootCfg {
	return firstbootCfg{
		cmdlinePath:  "/proc/cmdline",
		sshKeyGlob:   "/etc/ssh/ssh_host_*",
		hostnamePath: "/etc/hostname",
		identityPath: "/var/lib/shed/identity.json",
		runCommand:   runRealCommand,
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

// regenerateIdentity sets the hostname, then removes and re-derives SSH host
// keys. Hostname must be set BEFORE `ssh-keygen -A` so the new keys' comment
// field reflects the spawned shed's name; otherwise `ssh-keygen` records the
// source's hostname (still in /etc/hostname at this point) into every cloned
// shed's keys. Failures are returned per step; callers decide whether to
// abort or log.
//
// machine-id is intentionally NOT touched — the Dockerfile makes it transient
// via a symlink to /run/machine-id, so PID 1 generates a fresh value per boot.
func regenerateIdentity(cfg firstbootCfg, name string) error {
	// Hostname BEFORE SSH key generation so the keys' comment matches.
	// Avoid hostnamectl (requires running D-Bus, which is not yet up).
	if err := os.WriteFile(cfg.hostnamePath, []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("write hostname: %w", err)
	}
	if err := cfg.runCommand("hostname", "-F", cfg.hostnamePath); err != nil {
		return fmt.Errorf("apply hostname: %w", err)
	}

	// SSH host keys: remove existing, regenerate via ssh-keygen -A. The
	// hostname call above ensures the new keys' comment field carries the
	// spawned shed's name rather than the source's.
	matches, _ := filepath.Glob(cfg.sshKeyGlob)
	for _, p := range matches {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove ssh host key %s: %w", p, err)
		}
	}
	if err := cfg.runCommand("ssh-keygen", "-A"); err != nil {
		return fmt.Errorf("regen ssh host keys: %w", err)
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
