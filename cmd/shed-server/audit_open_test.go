package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/config"
)

// TestOpenAuditLog is when serve opens the audit log, and what a failure to
// open it does: on by default in mtls mode, where a failure warns and the
// server starts without it; required (auth.clients, or audit.enabled: true),
// where a failure stops the server; off in open mode and with
// audit.enabled: false.
func TestOpenAuditLog(t *testing.T) {
	yes, no := true, false
	mtls := &config.AuthConfig{Mode: config.AuthModeMTLS}
	clients := &config.AuthConfig{Mode: config.AuthModeMTLS, Clients: map[string]config.ServiceClientConfig{"c": {}}}
	tests := []struct {
		name     string
		cfg      config.ServerConfig
		on, must bool
	}{
		{"open mode", config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeOpen}}, false, false},
		{"no auth block", config.ServerConfig{}, false, false},
		{"open mode, enabled", config.ServerConfig{Audit: &config.AuditConfig{Enabled: &yes}}, true, true},
		{"mtls default", config.ServerConfig{Auth: mtls}, true, false},
		{"mtls, enabled", config.ServerConfig{Auth: mtls, Audit: &config.AuditConfig{Enabled: &yes}}, true, true},
		{"mtls, enabled false", config.ServerConfig{Auth: mtls, Audit: &config.AuditConfig{Enabled: &no}}, false, false},
		{"clients", config.ServerConfig{Auth: clients}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A directory it can open the log in.
			dir := t.TempDir()
			var logged []string
			logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
			l, err := openAuditLog(&tt.cfg, dir, logf)
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(dir, audit.FileName))
			if tt.on != (l != nil) || tt.on != (statErr == nil) {
				t.Fatalf("log open = %v (file: %v), want %v", l != nil, statErr, tt.on)
			}
			_ = l.Close()

			// A directory it cannot.
			logged = nil
			missing := filepath.Join(t.TempDir(), "gone", "deeper")
			l, err = openAuditLog(&tt.cfg, missing, logf)
			switch {
			case !tt.on:
				if l != nil || err != nil || len(logged) != 0 {
					t.Errorf("off: log %v, err %v, logged %q", l, err, logged)
				}
			case tt.must:
				if err == nil || l != nil || !strings.Contains(err.Error(), audit.FileName) {
					t.Errorf("required and unopenable: log %v, err %v; the server must not start", l, err)
				}
			default:
				if err != nil || l != nil {
					t.Errorf("on by default and unopenable: log %v, err %v; the server must start without it", l, err)
				}
				if len(logged) != 1 || !strings.HasPrefix(logged[0], "WARNING: audit:") || !strings.Contains(logged[0], missing) {
					t.Errorf("no warning naming the log: %q", logged)
				}
			}
		})
	}
}
