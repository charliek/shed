package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAuditLoggerWritesJSON(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")

	logger := NewAuditLogger(LogConfig{Enabled: true, Path: logPath}, slog.Default())
	defer logger.Close()

	logger.Log("mini2", "my-shed", "ssh-agent", "sign", "ok", "ssh-ed25519", "none")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	var entry AuditEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse log entry: %v", err)
	}

	if entry.Server != "mini2" {
		t.Errorf("server: got %q, want %q", entry.Server, "mini2")
	}
	if entry.Shed != "my-shed" {
		t.Errorf("shed: got %q, want %q", entry.Shed, "my-shed")
	}
	if entry.Namespace != "ssh-agent" {
		t.Errorf("namespace: got %q", entry.Namespace)
	}
	if entry.Operation != "sign" {
		t.Errorf("operation: got %q", entry.Operation)
	}
	if entry.Result != "ok" {
		t.Errorf("result: got %q", entry.Result)
	}
	if entry.Detail != "ssh-ed25519" {
		t.Errorf("detail: got %q", entry.Detail)
	}
	if entry.Timestamp == "" {
		t.Error("empty timestamp")
	}
}

func TestAuditLoggerDisabled(t *testing.T) {
	logger := NewAuditLogger(LogConfig{Enabled: false}, slog.Default())
	defer logger.Close()

	// Should not panic or error
	logger.Log("srv", "shed", "ns", "op", "ok", "", "none")
}

func TestAuditLoggerConcurrent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")

	logger := NewAuditLogger(LogConfig{Enabled: true, Path: logPath}, slog.Default())
	defer logger.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Log("srv", "shed", "ns", "op", "ok", "", "none")
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}

	// Count JSON lines
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 20 {
		t.Errorf("expected 20 log lines, got %d", lines)
	}
}

func TestAuditLoggerFilePermissions(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "subdir", "audit.log")

	logger := NewAuditLogger(LogConfig{Enabled: true, Path: logPath}, slog.Default())
	defer logger.Close()

	logger.Log("srv", "shed", "ns", "op", "ok", "", "none")

	// Check directory permissions
	dirInfo, err := os.Stat(filepath.Dir(logPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("dir permissions: got %o, want 0700", dirInfo.Mode().Perm())
	}

	// Check file permissions
	fileInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Errorf("file permissions: got %o, want 0600", fileInfo.Mode().Perm())
	}
}
