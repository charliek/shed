package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditEnabled(t *testing.T) {
	clients := &AuthConfig{Mode: AuthModeMTLS, Clients: map[string]ServiceClientConfig{"c": {}}}
	tests := []struct {
		name string
		cfg  ServerConfig
		want bool
	}{
		{"unset", ServerConfig{}, false},
		{"enabled", ServerConfig{Audit: &AuditConfig{Enabled: true}}, true},
		{"block without enabled", ServerConfig{Audit: &AuditConfig{ExecArgv: AuditExecArgvFull}}, false},
		{"clients force it on", ServerConfig{Auth: clients}, true},
		{"clients win over enabled false", ServerConfig{Auth: clients, Audit: &AuditConfig{Enabled: false}}, true},
		{"mtls without clients", ServerConfig{Auth: &AuthConfig{Mode: AuthModeMTLS}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.AuditEnabled(); got != tt.want {
				t.Errorf("AuditEnabled() = %v, want %v", got, tt.want)
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
	if !cfg.AuditEnabled() || !cfg.AuditExecArgvFull() {
		t.Errorf("loaded %+v", cfg.Audit)
	}
	if _, err := LoadServerConfigForCLI(write(base + "audit:\n  exec_argv: some\n")); err == nil || !strings.Contains(err.Error(), "audit.exec_argv") {
		t.Errorf("an unknown exec_argv was accepted: %v", err)
	}
}
