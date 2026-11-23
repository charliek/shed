package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditEnabled is when the audit log is on, and when it is required
// (a server that cannot open it does not start): on by default in mtls mode
// but not required then (plan 039 §3.6; the lead's ruling: a server that
// starts today still starts).
func TestAuditEnabled(t *testing.T) {
	yes, no := true, false
	clients := &AuthConfig{Mode: AuthModeMTLS, Clients: map[string]ServiceClientConfig{"c": {}}}
	mtls := &AuthConfig{Mode: AuthModeMTLS}
	open := &AuthConfig{Mode: AuthModeOpen}
	tests := []struct {
		name         string
		cfg          ServerConfig
		on, required bool
	}{
		{"unset", ServerConfig{}, false, false},
		{"open mode", ServerConfig{Auth: open}, false, false},
		{"enabled", ServerConfig{Audit: &AuditConfig{Enabled: &yes}}, true, true},
		{"enabled in open mode", ServerConfig{Auth: open, Audit: &AuditConfig{Enabled: &yes}}, true, true},
		{"block without enabled", ServerConfig{Audit: &AuditConfig{ExecArgv: AuditExecArgvFull}}, false, false},
		{"clients force it on", ServerConfig{Auth: clients}, true, true},
		{"clients win over enabled false", ServerConfig{Auth: clients, Audit: &AuditConfig{Enabled: &no}}, true, true},
		{"mtls by default", ServerConfig{Auth: mtls}, true, false},
		{"mtls, a block without enabled", ServerConfig{Auth: mtls, Audit: &AuditConfig{ExecArgv: AuditExecArgvFull}}, true, false},
		{"mtls, enabled", ServerConfig{Auth: mtls, Audit: &AuditConfig{Enabled: &yes}}, true, true},
		{"mtls, enabled false", ServerConfig{Auth: mtls, Audit: &AuditConfig{Enabled: &no}}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.AuditEnabled(); got != tt.on {
				t.Errorf("AuditEnabled() = %v, want %v", got, tt.on)
			}
			if got := tt.cfg.AuditRequired(); got != tt.required {
				t.Errorf("AuditRequired() = %v, want %v", got, tt.required)
			}
		})
	}
	if (&ServerConfig{}).AuditExecArgvFull() {
		t.Error("exec_argv defaults to argv0 and argc")
	}
	if !(&ServerConfig{Audit: &AuditConfig{ExecArgv: "full"}}).AuditExecArgvFull() {
		t.Error("exec_argv: full not honoured")
	}
}

func TestAuditConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"
	cfg, err := LoadServerConfigForCLI(write(base + "audit:\n  enabled: true\n  exec_argv: full\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AuditEnabled() || !cfg.AuditRequired() || !cfg.AuditExecArgvFull() {
		t.Errorf("loaded %+v", cfg.Audit)
	}
	cfg, err = LoadServerConfigForCLI(write(base + "audit:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audit == nil || cfg.Audit.Enabled == nil || *cfg.Audit.Enabled {
		t.Errorf("audit.enabled: false did not load as false: %+v", cfg.Audit)
	}
	if _, err := LoadServerConfigForCLI(write(base + "audit:\n  exec_argv: some\n")); err == nil || !strings.Contains(err.Error(), "audit.exec_argv") {
		t.Errorf("an unknown exec_argv was accepted: %v", err)
	}
}
