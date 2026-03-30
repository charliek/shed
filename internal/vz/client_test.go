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

// newTestCredMgr creates a CredentialManager with nil server config for tests.
func newTestCredMgr() *vmutil.CredentialManager {
	return vmutil.NewCredentialManager(nil)
}

func TestBuildEnvForGit(t *testing.T) {
	serverCfg := &config.ServerConfig{
		EnvVars: map[string]string{
			"GITHUB_TOKEN": "ghp_abc123",
			"GIT_AUTHOR":   "test",
		},
	}

	env := vmutil.BuildEnvForGit(serverCfg)

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
	env := vmutil.BuildEnvForGit(nil)
	if len(env) != 0 {
		t.Errorf("expected empty env for nil serverCfg, got %v", env)
	}
}

func TestBuildEnvForGitNoEnvVars(t *testing.T) {
	serverCfg := &config.ServerConfig{
		EnvVars: map[string]string{},
	}
	env := vmutil.BuildEnvForGit(serverCfg)
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
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
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
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
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
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
	}

	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestCredentialManagerNoServerCfg(t *testing.T) {
	// Creating a CredentialManager with nil serverCfg should not panic
	credMgr := vmutil.NewCredentialManager(nil)
	// Operations should not panic
	credMgr.StopListener("test")
	credMgr.Close()
}

func TestCredentialManagerNoWritableCredentials(t *testing.T) {
	// HasWritableCredentials should return false for empty/readonly credentials
	if vmutil.HasWritableCredentials(nil) {
		t.Error("expected false for nil credentials")
	}
	if vmutil.HasWritableCredentials(map[string]config.MountConfig{}) {
		t.Error("expected false for empty credentials")
	}
	if vmutil.HasWritableCredentials(map[string]config.MountConfig{
		"git": {ReadOnly: true},
	}) {
		t.Error("expected false for read-only credentials")
	}
}

func TestStopListenerNoOp(t *testing.T) {
	credMgr := vmutil.NewCredentialManager(nil)
	// Should not panic when stopping a non-existent listener
	credMgr.StopListener("nonexistent")
}

