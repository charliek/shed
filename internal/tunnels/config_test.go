package tunnels

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePortMapping(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    PortMapping
		wantErr bool
	}{
		{
			name:  "single port",
			input: "3000",
			want:  PortMapping{Local: 3000, Remote: 3000},
		},
		{
			name:  "local:remote",
			input: "4501:4096",
			want:  PortMapping{Local: 4501, Remote: 4096},
		},
		{
			name:  "with whitespace",
			input: "  8080  ",
			want:  PortMapping{Local: 8080, Remote: 8080},
		},
		{
			name:    "invalid format",
			input:   "abc",
			wantErr: true,
		},
		{
			name:    "port too high",
			input:   "70000",
			wantErr: true,
		},
		{
			name:    "port zero",
			input:   "0",
			wantErr: true,
		},
		{
			name:    "negative port",
			input:   "-1",
			wantErr: true,
		},
		{
			name:    "invalid local port",
			input:   "abc:3000",
			wantErr: true,
		},
		{
			name:    "invalid remote port",
			input:   "3000:abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePortMapping(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePortMapping() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParsePortMapping() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePortMappings(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []PortMapping
		wantErr bool
	}{
		{
			name:  "multiple ports",
			input: []string{"3000", "4501:4096", "8080"},
			want: []PortMapping{
				{Local: 3000, Remote: 3000},
				{Local: 4501, Remote: 4096},
				{Local: 8080, Remote: 8080},
			},
		},
		{
			name:  "empty list",
			input: []string{},
			want:  []PortMapping{},
		},
		{
			name:    "one invalid",
			input:   []string{"3000", "invalid"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePortMappings(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePortMappings() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParsePortMappings() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergePortMappings(t *testing.T) {
	tests := []struct {
		name  string
		input [][]PortMapping
		want  []PortMapping
	}{
		{
			name: "merge two slices",
			input: [][]PortMapping{
				{{Local: 3000, Remote: 3000}},
				{{Local: 4000, Remote: 4000}},
			},
			want: []PortMapping{
				{Local: 3000, Remote: 3000},
				{Local: 4000, Remote: 4000},
			},
		},
		{
			name: "override same local port",
			input: [][]PortMapping{
				{{Local: 3000, Remote: 3000}},
				{{Local: 3000, Remote: 4000}}, // Same local, different remote
			},
			want: []PortMapping{
				{Local: 3000, Remote: 4000}, // Later takes precedence
			},
		},
		{
			name: "preserve order",
			input: [][]PortMapping{
				{{Local: 8080, Remote: 8080}, {Local: 3000, Remote: 3000}},
				{{Local: 4000, Remote: 4000}},
			},
			want: []PortMapping{
				{Local: 8080, Remote: 8080},
				{Local: 3000, Remote: 3000},
				{Local: 4000, Remote: 4000},
			},
		},
		{
			name:  "empty slices",
			input: [][]PortMapping{},
			want:  []PortMapping{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergePortMappings(tt.input...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergePortMappings() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadTunnelConfig(t *testing.T) {
	// Create temp dir
	tmpDir := t.TempDir()

	t.Run("file not exists returns empty config", func(t *testing.T) {
		cfg, err := LoadTunnelConfigFromPath(filepath.Join(tmpDir, "nonexistent.yaml"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Sheds == nil {
			t.Error("Sheds map should be initialized")
		}
	})

	t.Run("valid config", func(t *testing.T) {
		configPath := filepath.Join(tmpDir, "tunnels.yaml")
		content := `
ssh:
  server_alive_interval: 60
  server_alive_count_max: 5
  connect_timeout: 15

sheds:
  myproj:
    profiles:
      default:
        - "4501:4096"
      dev:
        - "4501:4096"
        - "3000"
        - "8080"
`
		if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		cfg, err := LoadTunnelConfigFromPath(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.SSH.ServerAliveInterval != 60 {
			t.Errorf("ServerAliveInterval = %d, want 60", cfg.SSH.ServerAliveInterval)
		}
		if cfg.SSH.ServerAliveCountMax != 5 {
			t.Errorf("ServerAliveCountMax = %d, want 5", cfg.SSH.ServerAliveCountMax)
		}

		profiles := cfg.GetProfiles("myproj")
		if len(profiles) != 2 {
			t.Errorf("expected 2 profiles, got %d", len(profiles))
		}

		ports, err := cfg.GetPortMappings("myproj", "dev")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ports) != 3 {
			t.Errorf("expected 3 ports, got %d", len(ports))
		}
	})

	t.Run("invalid YAML", func(t *testing.T) {
		configPath := filepath.Join(tmpDir, "invalid.yaml")
		if err := os.WriteFile(configPath, []byte("invalid: yaml: content:"), 0600); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		_, err := LoadTunnelConfigFromPath(configPath)
		if err == nil {
			t.Error("expected error for invalid YAML")
		}
	})
}

func TestGetPortMappings(t *testing.T) {
	cfg := &TunnelConfig{
		Sheds: map[string]ShedConfig{
			"myproj": {
				Profiles: map[string][]string{
					"default": {"4501:4096"},
					"dev":     {"3000", "8080"},
				},
			},
		},
	}

	t.Run("valid profile", func(t *testing.T) {
		ports, err := cfg.GetPortMappings("myproj", "dev")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ports) != 2 {
			t.Errorf("expected 2 ports, got %d", len(ports))
		}
	})

	t.Run("unknown shed", func(t *testing.T) {
		_, err := cfg.GetPortMappings("unknown", "default")
		if err == nil {
			t.Error("expected error for unknown shed")
		}
	})

	t.Run("unknown profile", func(t *testing.T) {
		_, err := cfg.GetPortMappings("myproj", "unknown")
		if err == nil {
			t.Error("expected error for unknown profile")
		}
	})
}
