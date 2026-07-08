package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	content := `
server: http://localhost:9090
ssh:
  mode: agent-forward
  approval:
    policy: biometrics
    scope: per-request
    session_ttl: 2h
aws:
  default_role: arn:aws:iam::123:role/dev
  approval:
    policy: approve-all
docker:
  registries: [ghcr.io]
  approval:
    policy: shed-desktop
logging:
  enabled: true
  path: /tmp/test-audit.log
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Server != "http://localhost:9090" {
		t.Errorf("server: got %q, want %q", cfg.Server, "http://localhost:9090")
	}
	if cfg.SSH.Mode != "agent-forward" {
		t.Errorf("ssh.mode: got %q, want %q", cfg.SSH.Mode, "agent-forward")
	}
	if cfg.SSH.Approval.Policy != PolicyBiometrics {
		t.Errorf("ssh.approval.policy: got %q, want %q", cfg.SSH.Approval.Policy, PolicyBiometrics)
	}
	if cfg.SSH.Approval.Scope != "per-request" {
		t.Errorf("ssh.approval.scope: got %q, want %q", cfg.SSH.Approval.Scope, "per-request")
	}
	if cfg.SSH.Approval.SessionTTL != "2h" {
		t.Errorf("ssh.approval.session_ttl: got %q, want %q", cfg.SSH.Approval.SessionTTL, "2h")
	}
	if cfg.AWS.Approval.Policy != PolicyApproveAll {
		t.Errorf("aws.approval.policy: got %q, want %q", cfg.AWS.Approval.Policy, PolicyApproveAll)
	}
	if cfg.Docker.Approval.Policy != PolicyShedDesktop {
		t.Errorf("docker.approval.policy: got %q, want %q", cfg.Docker.Approval.Policy, PolicyShedDesktop)
	}
	if cfg.Logging.Path != "/tmp/test-audit.log" {
		t.Errorf("logging.path: got %q, want %q", cfg.Logging.Path, "/tmp/test-audit.log")
	}
}

func TestLoadConfigAWS(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	content := `
server: http://localhost:8080
aws:
  source_profile: staging
  default_role: arn:aws:iam::123456789012:role/dev
  session_duration: 2h
  cache_refresh_before: 10m
  approval:
    policy: approve-all
  servers:
    mini2:
      sheds:
        sso-app:
          mode: passthrough
        my-service:
          role: arn:aws:iam::123456789012:role/my-service
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.AWS.SourceProfile != "staging" {
		t.Errorf("aws.source_profile: got %q, want %q", cfg.AWS.SourceProfile, "staging")
	}
	if cfg.AWS.DefaultRole != "arn:aws:iam::123456789012:role/dev" {
		t.Errorf("aws.default_role: got %q", cfg.AWS.DefaultRole)
	}
	sheds := cfg.AWS.Servers["mini2"].Sheds
	if got := sheds["sso-app"].Mode; got != AWSModePassthrough {
		t.Errorf("aws.servers.mini2.sheds.sso-app.mode: got %q, want passthrough", got)
	}
	if got := sheds["my-service"].Role; got != "arn:aws:iam::123456789012:role/my-service" {
		t.Errorf("aws.servers.mini2.sheds.my-service.role: got %q", got)
	}
}