func TestGetShedNotFound(t *testing.T) {
	cfg := &config.VZConfig{
		InstanceDir: t.TempDir(),
	}
	client := &Client{
		cfg:     cfg,
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
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

func TestClassifyCredentialsDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	creds := map[string]config.MountConfig{
		"ssh": {Source: tmpDir, Target: "/home/shed/.ssh", ReadOnly: true},
	}

	virtioFS, tarOnly := vmutil.ClassifyCredentials(creds)

	if len(virtioFS) != 1 {
		t.Fatalf("expected 1 VirtioFS credential, got %d", len(virtioFS))
	}
	if _, ok := virtioFS["ssh"]; !ok {
		t.Error("expected 'ssh' in VirtioFS map")
	}
	if len(tarOnly) != 0 {
		t.Errorf("expected 0 tar-only credentials, got %d", len(tarOnly))
	}
}

func TestClassifyCredentialsSingleFile(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(tmpFile, []byte("[user]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	creds := map[string]config.MountConfig{
		"git": {Source: tmpFile, Target: "/home/shed/.gitconfig", ReadOnly: true},
	}

	virtioFS, tarOnly := vmutil.ClassifyCredentials(creds)

	if len(virtioFS) != 0 {
		t.Errorf("expected 0 VirtioFS credentials, got %d", len(virtioFS))
	}
	if len(tarOnly) != 1 {
		t.Fatalf("expected 1 tar-only credential, got %d", len(tarOnly))
	}
	if _, ok := tarOnly["git"]; !ok {
		t.Error("expected 'git' in tarOnly map")
	}
}

func TestClassifyCredentialsMissing(t *testing.T) {
	creds := map[string]config.MountConfig{
		"gone": {Source: "/nonexistent/path", Target: "/home/shed/.gone"},
	}

	virtioFS, tarOnly := vmutil.ClassifyCredentials(creds)

	if len(virtioFS) != 0 {
		t.Errorf("expected 0 VirtioFS credentials, got %d", len(virtioFS))
	}
	if len(tarOnly) != 0 {
		t.Errorf("expected 0 tar-only credentials, got %d", len(tarOnly))
	}
}

func TestClassifyCredentialsMixed(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(tmpFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	creds := map[string]config.MountConfig{
		"dir_cred":  {Source: tmpDir, Target: "/home/shed/.dir"},
		"file_cred": {Source: tmpFile, Target: "/home/shed/.file"},
		"missing":   {Source: "/nonexistent", Target: "/home/shed/.missing"},
	}

	virtioFS, tarOnly := vmutil.ClassifyCredentials(creds)

	if len(virtioFS) != 1 {
		t.Fatalf("expected 1 VirtioFS credential, got %d", len(virtioFS))
	}
	if _, ok := virtioFS["dir_cred"]; !ok {
		t.Error("expected 'dir_cred' in VirtioFS map")
	}
	if len(tarOnly) != 1 {
		t.Fatalf("expected 1 tar-only credential, got %d", len(tarOnly))
	}
	if _, ok := tarOnly["file_cred"]; !ok {
		t.Error("expected 'file_cred' in tarOnly map")
	}
}

func TestClassifyCredentialsWithExcludes(t *testing.T) {
	tmpDir := t.TempDir()
	creds := map[string]config.MountConfig{
		"claude": {
			Source:   tmpDir,
			Target:   "/home/shed/.claude",
			ReadOnly: false,
			Exclude:  []string{"debug/*", "cache/*"},
		},
	}

	virtioFS, tarOnly := vmutil.ClassifyCredentials(creds)

	// Directory credentials with excludes should still use VirtioFS
	// (excludes only mattered for tar transfer size, not VirtioFS)
	if len(virtioFS) != 1 {
		t.Fatalf("expected 1 VirtioFS credential, got %d", len(virtioFS))
	}
	if _, ok := virtioFS["claude"]; !ok {
		t.Error("expected 'claude' in VirtioFS map")
	}
	if len(tarOnly) != 0 {
		t.Errorf("expected 0 tar-only credentials, got %d", len(tarOnly))
	}
}

func TestHasWritableCredentials(t *testing.T) {
	tests := []struct {
		name     string
		creds    map[string]config.MountConfig
		expected bool
	}{
		{
			name:     "empty",
			creds:    map[string]config.MountConfig{},
			expected: false,
		},
		{
			name: "readonly_only",
			creds: map[string]config.MountConfig{
				"git": {ReadOnly: true},
			},
			expected: false,
		},
		{
			name: "writable",
			creds: map[string]config.MountConfig{
				"config": {ReadOnly: false},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vmutil.HasWritableCredentials(tt.creds)
			if got != tt.expected {
				t.Errorf("HasWritableCredentials() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestBuildCredentialShares(t *testing.T) {
	creds := map[string]config.MountConfig{
		"ssh": {Source: "/home/user/.ssh", Target: "/home/shed/.ssh"},
		"gh":  {Source: "/home/user/.config/gh", Target: "/home/shed/.config/gh"},
	}

	shares := buildCredentialShares(creds)

	if len(shares) != 2 {
		t.Fatalf("expected 2 shares, got %d", len(shares))
	}

	// Build a map for order-independent checking
	shareMap := make(map[string]string)
	for _, s := range shares {
		shareMap[s.MountTag] = s.SourceDir
	}

	if shareMap["cred-ssh"] != "/home/user/.ssh" {
		t.Error("expected cred-ssh share with source /home/user/.ssh")
	}
	if shareMap["cred-gh"] != "/home/user/.config/gh" {
		t.Error("expected cred-gh share with source /home/user/.config/gh")
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
		vms:     make(map[string]*VM),
		credMgr: newTestCredMgr(),
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
