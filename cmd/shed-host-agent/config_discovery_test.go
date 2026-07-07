package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestServerSelectorUnmarshal(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantAll  bool
		wantName []string
	}{
		{"scalar all", "servers: all", true, nil},
		{"omitted", "watch: poll", false, nil},
		{"empty scalar", `servers: ""`, true, nil},
		{"empty list", "servers: []", false, []string{}},
		{"list", "servers: [mini2, mini3]", false, []string{"mini2", "mini3"}},
		{"single name scalar", "servers: mini2", false, []string{"mini2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dc DiscoveryConfig
			if err := yaml.Unmarshal([]byte(tt.yaml), &dc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if dc.Servers.All != tt.wantAll {
				t.Errorf("All = %v, want %v", dc.Servers.All, tt.wantAll)
			}
			if !reflect.DeepEqual(dc.Servers.Names, tt.wantName) {
				t.Errorf("Names = %v, want %v", dc.Servers.Names, tt.wantName)
			}
		})
	}
}

func TestServerSelectorSelected(t *testing.T) {
	all := ServerSelector{All: true}
	if !all.Selected("anything") {
		t.Error("All selector should select anything")
	}
	zero := ServerSelector{}
	if !zero.Selected("anything") {
		t.Error("zero selector should select anything")
	}
	list := ServerSelector{Names: []string{"mini2"}}
	if !list.Selected("mini2") || list.Selected("mini3") {
		t.Error("list selector should select only listed names")
	}
	emptyList := ServerSelector{Names: []string{}}
	if emptyList.Selected("anything") {
		t.Error("explicit empty list (servers: []) should select nothing")
	}
}

func TestLoadConfigDiscoveryDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
discovery:
  servers: all
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Discovery == nil {
		t.Fatal("Discovery should be non-nil when the block is present")
	}
	if !cfg.Discovery.Servers.All {
		t.Error("servers: all should set All")
	}
	if cfg.Discovery.Watch != "fsnotify" {
		t.Errorf("default watch = %q, want fsnotify", cfg.Discovery.Watch)
	}
	if cfg.Discovery.PollInterval != "10s" {
		t.Errorf("default poll_interval = %q, want 10s", cfg.Discovery.PollInterval)
	}
	if cfg.Discovery.Debounce != "500ms" {
		t.Errorf("default debounce = %q, want 500ms", cfg.Discovery.Debounce)
	}
	// Source defaults to the shed CLI config, with ~ expanded.
	if cfg.Discovery.Source == "" || cfg.Discovery.Source == DefaultDiscoverySource {
		t.Errorf("source should be expanded, got %q", cfg.Discovery.Source)
	}
}

func TestLoadConfigLegacyNoDiscovery(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server: http://localhost:9090\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Discovery != nil {
		t.Error("Discovery should be nil when the block is absent (legacy mode)")
	}
}

func TestResolveTargets(t *testing.T) {
	discovered := []ServerTarget{
		{Name: "mini2", URL: "http://mini2:8080"},
		{Name: "mini3", URL: "http://mini3:8080"},
	}

	t.Run("legacy single server", func(t *testing.T) {
		cfg := Config{Server: "http://localhost:8080"}
		got := cfg.ResolveTargets(discovered)
		want := []ServerTarget{{Name: "", URL: "http://localhost:8080"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("discover all", func(t *testing.T) {
		cfg := Config{Discovery: &DiscoveryConfig{Servers: ServerSelector{All: true}}}
		got := cfg.ResolveTargets(discovered)
		if !reflect.DeepEqual(got, discovered) {
			t.Errorf("got %v, want %v", got, discovered)
		}
	})

	t.Run("include subset", func(t *testing.T) {
		cfg := Config{Discovery: &DiscoveryConfig{Servers: ServerSelector{Names: []string{"mini3"}}}}
		got := cfg.ResolveTargets(discovered)
		want := []ServerTarget{{Name: "mini3", URL: "http://mini3:8080"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("dedup by name", func(t *testing.T) {
		dup := []ServerTarget{
			{Name: "mini2", URL: "http://mini2:8080"},
			{Name: "mini2", URL: "http://other:8080"},
		}
		cfg := Config{Discovery: &DiscoveryConfig{Servers: ServerSelector{All: true}}}
		got := cfg.ResolveTargets(dup)
		if len(got) != 1 || got[0].URL != "http://mini2:8080" {
			t.Errorf("dedup failed, got %v", got)
		}
	})
}

func TestDockerResolve(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cfg := DockerConfig{
		Registries: []string{"ghcr.io"},
		AllowAll:   false,
		Servers: map[string]DockerServerConfig{
			"mini2": {
				AllowAll: boolPtr(true),
				Sheds: map[string]DockerShedConfig{
					"web": {Registries: []string{"reg.example.com"}, AllowAll: boolPtr(false)},
				},
			},
		},
	}

	t.Run("defaults when no override", func(t *testing.T) {
		r := cfg.Resolve("mini3", "anything")
		if r.AllowAll || !reflect.DeepEqual(r.Registries, []string{"ghcr.io"}) {
			t.Errorf("got %+v, want defaults", r)
		}
	})

	t.Run("per-server allow_all override", func(t *testing.T) {
		r := cfg.Resolve("mini2", "api")
		if !r.AllowAll {
			t.Errorf("mini2 should inherit allow_all=true, got %+v", r)
		}
	})

	t.Run("per-shed overrides server (registries replace, allow_all false)", func(t *testing.T) {
		r := cfg.Resolve("mini2", "web")
		if r.AllowAll {
			t.Error("mini2/web should override allow_all back to false")
		}
		if !reflect.DeepEqual(r.Registries, []string{"reg.example.com"}) {
			t.Errorf("registries = %v, want [reg.example.com]", r.Registries)
		}
	})
}
