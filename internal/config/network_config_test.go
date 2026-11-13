package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNetworkConfigDefaults(t *testing.T) {
	var c ServerConfig
	if got := c.NetworkDefaultProfile(); got != NetworkProfileOpen {
		t.Errorf("default profile = %q, want open", got)
	}
	if got := c.NetworkAllowCIDRs(); len(got) != 0 {
		t.Errorf("allow_cidrs = %v, want none", got)
	}
	c.Network = &NetworkConfig{DefaultProfile: NetworkProfileIsolated, AllowCIDRs: []string{"192.168.77.5/24", " 10.1.0.0/16 "}}
	if got := c.NetworkDefaultProfile(); got != NetworkProfileIsolated {
		t.Errorf("default profile = %q, want isolated", got)
	}
	// Canonical network addresses, as nft wants them.
	if got, want := c.NetworkAllowCIDRs(), []string{"192.168.77.0/24", "10.1.0.0/16"}; !slices.Equal(got, want) {
		t.Errorf("allow_cidrs = %v, want %v", got, want)
	}
	if got := (ServiceClientPolicy{}).NetworkProfile(); got != NetworkProfileIsolated {
		t.Errorf("client policy default = %q, want isolated", got)
	}
	if got := (ServiceClientPolicy{Network: NetworkProfileOpen}).NetworkProfile(); got != NetworkProfileOpen {
		t.Errorf("client policy open = %q", got)
	}
	if got := EffectiveNetworkProfile(""); got != NetworkProfileOpen {
		t.Errorf("EffectiveNetworkProfile(\"\") = %q, want open (legacy sheds)", got)
	}
}

func TestValidateNetwork(t *testing.T) {
	clientWith := func(network string) *ServerConfig {
		c := validClient()
		c.Policy.Network = network
		return mtlsWithClients(map[string]ServiceClientConfig{"nocturn": c})
	}
	tests := []struct {
		name    string
		cfg     *ServerConfig
		wantErr string
	}{
		{"absent", &ServerConfig{}, ""},
		{"open", &ServerConfig{Network: &NetworkConfig{DefaultProfile: "open"}}, ""},
		{"isolated with cidrs", &ServerConfig{Network: &NetworkConfig{DefaultProfile: "isolated", AllowCIDRs: []string{"192.168.77.0/24"}}}, ""},
		{"unknown profile", &ServerConfig{Network: &NetworkConfig{DefaultProfile: "proxy-only"}}, "network.default_profile"},
		{"cidr without prefix", &ServerConfig{Network: &NetworkConfig{AllowCIDRs: []string{"192.168.77.1"}}}, "allow_cidrs"},
		{"ipv6 cidr", &ServerConfig{Network: &NetworkConfig{AllowCIDRs: []string{"fd00::/8"}}}, "allow_cidrs"},
		{"garbage cidr", &ServerConfig{Network: &NetworkConfig{AllowCIDRs: []string{"lan"}}}, "allow_cidrs"},
		{"client isolated", clientWith("isolated"), ""},
		{"client open", clientWith("open"), ""},
		{"client default", clientWith(""), ""},
		{"client unknown", clientWith("none"), "policy.network"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateNetwork()
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

func TestValidateNetworkBackend(t *testing.T) {
	clientWith := func(network string) map[string]ServiceClientConfig {
		c := validClient()
		c.Policy.Network = network
		return map[string]ServiceClientConfig{"nocturn": c}
	}
	staticFirewall := &RemoteConfig{Firewall: RemoteFirewallConfig{Mode: RemoteFirewallStatic}}
	anyFirewall := &RemoteConfig{Firewall: RemoteFirewallConfig{Mode: RemoteFirewallAny, AcknowledgeAny: true}}
	tests := []struct {
		name    string
		backend string
		network *NetworkConfig
		clients map[string]ServiceClientConfig
		remote  *RemoteConfig
		wantErr string
	}{
		{"fc isolated default", BackendFirecracker, &NetworkConfig{DefaultProfile: "isolated"}, clientWith(""), nil, ""},
		{"vz no network block", BackendVZ, nil, nil, nil, ""},
		{"vz open default", BackendVZ, &NetworkConfig{DefaultProfile: "open"}, nil, nil, ""},
		{"vz isolated default", BackendVZ, &NetworkConfig{DefaultProfile: "isolated"}, nil, nil, "vz backend"},
		{"vz client default is isolated", BackendVZ, nil, clientWith(""), nil, "set network: open"},
		{"vz client isolated", BackendVZ, nil, clientWith("isolated"), nil, "set network: open"},
		{"vz client open", BackendVZ, nil, clientWith("open"), nil, ""},
		// Remote accepts isolated: there it is isolation by topology.
		{"remote no network block", BackendRemote, nil, nil, staticFirewall, ""},
		{"remote isolated default", BackendRemote, &NetworkConfig{DefaultProfile: "isolated"}, nil, staticFirewall, ""},
		{"remote client default is isolated", BackendRemote, nil, clientWith(""), staticFirewall, ""},
		{"remote client isolated", BackendRemote, nil, clientWith("isolated"), staticFirewall, ""},
		{"remote client open", BackendRemote, nil, clientWith("open"), staticFirewall, ""},
		{"remote any firewall, open default", BackendRemote, nil, nil, anyFirewall, ""},
		{"remote any firewall, isolated default", BackendRemote, &NetworkConfig{DefaultProfile: "isolated"}, nil, anyFirewall, "remote.firewall.mode: any"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ServerConfig{DefaultBackend: tt.backend, Network: tt.network, Remote: tt.remote}
			if tt.clients != nil {
				c.Auth = &AuthConfig{Mode: AuthModeMTLS, Clients: tt.clients}
			}
			err := c.validateNetworkBackend()
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

func TestNetworkConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"

	cfg, err := LoadServerConfigForCLI(write(base + "network:\n  default_profile: isolated\n  allow_cidrs: [192.168.77.0/24]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkDefaultProfile() != NetworkProfileIsolated || !slices.Equal(cfg.NetworkAllowCIDRs(), []string{"192.168.77.0/24"}) {
		t.Errorf("loaded %+v", cfg.Network)
	}

	for name, body := range map[string]string{
		"typo key":        "network:\n  default_profle: isolated\n",
		"unknown profile": "network:\n  default_profile: none\n",
		"bad cidr":        "network:\n  allow_cidrs: [nope]\n",
	} {
		if _, err := LoadServerConfigForCLI(write(base + body)); err == nil || !strings.Contains(err.Error(), "network") {
			t.Errorf("%s: err = %v, want a network refusal", name, err)
		}
	}

	auth := "auth:\n  mode: mtls\n  ssh:\n    github_users: [someone]\n  clients:\n    nocturn:\n      policy:\n        name_prefix: nocturn-\n        max_sheds: 4\n        max_age: 30h\n"
	cfg, err = LoadServerConfigForCLI(write(base + auth + "        network: open\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cc, _ := cfg.ServiceClient("nocturn"); cc.Policy.NetworkProfile() != NetworkProfileOpen {
		t.Errorf("client network = %q, want open", cc.Policy.NetworkProfile())
	}
	cfg, err = LoadServerConfigForCLI(write(base + auth))
	if err != nil {
		t.Fatal(err)
	}
	if cc, _ := cfg.ServiceClient("nocturn"); cc.Policy.NetworkProfile() != NetworkProfileIsolated {
		t.Errorf("client network default = %q, want isolated", cc.Policy.NetworkProfile())
	}
}
