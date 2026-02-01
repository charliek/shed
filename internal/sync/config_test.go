package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestLoadConfigFromPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sync.yaml")

	content := `
features:
  devproxy:
    description: "Sync mkcert certificates for devproxy"
    paths:
      - source: ~/.local/share/mkcert/rootCA.pem
        target: /usr/local/share/ca-certificates/mkcert-ca.crt
      - source: ~/.devproxy/certs
        target: /etc/ssl/devproxy
        include: "*.pem"
    postSync:
      - run: update-ca-certificates

  dotfiles:
    description: "Sync shell config and git settings"
    paths:
      - source: ~/.gitconfig
        target: /root/.gitconfig
      - source: ~/.bashrc
        target: /root/.bashrc

profiles:
  default:
    features: [devproxy]
  full:
    features: [devproxy, dotfiles]
`

	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFromPath(configPath)
	if err != nil {
		t.Fatalf("LoadConfigFromPath failed: %v", err)
	}

	// Verify features
	if len(cfg.Features) != 2 {
		t.Errorf("Expected 2 features, got %d", len(cfg.Features))
	}

	devproxy, ok := cfg.Features["devproxy"]
	if !ok {
		t.Fatal("devproxy feature not found")
	}
	if devproxy.Description != "Sync mkcert certificates for devproxy" {
		t.Errorf("devproxy description = %q, want %q", devproxy.Description, "Sync mkcert certificates for devproxy")
	}
	if len(devproxy.Paths) != 2 {
		t.Errorf("devproxy paths count = %d, want 2", len(devproxy.Paths))
	}
	if len(devproxy.PostSync) != 1 {
		t.Errorf("devproxy postSync count = %d, want 1", len(devproxy.PostSync))
	}
	if devproxy.PostSync[0].Run != "update-ca-certificates" {
		t.Errorf("devproxy postSync run = %q, want %q", devproxy.PostSync[0].Run, "update-ca-certificates")
	}

	// Verify profiles
	if len(cfg.Profiles) != 2 {
		t.Errorf("Expected 2 profiles, got %d", len(cfg.Profiles))
	}

	defaultProfile, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatal("default profile not found")
	}
	if len(defaultProfile.Features) != 1 || defaultProfile.Features[0] != "devproxy" {
		t.Errorf("default profile features = %v, want [devproxy]", defaultProfile.Features)
	}
}

func TestLoadConfigFromPath_NotFound(t *testing.T) {
	cfg, err := LoadConfigFromPath("/nonexistent/path/sync.yaml")
	if err != nil {
		t.Fatalf("Expected no error for missing file, got: %v", err)
	}

	if !cfg.IsEmpty() {
		t.Error("Expected empty config for missing file")
	}
}

func TestConfig_GetProfile(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"default": {Features: []string{"feature1"}},
			"full":    {Features: []string{"feature1", "feature2"}},
		},
	}

	// Test existing profile
	profile, err := cfg.GetProfile("default")
	if err != nil {
		t.Fatalf("GetProfile(default) failed: %v", err)
	}
	if len(profile.Features) != 1 {
		t.Errorf("default profile features = %d, want 1", len(profile.Features))
	}

	// Test non-existing profile
	_, err = cfg.GetProfile("nonexistent")
	if err == nil {
		t.Error("Expected error for non-existing profile")
	}
}

func TestConfig_GetFeature(t *testing.T) {
	cfg := &Config{
		Features: map[string]Feature{
			"feature1": {Description: "First feature", Paths: []PathMapping{{Source: "/a", Target: "/b"}}},
			"feature2": {Description: "Second feature", Paths: []PathMapping{{Source: "/c", Target: "/d"}}},
		},
	}

	// Test existing feature
	feature, err := cfg.GetFeature("feature1")
	if err != nil {
		t.Fatalf("GetFeature(feature1) failed: %v", err)
	}
	if feature.Description != "First feature" {
		t.Errorf("feature1 description = %q, want %q", feature.Description, "First feature")
	}

	// Test non-existing feature
	_, err = cfg.GetFeature("nonexistent")
	if err == nil {
		t.Error("Expected error for non-existing feature")
	}
}

func TestConfig_GetFeaturesForProfile(t *testing.T) {
	cfg := &Config{
		Features: map[string]Feature{
			"feature1": {Description: "First", Paths: []PathMapping{{Source: "/a", Target: "/b"}}},
			"feature2": {Description: "Second", Paths: []PathMapping{{Source: "/c", Target: "/d"}}},
		},
		Profiles: map[string]Profile{
			"full": {Features: []string{"feature1", "feature2"}},
		},
	}

	features, err := cfg.GetFeaturesForProfile("full")
	if err != nil {
		t.Fatalf("GetFeaturesForProfile failed: %v", err)
	}

	if len(features) != 2 {
		t.Errorf("Expected 2 features, got %d", len(features))
	}
	if _, ok := features["feature1"]; !ok {
		t.Error("feature1 not found in result")
	}
	if _, ok := features["feature2"]; !ok {
		t.Error("feature2 not found in result")
	}
}

