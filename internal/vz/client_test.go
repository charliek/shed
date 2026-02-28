//go:build darwin
// +build darwin

package vz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
)

func TestBuildEnvForGit(t *testing.T) {
	client := &Client{
		serverCfg: &config.ServerConfig{
			EnvVars: map[string]string{
				"GITHUB_TOKEN": "ghp_abc123",
				"GIT_AUTHOR":   "test",
			},
		},
	}

	env := client.buildEnvForGit()

	if len(env) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(env))
	}

	envMap := make(map[string]bool)
	for _, e := range env {
		envMap[e] = true
	}

	if !envMap["GITHUB_TOKEN=ghp_abc123"] {
		t.Error("expected GITHUB_TOKEN in env")
	}
	if !envMap["GIT_AUTHOR=test"] {
		t.Error("expected GIT_AUTHOR in env")
	}
}

func TestBuildEnvForGitNilServerCfg(t *testing.T) {
	client := &Client{serverCfg: nil}
	env := client.buildEnvForGit()
	if len(env) != 0 {
		t.Errorf("expected empty env for nil serverCfg, got %v", env)
	}
}

func TestBuildEnvForGitNoEnvVars(t *testing.T) {
	client := &Client{
		serverCfg: &config.ServerConfig{
			EnvVars: map[string]string{},
		},
	}
	env := client.buildEnvForGit()
	if len(env) != 0 {
		t.Errorf("expected empty env for empty EnvVars, got %v", env)
	}
}

func TestGetNetworkEndpoint(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.VZConfig{
		InstanceDir: tmpDir,
	}

	// Create a valid metadata file
	meta := &Metadata{
		Name:     "test-vm",
		Status:   config.StatusRunning,
		Backend:  config.BackendVZ,
		CPUs:     2,
		MemoryMB: 4096,
	}
	meta.Save(tmpDir)

	client := &Client{
		cfg:             cfg,
		vms:             make(map[string]*VM),
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}

	endpoint, err := client.GetNetworkEndpoint(context.Background(), "test-vm")
	if err != nil {
		t.Fatalf("GetNetworkEndpoint() error = %v", err)
	}
	if endpoint != "127.0.0.1" {
		t.Errorf("GetNetworkEndpoint() = %q, want %q", endpoint, "127.0.0.1")
	}
}

func TestGetNetworkEndpointNotFound(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.VZConfig{
		InstanceDir: tmpDir,
	}

	client := &Client{
		cfg:             cfg,
		vms:             make(map[string]*VM),
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}

	_, err := client.GetNetworkEndpoint(context.Background(), "nonexistent")
	if err == nil {
		t.Error("GetNetworkEndpoint() expected error for nonexistent shed")
	}
}

func TestNewClientCreation(t *testing.T) {
	cfg := &config.VZConfig{
		InstanceDir: t.TempDir(),
	}
	serverCfg := &config.ServerConfig{
		Credentials: make(map[string]config.MountConfig),
	}

	client, err := NewClient(cfg, serverCfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned nil")
	}
	if client.cfg != cfg {
		t.Error("client.cfg should reference the provided config")
	}
	if client.serverCfg != serverCfg {
		t.Error("client.serverCfg should reference the provided server config")
	}
}

func TestNewAgentClient(t *testing.T) {
	cfg := &config.VZConfig{
		SocketDir:   "/tmp/test-sockets",
		ConsolePort: 1024,
		HealthPort:  1025,
		NotifyPort:  1026,
	}

	client := &Client{cfg: cfg}
	agent := client.newAgentClient("test-vm")

	if agent == nil {
		t.Fatal("newAgentClient() returned nil")
	}
	if agent.NotifyPort() != 1026 {
		t.Errorf("NotifyPort() = %d, want 1026", agent.NotifyPort())
	}
}

func TestClientClose(t *testing.T) {
	cfg := &config.VZConfig{}
	client := &Client{
		cfg:             cfg,
		vms:             make(map[string]*VM),
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}

	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestStartNotifyListenerNoServerCfg(t *testing.T) {
	client := &Client{
		serverCfg:       nil,
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}
	// Should not panic
	client.startNotifyListener("test", nil)
}

func TestStartNotifyListenerNoWritableCredentials(t *testing.T) {
	client := &Client{
		serverCfg: &config.ServerConfig{
			Credentials: map[string]config.MountConfig{
				"ssh": {Source: "/home/user/.ssh", Target: "/home/shed/.ssh", ReadOnly: true},
			},
		},
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}
	// Should not start a listener since all credentials are read-only
	client.startNotifyListener("test", nil)

	client.mu.Lock()
	count := len(client.notifyListeners)
	client.mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 listeners for read-only credentials, got %d", count)
	}
}

func TestStopNotifyListenerNoOp(t *testing.T) {
	client := &Client{
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}
	// Should not panic when stopping a non-existent listener
	client.stopNotifyListener("nonexistent")
}

func TestGetShedNotFound(t *testing.T) {
	cfg := &config.VZConfig{
		InstanceDir: t.TempDir(),
	}
	client := &Client{
		cfg:             cfg,
		vms:             make(map[string]*VM),
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}

	_, err := client.GetShed(context.Background(), "nonexistent")
	if err == nil {
		t.Error("GetShed() expected error for nonexistent shed")
	}
	expected := fmt.Sprintf("%s: %s", config.ErrShedNotFoundSentinel, "nonexistent")
	if err.Error() != expected {
		t.Errorf("GetShed() error = %q, want %q", err.Error(), expected)
	}
}

func TestCreateShedValidatesResources(t *testing.T) {
	tmpDir := t.TempDir()
	baseRootfs := filepath.Join(tmpDir, "base-rootfs.ext4")
	if err := os.WriteFile(baseRootfs, []byte("rootfs"), 0644); err != nil {
		t.Fatalf("failed to write base rootfs: %v", err)
	}

	client := &Client{
		cfg: &config.VZConfig{
			BaseRootfs:  baseRootfs,
			InstanceDir: tmpDir,
		},
		vms:             make(map[string]*VM),
		notifyListeners: make(map[string]*vmutil.CredentialNotifyListener),
	}

	_, err := client.CreateShed(context.Background(), config.CreateShedRequest{
		Name: "too-many-cpus",
		CPUs: config.MaxVZCPUs + 1,
	})
	if err == nil {
		t.Fatal("expected cpu validation error")
	}
	if RootfsExists(tmpDir, "too-many-cpus") {
		t.Fatal("rootfs should not be created for invalid cpu request")
	}
	if _, statErr := os.Stat(InstanceDir(tmpDir, "too-many-cpus")); !os.IsNotExist(statErr) {
		t.Fatalf("instance dir should not exist for invalid cpu request, stat err: %v", statErr)
	}

	_, err = client.CreateShed(context.Background(), config.CreateShedRequest{
		Name:     "too-little-memory",
		MemoryMB: 64,
	})
	if err == nil {
		t.Fatal("expected memory validation error")
	}
	if RootfsExists(tmpDir, "too-little-memory") {
		t.Fatal("rootfs should not be created for invalid memory request")
	}
	if _, statErr := os.Stat(InstanceDir(tmpDir, "too-little-memory")); !os.IsNotExist(statErr) {
		t.Fatalf("instance dir should not exist for invalid memory request, stat err: %v", statErr)
	}
}
