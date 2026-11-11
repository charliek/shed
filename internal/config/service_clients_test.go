package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validClient() ServiceClientConfig {
	return ServiceClientConfig{Policy: ServiceClientPolicy{
		NamePrefix: "nocturn-",
		MaxSheds:   4,
		MaxAge:     Duration(30 * time.Hour),
	}}
}

func mtlsWithClients(clients map[string]ServiceClientConfig) *ServerConfig {
	return &ServerConfig{
		Auth:   &AuthConfig{Mode: AuthModeMTLS, Clients: clients},
		Mounts: map[string]MountConfig{"claude": {Source: "/x", Target: "/home/shed/.claude"}},
	}
}

func TestValidateClients(t *testing.T) {
	mod := func(f func(*ServiceClientConfig)) ServiceClientConfig {
		c := validClient()
		f(&c)
		return c
	}
	tests := []struct {
		name    string
		cfg     *ServerConfig
		wantErr string
	}{
		{"valid", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": validClient()}), ""},
		{"valid with mounts and caps", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) {
			c.Policy.ServerMounts = []string{"claude"}
			c.Policy.Caps = ServiceClientCaps{CPUs: 4, MemoryMB: 8192}
			c.CertMaxTTL = Duration(time.Hour)
		})}), ""},
		{"token mode", &ServerConfig{Auth: &AuthConfig{Mode: AuthModeToken, Clients: map[string]ServiceClientConfig{"nocturn": validClient()}}}, "auth.clients requires auth.mode: mtls"},
		{"open mode", &ServerConfig{Auth: &AuthConfig{Clients: map[string]ServiceClientConfig{"nocturn": validClient()}}}, "auth.clients requires auth.mode: mtls"},
		{"bad name uppercase", mtlsWithClients(map[string]ServiceClientConfig{"Nocturn": validClient()}), "invalid auth.clients name"},
		{"bad name colon", mtlsWithClients(map[string]ServiceClientConfig{"a:b": validClient()}), "invalid auth.clients name"},
		{"missing prefix", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.Policy.NamePrefix = "" })}), "name_prefix is required"},
		{"bad prefix", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.Policy.NamePrefix = "Noc_" })}), "name_prefix"},
		{"missing max_sheds", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.Policy.MaxSheds = 0 })}), "max_sheds"},
		{"missing max_age", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.Policy.MaxAge = 0 })}), "max_age"},
		{"negative cert ttl", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.CertMaxTTL = Duration(-time.Hour) })}), "cert_max_ttl"},
		{"negative caps", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.Policy.Caps.CPUs = -1 })}), "caps"},
		{"unknown server mount", mtlsWithClients(map[string]ServiceClientConfig{"nocturn": mod(func(c *ServiceClientConfig) { c.Policy.ServerMounts = []string{"ssh"} })}), `server_mounts names "ssh"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateClients()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateClientImages(t *testing.T) {
	cfg := mtlsWithClients(map[string]ServiceClientConfig{"nocturn": validClient()})
	cfg.DefaultBackend = BackendFirecracker
	cfg.Firecracker = &FirecrackerConfig{ImageAliases: map[string]string{"itest": "ghcr.io/x/y:v1"}}
	c := cfg.Auth.Clients["nocturn"]
	c.Policy.Images = []string{"itest"}
	cfg.Auth.Clients["nocturn"] = c
	if err := cfg.validateClientImages(); err != nil {
		t.Fatalf("a known alias was refused: %v", err)
	}
	c.Policy.Images = []string{"ghcr.io/x/y:v1"}
	cfg.Auth.Clients["nocturn"] = c
	if err := cfg.validateClientImages(); err == nil || !strings.Contains(err.Error(), "not an image alias") {
		t.Fatalf("a ref that is not an alias was accepted: %v", err)
	}
}

func TestServiceClientAccessors(t *testing.T) {
	cfg := mtlsWithClients(map[string]ServiceClientConfig{"nocturn": validClient()})
	if !cfg.ServiceClientsEnabled() {
		t.Error("mtls with a client: not enabled")
	}
	cc, ok := cfg.ServiceClient("nocturn")
	if !ok || cc.Policy.NamePrefix != "nocturn-" {
		t.Errorf("ServiceClient(nocturn) = %+v, %v", cc, ok)
	}
	if cc.CertMaxTTLValue() != DefaultClientCertMaxTTL || DefaultClientCertMaxTTL != 2160*time.Hour {
		t.Errorf("default cert_max_ttl = %s, want 2160h", cc.CertMaxTTLValue())
	}
	if _, ok := cfg.ServiceClient("other"); ok {
		t.Error("unknown client found")
	}
	// The same clients in token mode are not usable (validation refuses the
	// config anyway; the accessor fails closed on its own).
	cfg.Auth.Mode = AuthModeToken
	if cfg.ServiceClientsEnabled() {
		t.Error("token mode: service clients enabled")
	}
	if _, ok := cfg.ServiceClient("nocturn"); ok {
		t.Error("token mode: client found")
	}
}

// TestServiceClientsFromYAML: the block loads, defaults apply, an unknown key
// anywhere under a client is refused, and clients outside mtls are refused.
func TestServiceClientsFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"
	auth := "auth:\n  mode: mtls\n  ssh:\n    github_users: [someone]\n  clients:\n    nocturn:\n"
	good := auth + "      policy:\n        name_prefix: nocturn-\n        max_sheds: 4\n        max_age: 30h\n"

	cfg, err := LoadServerConfigForCLI(write(base + good))
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := cfg.ServiceClient("nocturn")
	if !ok {
		t.Fatal("client not loaded")
	}
	if cc.Policy.MaxSheds != 4 || cc.Policy.MaxAge.Duration() != 30*time.Hour || cc.CertMaxTTLValue() != 2160*time.Hour {
		t.Errorf("loaded %+v", cc)
	}
	if len(cc.Policy.ServerMounts) != 0 || len(cc.Policy.Images) != 0 {
		t.Errorf("defaults: server_mounts=%v images=%v, want none", cc.Policy.ServerMounts, cc.Policy.Images)
	}

	for name, body := range map[string]string{
		"typo in policy":  auth + "      policy:\n        name_prefix: nocturn-\n        max_shed: 4\n        max_age: 30h\n",
		"typo at client":  auth + "      cert_max_tll: 1h\n      policy:\n        name_prefix: nocturn-\n        max_sheds: 4\n        max_age: 30h\n",
		"typo in caps":    auth + "      policy:\n        name_prefix: nocturn-\n        max_sheds: 4\n        max_age: 30h\n        caps: {cpu: 2}\n",
		"network not yet": auth + "      policy:\n        name_prefix: nocturn-\n        max_sheds: 4\n        max_age: 30h\n        network: isolated\n",
	} {
		if _, err := LoadServerConfigForCLI(write(base + body)); err == nil || !strings.Contains(err.Error(), "auth.clients") {
			t.Errorf("%s: err = %v, want an auth.clients refusal", name, err)
		}
	}

	tokenAuth := strings.Replace(good, "mode: mtls", "mode: token", 1)
	if _, err := LoadServerConfigForCLI(write(base + tokenAuth)); err == nil || !strings.Contains(err.Error(), "requires auth.mode: mtls") {
		t.Errorf("token mode with clients: %v", err)
	}
}

func TestMountSelectionAllows(t *testing.T) {
	var nilSel *MountSelection
	cases := []struct {
		name string
		sel  *MountSelection
		want map[string]bool
	}{
		{"nil is all (legacy)", nilSel, map[string]bool{"a": true, "b": true}},
		{"all", AllMounts(), map[string]bool{"a": true, "b": true}},
		{"selected none", SelectedMounts(nil), map[string]bool{"a": false, "b": false}},
		{"selected a", SelectedMounts([]string{"a"}), map[string]bool{"a": true, "b": false}},
		{"unknown mode fails closed", &MountSelection{Mode: "some"}, map[string]bool{"a": false}},
		{"empty mode fails closed", &MountSelection{}, map[string]bool{"a": false}},
	}
	for _, tt := range cases {
		for name, want := range tt.want {
			if got := tt.sel.Allows(name); got != want {
				t.Errorf("%s: Allows(%q) = %v, want %v", tt.name, name, got, want)
			}
		}
	}
	if got := EffectiveMountSelection(nil); got.Mode != MountModeAll {
		t.Errorf("EffectiveMountSelection(nil) = %+v, want all", got)
	}
}