func TestValidateAWS(t *testing.T) {
	t.Run("rejects removed aws.sheds with migration message", func(t *testing.T) {
		c := Config{AWS: AWSConfig{Sheds: map[string]ShedAWSConfig{"web": {Role: "arn:aws:iam::123:role/web"}}}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "aws.sheds was removed") {
			t.Fatalf("expected migration error, got %v", err)
		}
	})

	t.Run("rejects unknown top-level mode", func(t *testing.T) {
		c := Config{AWS: AWSConfig{Mode: "bogus"}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "aws.mode") {
			t.Fatalf("expected aws.mode error, got %v", err)
		}
	})

	t.Run("names the offending per-shed mode location", func(t *testing.T) {
		c := Config{AWS: AWSConfig{Servers: map[string]AWSServerConfig{"mini2": {
			Sheds: map[string]ShedAWSConfig{"web": {Mode: "nope"}},
		}}}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "aws.servers.mini2.sheds.web.mode") {
			t.Fatalf("expected located mode error, got %v", err)
		}
	})

	t.Run("accepts valid modes at every level", func(t *testing.T) {
		c := Config{AWS: AWSConfig{
			Mode: AWSModePassthrough,
			Servers: map[string]AWSServerConfig{"mini2": {
				Mode:  AWSModeAssumeRole,
				Sheds: map[string]ShedAWSConfig{"web": {Mode: AWSModePassthrough}},
			}},
		}}
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	content := `server: http://localhost:8080`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.SSH.Mode != "" {
		t.Errorf("ssh.mode default: got %q, want empty", cfg.SSH.Mode)
	}
	// No policy specified anywhere => every extension is deny-all (fail closed).
	if got := cfg.SSH.Approval.EffectivePolicy(); got != PolicyDenyAll {
		t.Errorf("ssh policy default: got %q, want %q", got, PolicyDenyAll)
	}
	if got := cfg.AWS.Approval.EffectivePolicy(); got != PolicyDenyAll {
		t.Errorf("aws policy default: got %q, want %q", got, PolicyDenyAll)
	}
	if got := cfg.Docker.Approval.EffectivePolicy(); got != PolicyDenyAll {
		t.Errorf("docker policy default: got %q, want %q", got, PolicyDenyAll)
	}
	// Biometric scope/ttl defaults exist for when a biometric policy is chosen.
	if cfg.SSH.Approval.Scope != "per-session" {
		t.Errorf("ssh.approval.scope default: got %q, want per-session", cfg.SSH.Approval.Scope)
	}
	if cfg.SSH.Approval.SessionTTL != "4h" {
		t.Errorf("ssh.approval.session_ttl default: got %q, want 4h", cfg.SSH.Approval.SessionTTL)
	}
	if cfg.Logging.Enabled != true {
		t.Error("logging.enabled default: got false, want true")
	}
	if cfg.AWS.SourceProfile != "default" {
		t.Errorf("aws.source_profile default: got %q, want %q", cfg.AWS.SourceProfile, "default")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path.yaml")
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

// An unrecognized policy value is rejected at load time.
func TestLoadConfigRejectsBadPolicy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("ssh:\n  approval:\n    policy: maybe\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected error for unknown ssh.approval.policy")
	}
}

// AWS/Docker do not support the native biometric policies (SSH only).
func TestValidateRejectsBiometricForAWS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AWS.Approval.Policy = PolicyBiometrics
	if err := cfg.Validate(); err == nil {
		t.Fatal("aws biometrics policy should be rejected")
	}
	cfg = DefaultConfig()
	cfg.Docker.Approval.Policy = PolicyBiometricsOrPassword
	if err := cfg.Validate(); err == nil {
		t.Fatal("docker biometrics-or-password policy should be rejected")
	}
}

func TestValidateAcceptsValidPolicies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SSH.Approval.Policy = PolicyBiometricsOrPassword
	cfg.AWS.Approval.Policy = PolicyShedDesktop
	cfg.Docker.Approval.Policy = PolicyApproveAll
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestEffectivePolicyDefaultsToDenyAll(t *testing.T) {
	if got := (ApprovalConfig{}).EffectivePolicy(); got != PolicyDenyAll {
		t.Errorf("empty policy = %q, want %q", got, PolicyDenyAll)
	}
	if got := (ApprovalConfig{Policy: PolicyApproveAll}).EffectivePolicy(); got != PolicyApproveAll {
		t.Errorf("approve-all policy = %q, want %q", got, PolicyApproveAll)
	}
}

// The shipped default config (installed by Homebrew as extensions.yaml) must
// load + validate, and carry the documented defaults.
func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := LoadConfig("../../configs/extensions.example.yaml")
	if err != nil {
		t.Fatalf("example config failed to load/validate: %v", err)
	}
	if cfg.SSH.Approval.Policy != PolicyBiometricsOrPassword {
		t.Errorf("ssh policy = %q, want %q", cfg.SSH.Approval.Policy, PolicyBiometricsOrPassword)
	}
	if cfg.SSH.Approval.SessionTTL != "1h" {
		t.Errorf("ssh session_ttl = %q, want 1h", cfg.SSH.Approval.SessionTTL)
	}
	if cfg.AWS.Approval.Policy != PolicyDenyAll {
		t.Errorf("aws policy = %q, want %q (off by default)", cfg.AWS.Approval.Policy, PolicyDenyAll)
	}
	if cfg.Docker.Approval.Policy != PolicyApproveAll {
		t.Errorf("docker policy = %q, want %q", cfg.Docker.Approval.Policy, PolicyApproveAll)
	}
	if len(cfg.Docker.Registries) != 2 || cfg.Docker.Registries[0] != "index.docker.io" || cfg.Docker.Registries[1] != "ghcr.io" {
		t.Errorf("docker registries = %v, want [index.docker.io ghcr.io]", cfg.Docker.Registries)
	}
}

