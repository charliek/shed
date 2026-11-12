package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecConfigDefaults(t *testing.T) {
	for _, cfg := range []*ServerConfig{{}, {Exec: &ExecConfig{}}} {
		if got := cfg.ExecMaxStdinBytes(); got != 1<<20 {
			t.Errorf("max_stdin_bytes default %d", got)
		}
		if got := cfg.ExecMaxTimeout(); got != time.Hour {
			t.Errorf("max_timeout default %s", got)
		}
		if got := cfg.ExecMaxConcurrentPerShed(); got != 8 {
			t.Errorf("max_concurrent_per_shed default %d", got)
		}
		if err := cfg.validateExec(); err != nil {
			t.Errorf("defaults refused: %v", err)
		}
	}
	cfg := &ServerConfig{Exec: &ExecConfig{MaxStdinBytes: 10, MaxTimeout: Duration(2 * time.Minute), MaxConcurrentPerShed: 3}}
	if cfg.ExecMaxStdinBytes() != 10 || cfg.ExecMaxTimeout() != 2*time.Minute || cfg.ExecMaxConcurrentPerShed() != 3 {
		t.Errorf("set values not used: %d %s %d", cfg.ExecMaxStdinBytes(), cfg.ExecMaxTimeout(), cfg.ExecMaxConcurrentPerShed())
	}
}

func TestValidateExec(t *testing.T) {
	tests := []struct {
		name    string
		exec    ExecConfig
		wantErr string
	}{
		{"all set", ExecConfig{MaxStdinBytes: 1, MaxTimeout: Duration(time.Second), MaxConcurrentPerShed: 1}, ""},
		{"negative stdin", ExecConfig{MaxStdinBytes: -1}, "exec.max_stdin_bytes"},
		{"negative concurrency", ExecConfig{MaxConcurrentPerShed: -1}, "exec.max_concurrent_per_shed"},
		{"timeout under 1s", ExecConfig{MaxTimeout: Duration(500 * time.Millisecond)}, "exec.max_timeout"},
		{"negative timeout", ExecConfig{MaxTimeout: Duration(-time.Second)}, "exec.max_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ServerConfig{Exec: &tt.exec}
			err := cfg.validateExec()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}

// TestExecConfigFromYAML: the block loads through the CLI loader and a bad
// value is refused by ValidateNoHostCoupling.
func TestExecConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"
	cfg, err := LoadServerConfigForCLI(write(base + "exec:\n  max_stdin_bytes: 2048\n  max_timeout: 10m\n  max_concurrent_per_shed: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecMaxStdinBytes() != 2048 || cfg.ExecMaxTimeout() != 10*time.Minute || cfg.ExecMaxConcurrentPerShed() != 2 {
		t.Errorf("loaded %+v", cfg.Exec)
	}
	if _, err := LoadServerConfigForCLI(write(base + "exec:\n  max_timeout: 100ms\n")); err == nil || !strings.Contains(err.Error(), "exec.max_timeout") {
		t.Errorf("a sub-second max_timeout was accepted: %v", err)
	}
}
