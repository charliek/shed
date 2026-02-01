package provision

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ShedYAML(t *testing.T) {
	// Create temp directory
	dir := t.TempDir()

	// Create .shed directory
	shedDir := filepath.Join(dir, ".shed")
	if err := os.MkdirAll(shedDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write provision.yaml
	content := `
hooks:
  install: scripts/install.sh
  startup: scripts/startup.sh
env:
  MY_VAR: my_value
  ANOTHER_VAR: "another value"
`
	if err := os.WriteFile(filepath.Join(shedDir, "provision.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Load config
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Verify hooks
	if cfg.Hooks.Install != "scripts/install.sh" {
		t.Errorf("Install hook = %q, want %q", cfg.Hooks.Install, "scripts/install.sh")
	}
	if cfg.Hooks.Startup != "scripts/startup.sh" {
		t.Errorf("Startup hook = %q, want %q", cfg.Hooks.Startup, "scripts/startup.sh")
	}

	// Verify env
	if cfg.Env["MY_VAR"] != "my_value" {
		t.Errorf("MY_VAR = %q, want %q", cfg.Env["MY_VAR"], "my_value")
	}
	if cfg.Env["ANOTHER_VAR"] != "another value" {
		t.Errorf("ANOTHER_VAR = %q, want %q", cfg.Env["ANOTHER_VAR"], "another value")
	}
}

func TestLoadConfig_NoConfig(t *testing.T) {
	// Create temp directory with no config files
	dir := t.TempDir()

	// Load config
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	// Should return empty config
	if cfg.HasAnyHooks() {
		t.Errorf("Expected no hooks, got install=%q startup=%q", cfg.Hooks.Install, cfg.Hooks.Startup)
	}
}

func TestConfig_HasHooks(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		hasInstall bool
		hasStartup bool
		hasAny     bool
	}{
		{
			name:       "empty config",
			config:     Config{},
			hasInstall: false,
			hasStartup: false,
			hasAny:     false,
		},
		{
			name: "install only",
			config: Config{
				Hooks: HooksConfig{Install: "install.sh"},
			},
			hasInstall: true,
			hasStartup: false,
			hasAny:     true,
		},
		{
			name: "startup only",
			config: Config{
				Hooks: HooksConfig{Startup: "startup.sh"},
			},
			hasInstall: false,
			hasStartup: true,
			hasAny:     true,
		},
		{
			name: "both hooks",
			config: Config{
				Hooks: HooksConfig{
					Install: "install.sh",
					Startup: "startup.sh",
				},
			},
			hasInstall: true,
			hasStartup: true,
			hasAny:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.HasInstallHook(); got != tt.hasInstall {
				t.Errorf("HasInstallHook() = %v, want %v", got, tt.hasInstall)
			}
			if got := tt.config.HasStartupHook(); got != tt.hasStartup {
				t.Errorf("HasStartupHook() = %v, want %v", got, tt.hasStartup)
			}
			if got := tt.config.HasAnyHooks(); got != tt.hasAny {
				t.Errorf("HasAnyHooks() = %v, want %v", got, tt.hasAny)
			}
		})
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	shedDir := filepath.Join(dir, ".shed")
	if err := os.MkdirAll(shedDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write invalid YAML
	content := `
hooks:
  install: [invalid yaml
`
	if err := os.WriteFile(filepath.Join(shedDir, "provision.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(dir)
	if err == nil {
		t.Error("Expected error for invalid YAML, got nil")
	}
}
