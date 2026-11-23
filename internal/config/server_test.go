package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtensionsConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *ExtensionsConfig
		wantErr bool
	}{
		{
			name: "valid with multiple namespaces",
			config: &ExtensionsConfig{
				Enabled: []string{"my-extension", "other-ext"},
			},
			wantErr: false,
		},
		{
			name: "empty namespace rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"my-extension", ""},
			},
			wantErr: true,
		},
		{
			name: "duplicate namespace rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"my-extension", "my-extension"},
			},
			wantErr: true,
		},
		{
			name: "system prefix rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"system:health"},
			},
			wantErr: true,
		},
		{
			name: "spaces in namespace rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"my extension"},
			},
			wantErr: true,
		},
		{
			name: "non-ASCII characters rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"my-plügin"},
			},
			wantErr: true,
		},
		{
			name: "empty enabled list is valid",
			config: &ExtensionsConfig{
				Enabled: []string{},
			},
			wantErr: false,
		},
		{
			name: "nil enabled list is valid",
			config: &ExtensionsConfig{
				Enabled: nil,
			},
			wantErr: false,
		},
		{
			name:    "retired ssh-agent refused",
			config:  &ExtensionsConfig{Enabled: []string{"ssh-agent"}},
			wantErr: true,
		},
		{
			name:    "retired aws-credentials refused",
			config:  &ExtensionsConfig{Enabled: []string{"my-extension", "aws-credentials"}},
			wantErr: true,
		},
		{
			name:    "retired docker-credentials refused",
			config:  &ExtensionsConfig{Enabled: []string{"docker-credentials"}},
			wantErr: true,
		},
		{
			name: "valid with dots and colons",
			config: &ExtensionsConfig{
				Enabled: []string{"com.example.ext", "my:extension"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestGitConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *GitConfig
		wantErr bool
	}{
		{
			name:    "nil extras is valid",
			config:  &GitConfig{ExtraKnownHosts: nil},
			wantErr: false,
		},
		{
			name:    "empty extras is valid",
			config:  &GitConfig{ExtraKnownHosts: []string{}},
			wantErr: false,
		},
		{
			name: "valid ed25519 line",
			config: &GitConfig{ExtraKnownHosts: []string{
				"gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf",
			}},
			wantErr: false,
		},
		{
			name: "valid ecdsa line",
			config: &GitConfig{ExtraKnownHosts: []string{
				"example.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTY=",
			}},
			wantErr: false,
		},
		{
			name: "valid rsa line with trailing comment",
			config: &GitConfig{ExtraKnownHosts: []string{
				"my-gitea.internal ssh-rsa AAAAB3NzaC1yc2E= operator@laptop",
			}},
			wantErr: false,
		},
		{
			name:    "empty line rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{""}},
			wantErr: true,
		},
		{
			name:    "whitespace-only line rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{"   "}},
			wantErr: true,
		},
		{
			name:    "single field rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{"github.com"}},
			wantErr: true,
		},
		{
			name:    "two fields rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{"github.com ssh-ed25519"}},
			wantErr: true,
		},
		{
			name: "unknown key type rejected",
			config: &GitConfig{ExtraKnownHosts: []string{
				"github.com ssh-magic AAAAB3NzaC1y",
			}},
			wantErr: true,
		},
		{
			name: "garbage rejected",
			config: &GitConfig{ExtraKnownHosts: []string{
				"this is not a known_hosts line",
			}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseUpperSize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"5G is the default-ish", "5G", 5 * 1024 * 1024 * 1024, false},
		{"100G upper bound", "100G", 100 * 1024 * 1024 * 1024, false},
		{"1G lower bound", "1G", 1024 * 1024 * 1024, false},
		{"1024M = 1G accepted", "1024M", 1024 * 1024 * 1024, false},
		{"lowercase g", "5g", 5 * 1024 * 1024 * 1024, false},
		{"500M below 1G floor rejected", "500M", 0, true},
		{"101G above 100G ceiling rejected", "101G", 0, true},
		{"empty rejected", "", 0, true},
		{"non-numeric rejected", "fiveG", 0, true},
		{"negative rejected", "-1G", 0, true},
		{"overflow on G suffix rejected", "10000000000G", 0, true},
		{"raw int64-max rejected", "9223372036854775807", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUpperSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseUpperSize(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseUpperSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtensionsConfigRetiredMessage(t *testing.T) {
	for _, name := range []string{"ssh-agent", "aws-credentials", "docker-credentials"} {
		err := (&ExtensionsConfig{Enabled: []string{name}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "docs/upgrades/v0.10-to-v0.11.md") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error %v does not name the name and the upgrade guide", name, err)
		}
	}
}

// TestRetiredExtensionsRefusedByTheCLILoader: the CLI's loader refuses a
// retired helper too, and still takes any other extension name.
func TestRetiredExtensionsRefusedByTheCLILoader(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"
	for _, name := range []string{"ssh-agent", "aws-credentials", "docker-credentials"} {
		_, err := LoadServerConfigForCLI(write(base + "extensions:\n  enabled: [" + name + "]\n"))
		if err == nil || !strings.Contains(err.Error(), "docs/upgrades/v0.10-to-v0.11.md") {
			t.Errorf("%s: err = %v, want the upgrade guide named", name, err)
		}
	}
	if _, err := LoadServerConfigForCLI(write(base + "extensions:\n  enabled: [my-extension]\n")); err != nil {
		t.Errorf("an other extension name was refused: %v", err)
	}
}