func TestResolveAllowPassword(t *testing.T) {
	tests := []struct {
		policy string
		want   bool
	}{
		{PolicyBiometrics, false},
		{PolicyBiometricsOrPassword, true},
		{"", true},        // only exact "biometrics" disables the fallback
		{"garbage", true}, // unknown values keep the permissive fallback
	}
	for _, tt := range tests {
		t.Run(tt.policy, func(t *testing.T) {
			if got := resolveAllowPassword(tt.policy); got != tt.want {
				t.Errorf("resolveAllowPassword(%q) = %v, want %v", tt.policy, got, tt.want)
			}
		})
	}
}

// writeConfig writes content to a temp config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A config that still sets the deprecated desktop.* keys must load without
// error — they're warn-and-ignored, not rejected (so old configs keep working).
func TestLoadConfigDeprecatedDesktopKeysIgnored(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
server: http://localhost:8080
desktop:
  enabled: true
  socket_path: ~/somewhere/else.sock
  timeout_ms: 9000
`))
	if err != nil {
		t.Fatalf("deprecated desktop keys should load, got: %v", err)
	}
	// Ignored: the budget comes from approval_timeout (defaulted), not timeout_ms.
	if got, _ := cfg.ApprovalTimeoutDuration(); got != 25*time.Second {
		t.Errorf("approval timeout = %v, want 25s (timeout_ms must be ignored)", got)
	}
}

func TestApprovalTimeout(t *testing.T) {
	// Omitted => defaults to 25s and validates.
	if cfg, err := LoadConfig(writeConfig(t, "server: http://localhost:8080\n")); err != nil {
		t.Fatalf("default config: %v", err)
	} else if d, _ := cfg.ApprovalTimeoutDuration(); d != 25*time.Second {
		t.Errorf("default approval timeout = %v, want 25s", d)
	}

	// Explicit valid value is parsed.
	if cfg, err := LoadConfig(writeConfig(t, "approval_timeout: 40s\n")); err != nil {
		t.Fatalf("valid approval_timeout: %v", err)
	} else if d, _ := cfg.ApprovalTimeoutDuration(); d != 40*time.Second {
		t.Errorf("approval timeout = %v, want 40s", d)
	}

	// Unparseable and non-positive values are rejected at load time.
	for _, bad := range []string{"approval_timeout: nonsense\n", "approval_timeout: 0s\n", "approval_timeout: -5s\n"} {
		if _, err := LoadConfig(writeConfig(t, bad)); err == nil {
			t.Errorf("LoadConfig(%q) should fail validation", strings.TrimSpace(bad))
		}
	}
}