func TestConfig_GetFeaturesForProfile_InvalidReference(t *testing.T) {
	cfg := &Config{
		Features: map[string]Feature{
			"feature1": {Description: "First", Paths: []PathMapping{{Source: "/a", Target: "/b"}}},
		},
		Profiles: map[string]Profile{
			"bad": {Features: []string{"feature1", "nonexistent"}},
		},
	}

	_, err := cfg.GetFeaturesForProfile("bad")
	if err == nil {
		t.Error("Expected error for invalid feature reference")
	}
}

func TestConfig_HasDefaultProfile(t *testing.T) {
	tests := []struct {
		name     string
		profiles map[string]Profile
		expected bool
	}{
		{
			name:     "has default",
			profiles: map[string]Profile{"default": {}},
			expected: true,
		},
		{
			name:     "no default",
			profiles: map[string]Profile{"other": {}},
			expected: false,
		},
		{
			name:     "empty",
			profiles: map[string]Profile{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Profiles: tt.profiles}
			if got := cfg.HasDefaultProfile(); got != tt.expected {
				t.Errorf("HasDefaultProfile() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestConfig_IsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *Config
		expected bool
	}{
		{
			name:     "completely empty",
			cfg:      &Config{Features: make(map[string]Feature), Profiles: make(map[string]Profile)},
			expected: true,
		},
		{
			name:     "has features",
			cfg:      &Config{Features: map[string]Feature{"f1": {}}, Profiles: make(map[string]Profile)},
			expected: false,
		},
		{
			name:     "has profiles",
			cfg:      &Config{Features: make(map[string]Feature), Profiles: map[string]Profile{"p1": {}}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEmpty(); got != tt.expected {
				t.Errorf("IsEmpty() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	// Valid config with paths
	validCfg := &Config{
		Features: map[string]Feature{
			"f1": {Paths: []PathMapping{{Source: "/a", Target: "/b"}}},
			"f2": {Paths: []PathMapping{{Source: "/c", Target: "/d"}}},
		},
		Profiles: map[string]Profile{
			"p1": {Features: []string{"f1", "f2"}},
		},
	}

	if err := validCfg.Validate(); err != nil {
		t.Errorf("Validate() failed for valid config: %v", err)
	}

	// Invalid config - references nonexistent feature
	invalidCfg := &Config{
		Features: map[string]Feature{
			"f1": {Paths: []PathMapping{{Source: "/a", Target: "/b"}}},
		},
		Profiles: map[string]Profile{
			"p1": {Features: []string{"f1", "nonexistent"}},
		},
	}

	if err := invalidCfg.Validate(); err == nil {
		t.Error("Validate() should fail for invalid feature reference")
	}

	// Invalid config - empty feature (no paths)
	emptyFeatureCfg := &Config{
		Features: map[string]Feature{
			"empty": {},
		},
		Profiles: map[string]Profile{},
	}

	if err := emptyFeatureCfg.Validate(); err == nil {
		t.Error("Validate() should fail for feature with no paths")
	}

	// Invalid config - empty source path
	emptySourceCfg := &Config{
		Features: map[string]Feature{
			"f1": {Paths: []PathMapping{{Source: "", Target: "/b"}}},
		},
		Profiles: map[string]Profile{},
	}

	if err := emptySourceCfg.Validate(); err == nil {
		t.Error("Validate() should fail for path with empty source")
	}

	// Invalid config - empty target path
	emptyTargetCfg := &Config{
		Features: map[string]Feature{
			"f1": {Paths: []PathMapping{{Source: "/a", Target: ""}}},
		},
		Profiles: map[string]Profile{},
	}

	if err := emptyTargetCfg.Validate(); err == nil {
		t.Error("Validate() should fail for path with empty target")
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("Could not get home directory")
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"~/test", filepath.Join(home, "test")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~", "~"}, // Single ~ is not expanded (no trailing slash)
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := config.ExpandPath(tt.input)
			if got != tt.expected {
				t.Errorf("ExpandPath(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestPathMapping_IncludePattern(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sync.yaml")

	content := `
features:
  certs:
    paths:
      - source: ~/certs
        target: /etc/ssl
        include: "*.pem"
profiles:
  default:
    features: [certs]
`

	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFromPath(configPath)
	if err != nil {
		t.Fatalf("LoadConfigFromPath failed: %v", err)
	}

	certs := cfg.Features["certs"]
	if certs.Paths[0].Include != "*.pem" {
		t.Errorf("Include pattern = %q, want %q", certs.Paths[0].Include, "*.pem")
	}
}
