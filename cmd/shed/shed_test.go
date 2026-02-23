package main

import (
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestResolveCreateBackend(t *testing.T) {
	info := &config.ServerInfo{
		DefaultBackend:  config.BackendFirecracker,
		EnabledBackends: []string{config.BackendDocker, config.BackendFirecracker},
	}

	t.Run("default backend", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, "", 0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendFirecracker {
			t.Fatalf("backend = %q, want %q", backend, config.BackendFirecracker)
		}
		if warning != "" {
			t.Fatalf("unexpected warning: %s", warning)
		}
	})

	t.Run("requested backend enabled", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, config.BackendDocker, 0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendDocker {
			t.Fatalf("backend = %q, want %q", backend, config.BackendDocker)
		}
		if warning != "" {
			t.Fatalf("unexpected warning: %s", warning)
		}
	})

	t.Run("requested backend not enabled", func(t *testing.T) {
		info := &config.ServerInfo{
			DefaultBackend:  config.BackendDocker,
			EnabledBackends: []string{config.BackendDocker},
		}
		_, _, err := resolveCreateBackend(info, config.BackendFirecracker, 0, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("warn on docker resources", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, config.BackendDocker, 2, 2048)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendDocker {
			t.Fatalf("backend = %q, want %q", backend, config.BackendDocker)
		}
		if !strings.Contains(warning, "ignored") {
			t.Fatalf("expected warning about ignored flags, got %q", warning)
		}
	})

	t.Run("validate firecracker resources", func(t *testing.T) {
		_, _, err := resolveCreateBackend(info, config.BackendFirecracker, 0, 64)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing server info", func(t *testing.T) {
		_, _, err := resolveCreateBackend(&config.ServerInfo{}, "", 0, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
