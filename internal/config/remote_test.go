package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// validRemote returns a remote block that passes Validate, as a config file
// naming only the environment and the default image would load.
func validRemote() *RemoteConfig {
	c := &RemoteConfig{Environment: "dev-test", DefaultImage: "family/shed-extensions-amd64"}
	c.applyDefaults()
	return c
}

func TestRemoteDefaults(t *testing.T) {
	c := validRemote()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
	if c.Provider != RemoteProviderGCE || c.Transport != RemoteTransportSSH {
		t.Errorf("drivers = %s/%s, want gce/ssh", c.Provider, c.Transport)
	}
	if c.DefaultSize != "standard" || len(c.Sizes) != 2 {
		t.Errorf("sizes = %q %v, want standard and two sizes", c.DefaultSize, c.Sizes)
	}
	if got := c.Sizes["large"]; got != (RemoteSizeConfig{MachineType: "e2-standard-8", CPUs: 8, MemoryMB: 32768, Arch: "amd64"}) {
		t.Errorf("large = %+v", got)
	}
	if c.DiskGB != 50 || c.Limits != (RemoteLimitsConfig{MaxRunning: 2, MaxSheds: 8}) {
		t.Errorf("disk %d limits %+v", c.DiskGB, c.Limits)
	}
	if c.MaxRunDuration.Duration() != 24*time.Hour || c.AgentInstall != RemoteAgentInstallIncompatible || c.Firewall.Mode != RemoteFirewallStatic {
		t.Errorf("run limit %s, agent_install %q, firewall %q", c.MaxRunDuration.Duration(), c.AgentInstall, c.Firewall.Mode)
	}
	if c.SSH != (RemoteSSHConfig{ClientKeyEnv: "SHED_REMOTE_SSH_KEY", ClientKeyFileEnv: "SHED_REMOTE_SSH_KEY_FILE"}) {
		t.Errorf("ssh = %+v", c.SSH)
	}
	want := RemoteGCEConfig{
		ProjectEnv: "SHED_GCE_PROJECT", ZoneEnv: "SHED_GCE_ZONE", NetworkEnv: "SHED_GCE_NETWORK", SubnetEnv: "SHED_GCE_SUBNET",
		CredentialsEnv: "GOOGLE_CREDENTIALS_BASE64", CredentialsFileEnv: "SHED_GCE_CREDENTIALS_FILE",
	}
	if c.GCE == nil || *c.GCE != want {
		t.Errorf("gce = %+v", c.GCE)
	}
	if got := c.StateDirPath("/var/lib/shed"); got != "/var/lib/shed/remote" {
		t.Errorf("StateDirPath = %q", got)
	}
	if got := c.AgentDirPath("/var/lib/shed"); got != "/var/lib/shed/agent" {
		t.Errorf("AgentDirPath = %q", got)
	}
	c.StateDir, c.AgentDir = "/s", "/a"
	if c.StateDirPath("/x") != "/s" || c.AgentDirPath("/x") != "/a" {
		t.Errorf("explicit dirs not honoured: %q %q", c.StateDirPath("/x"), c.AgentDirPath("/x"))
	}

	// The fake provider gets no gce block.
	f := &RemoteConfig{Provider: RemoteProviderFake}
	f.applyDefaults()
	if f.GCE != nil {
		t.Errorf("provider fake got a gce block: %+v", f.GCE)
	}
}

func TestRemoteValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *RemoteConfig)
		wantErr string
	}{
		{"defaults", func(c *RemoteConfig) {}, ""},
		{"unknown provider", func(c *RemoteConfig) { c.Provider = "aws" }, "invalid provider"},
		{"unknown transport", func(c *RemoteConfig) { c.Transport = "tailscale" }, "invalid transport"},
		{"fake without flag", func(c *RemoteConfig) { c.Provider = RemoteProviderFake }, "allow_test_drivers"},
		{"local without flag", func(c *RemoteConfig) { c.Transport = RemoteTransportLocal }, "allow_test_drivers"},
		{"fake and local with flag", func(c *RemoteConfig) {
			c.Provider, c.Transport, c.AllowTestDrivers, c.GCE = RemoteProviderFake, RemoteTransportLocal, true, nil
		}, ""},
		{"environment missing", func(c *RemoteConfig) { c.Environment = "" }, "environment is required"},
		{"environment uppercase", func(c *RemoteConfig) { c.Environment = "Dev" }, "invalid environment"},
		{"environment leading digit", func(c *RemoteConfig) { c.Environment = "1dev" }, "invalid environment"},
		{"environment trailing hyphen", func(c *RemoteConfig) { c.Environment = "dev-" }, "invalid environment"},
		{"environment 21 characters", func(c *RemoteConfig) { c.Environment = strings.Repeat("a", 21) }, "invalid environment"},
		{"environment 20 characters", func(c *RemoteConfig) { c.Environment = strings.Repeat("a", 20) }, ""},
		{"environment one letter", func(c *RemoteConfig) { c.Environment = "p" }, ""},
		{"relative state_dir", func(c *RemoteConfig) { c.StateDir = "state/remote" }, "state_dir must be an absolute path"},
		{"home state_dir", func(c *RemoteConfig) { c.StateDir = "~/.shed/dev-remote/remote" }, ""},
		{"relative agent_dir", func(c *RemoteConfig) { c.AgentDir = "agent" }, "agent_dir must be an absolute path"},
		{"default_image missing", func(c *RemoteConfig) { c.DefaultImage = "" }, "default_image is required"},
		{"default_image with a space", func(c *RemoteConfig) { c.DefaultImage = "family/shed base" }, "invalid default_image"},
		{"alias to empty name", func(c *RemoteConfig) { c.ImageAliases = map[string]string{"cloud": ""} }, "image_aliases.cloud"},
		{"alias", func(c *RemoteConfig) { c.ImageAliases = map[string]string{"cloud": "family/shed-extensions-amd64"} }, ""},
		{"default_size unknown", func(c *RemoteConfig) { c.DefaultSize = "huge" }, "default_size"},
		{"size without machine type", func(c *RemoteConfig) { c.Sizes["tiny"] = RemoteSizeConfig{CPUs: 1, MemoryMB: 1024, Arch: "amd64"} }, "sizes.tiny.machine_type"},
		{"size without cpus", func(c *RemoteConfig) {
			c.Sizes["tiny"] = RemoteSizeConfig{MachineType: "e2-micro", MemoryMB: 1024, Arch: "amd64"}
		}, "sizes.tiny.cpus"},
		{"size without memory", func(c *RemoteConfig) {
			c.Sizes["tiny"] = RemoteSizeConfig{MachineType: "e2-micro", CPUs: 1, Arch: "amd64"}
		}, "sizes.tiny.memory_mb"},
		{"size with unknown arch", func(c *RemoteConfig) {
			c.Sizes["tiny"] = RemoteSizeConfig{MachineType: "e2-micro", CPUs: 1, MemoryMB: 1024, Arch: "riscv"}
		}, "sizes.tiny.arch"},
		{"size name uppercase", func(c *RemoteConfig) {
			c.Sizes["Tiny"] = RemoteSizeConfig{MachineType: "e2-micro", CPUs: 1, MemoryMB: 1024, Arch: "amd64"}
		}, "invalid sizes name"},
		{"disk too small", func(c *RemoteConfig) { c.DiskGB = 5 }, "invalid disk_gb"},
		{"disk too large", func(c *RemoteConfig) { c.DiskGB = 501 }, "invalid disk_gb"},
		{"negative max_running", func(c *RemoteConfig) { c.Limits.MaxRunning = -1 }, "limits.max_running"},
		{"negative max_sheds", func(c *RemoteConfig) { c.Limits.MaxSheds = -1 }, "limits.max_sheds"},
		{"max_running above max_sheds", func(c *RemoteConfig) { c.Limits = RemoteLimitsConfig{MaxRunning: 9, MaxSheds: 8} }, "must not exceed"},
		{"run limit too short", func(c *RemoteConfig) { c.MaxRunDuration = Duration(time.Minute) }, "invalid max_run_duration"},
		{"run limit too long", func(c *RemoteConfig) { c.MaxRunDuration = Duration(121 * 24 * time.Hour) }, "invalid max_run_duration"},
		{"agent_install unknown", func(c *RemoteConfig) { c.AgentInstall = "sometimes" }, "invalid agent_install"},
		{"agent_install never", func(c *RemoteConfig) { c.AgentInstall = RemoteAgentInstallNever }, ""},
		{"ssh env not a name", func(c *RemoteConfig) { c.SSH.ClientKeyEnv = "MY KEY" }, "ssh.client_key_env"},
		{"ssh pair same name", func(c *RemoteConfig) { c.SSH.ClientKeyFileEnv = c.SSH.ClientKeyEnv }, "different variables"},
		{"gce env not a name", func(c *RemoteConfig) { c.GCE.ZoneEnv = "us-central1-a" }, "gce.zone_env"},
		{"gce credentials pair same name", func(c *RemoteConfig) { c.GCE.CredentialsFileEnv = c.GCE.CredentialsEnv }, "different variables"},
		{"gce block missing", func(c *RemoteConfig) { c.GCE = nil }, "gce block is required"},
		{"firewall unknown mode", func(c *RemoteConfig) { c.Firewall.Mode = "open" }, "invalid firewall.mode"},
		{"firewall static defaults to the env variable", func(c *RemoteConfig) {
			if c.Firewall.SourceRangesEnv != DefaultRemoteFirewallSourceRangesEnv {
				panic("no default source_ranges_env")
			}
		}, ""},
		{"firewall static with ranges", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"203.0.113.7/32", "198.51.100.0/24"}}
		}, ""},
		{"firewall static with a named variable", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "MY_RANGES"}
		}, ""},
		{"firewall static with neither", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic}
		}, "needs firewall.source_ranges or firewall.source_ranges_env"},
		{"firewall static with both", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"203.0.113.7/32"}, SourceRangesEnv: "MY_RANGES"}
		}, "set exactly one"},
		{"firewall static variable not a name", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "203.0.113.7/32"}
		}, "not an environment variable name"},
		{"firewall bad range", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"203.0.113.7"}}
		}, "firewall.source_ranges"},
		{"firewall ipv6 range", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"2001:db8::/32"}}
		}, "firewall.source_ranges"},
		{"firewall static any address", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"0.0.0.0/0"}}
		}, "wider than a /24"},
		{"firewall static wide range", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"203.0.113.7/32", "198.51.0.0/16"}}
		}, "wider than a /24"},
		{"firewall static wide range acknowledged", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"198.51.0.0/16"}, AcknowledgeAny: true}
		}, ""},
		{"firewall static a /24", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"198.51.100.0/24"}}
		}, ""},
		{"firewall static with echo urls", func(c *RemoteConfig) { c.Firewall.EchoURLs = []string{"https://a.example", "https://b.example"} }, "echo_urls is only used"},
		{"firewall auto without urls", func(c *RemoteConfig) { c.Firewall.Mode = RemoteFirewallAuto }, "at least two"},
		{"firewall auto with one url", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example"}}
		}, "at least two"},
		{"firewall auto with http url", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "http://b.example"}}
		}, "must be an https URL"},
		{"firewall auto", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example"}}
		}, ""},
		{"firewall auto with one host twice", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example/ip", "https://a.example/ip"}}
		}, "names host a.example more than once"},
		{"firewall auto with two paths on one host", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example/ip", "https://A.Example.:8443/other"}}
		}, "names host a.example more than once"},
		{"firewall auto with a third entry on a used host", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example", "https://user@b.example/x"}}
		}, "names host b.example more than once"},
		{"firewall auto with ranges", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example"}, SourceRanges: []string{"203.0.113.7/32"}}
		}, "source_ranges is only used"},
		{"firewall auto with a ranges variable", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example"}, SourceRangesEnv: "MY_RANGES"}
		}, "source_ranges_env is only used"},
		{"firewall auto with acknowledge_any", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example"}, AcknowledgeAny: true}
		}, "acknowledge_any is not used"},
		{"firewall any unacknowledged", func(c *RemoteConfig) { c.Firewall.Mode = RemoteFirewallAny }, "acknowledge_any: true"},
		{"firewall any with ranges", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAny, AcknowledgeAny: true, SourceRanges: []string{"0.0.0.0/0"}}
		}, "source_ranges is only used"},
		{"firewall any with a ranges variable", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAny, AcknowledgeAny: true, SourceRangesEnv: "MY_RANGES"}
		}, "source_ranges_env is only used"},
		{"firewall any acknowledged", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAny, AcknowledgeAny: true}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validRemote()
			tt.mutate(c)
			checkErr(t, c.Validate(), tt.wantErr)
		})
	}
}

func checkErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error %v, want one containing %q", err, want)
	}
}

func TestRemoteResolveSize(t *testing.T) {
	c := validRemote()
	c.Sizes["medium"] = RemoteSizeConfig{MachineType: "e2-standard-4", CPUs: 4, MemoryMB: 16384, Arch: "amd64"}
	tests := []struct {
		name     string
		cpus     int
		memoryMB int
		want     string
		wantErr  string
	}{
		{"default", 0, 0, "standard", ""},
		{"exact standard", 2, 8192, "standard", ""},
		{"exact large", 8, 32768, "large", ""},
		{"cpus only", 1, 0, "standard", ""},
		{"memory only", 0, 4096, "standard", ""},
		{"cpus round up", 3, 0, "medium", ""},
		{"memory rounds up past cpus", 2, 8193, "medium", ""},
		{"memory needs large", 2, 20000, "large", ""},
		{"too many cpus", 9, 0, "", "no size provides 9 CPUs"},
		{"too much memory", 2, 65536, "", "sizes: large=8 CPUs/32768 MB"},
		{"negative cpus", -1, 0, "", "must not be negative"},
		{"negative memory", 0, -1, "", "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ResolveSize(tt.cpus, tt.memoryMB)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tt.wantErr)
				}
				if !errors.Is(err, ErrInvalidShedRequestSentinel) {
					t.Errorf("error %v does not wrap ErrInvalidShedRequestSentinel", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.want {
				t.Errorf("size = %s, want %s", got.Name, tt.want)
			}
			if s := c.Sizes[tt.want]; got.CPUs != s.CPUs || got.MemoryMB != s.MemoryMB || got.MachineType != s.MachineType || got.Arch != s.Arch {
				t.Errorf("size = %+v, want config entry %+v", got, s)
			}
		})
	}
	if _, err := c.ResolveSize(64, 0); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("error %v does not keep the provider's ErrInvalid", err)
	}
}

func TestRemoteResolveDiskGB(t *testing.T) {
	const gb = int64(1) << 30
	c := validRemote()
	tests := []struct {
		name    string
		bytes   int64
		want    int
		wantErr bool
	}{
		{"zero is disk_gb", 0, 50, false},
		{"exact", 100 * gb, 100, false},
		{"rounds up", 20*gb + 1, 21, false},
		{"one byte short of whole", 30*gb - 1, 30, false},
		{"minimum", 10 * gb, 10, false},
		{"maximum", 500 * gb, 500, false},
		{"below minimum", 9 * gb, 0, true},
		{"above maximum", 500*gb + 1, 0, true},
		{"negative", -1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ResolveDiskGB(tt.bytes)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidShedRequestSentinel) {
					t.Fatalf("error %v, want ErrInvalidShedRequestSentinel", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("disk = %d GB, want %d", got, tt.want)
			}
		})
	}
}

func TestRemoteResolveImage(t *testing.T) {
	c := validRemote()
	c.ImageAliases = map[string]string{"cloud": "family/shed-extensions-amd64", "pinned": "shed-extensions-amd64-v1"}
	tests := []struct {
		name, image, want string
		wantErr           bool
	}{
		{"default", "", "family/shed-extensions-amd64", false},
		{"alias", "cloud", "family/shed-extensions-amd64", false},
		{"alias to exact name", "pinned", "shed-extensions-amd64-v1", false},
		{"raw image name", "shed-extensions-amd64-v1", "", true},
		{"unknown", "full", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ResolveImage(tt.image)
			if tt.wantErr {
				if !errors.Is(err, ErrUnknownImageSentinel) {
					t.Fatalf("error %v, want ErrUnknownImageSentinel", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("image = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestRemoteRunLimit(t *testing.T) {
	c := validRemote()
	tests := []struct {
		name         string
		clientMaxAge time.Duration
		want         time.Duration
	}{
		{"admin shed", 0, 24 * time.Hour},
		{"client max_age shorter", 6 * time.Hour, 24 * time.Hour},
		{"client max_age longer", 30 * time.Hour, 30 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.RunLimit(tt.clientMaxAge); got != tt.want {
				t.Errorf("RunLimit = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRemoteResolve(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "link_ed25519")
	credsFile := filepath.Join(dir, "sa.json")
	emptyFile := filepath.Join(dir, "empty")
	for path, body := range map[string]string{keyFile: "KEY-FROM-FILE\n", credsFile: `{"type":"service_account"}`, emptyFile: "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	full := func() map[string]string {
		return map[string]string{
			"SHED_GCE_PROJECT": "proj", "SHED_GCE_ZONE": "zone-a", "SHED_GCE_NETWORK": "net", "SHED_GCE_SUBNET": "subnet",
			"SHED_GCE_CREDENTIALS_FILE": credsFile,
			"SHED_REMOTE_SSH_KEY_FILE":  keyFile,
			"SHED_REMOTE_SOURCE_RANGES": "203.0.113.7/32",
		}
	}
	tests := []struct {
		name      string
		mutateCfg func(c *RemoteConfig)
		mutateEnv func(env map[string]string)
		wantErr   string
		wantKey   string
		wantCreds string
		// wantSources is compared when set; "-" means none.
		wantSources string
	}{
		{name: "source ranges from the variable, canonical and sorted", mutateEnv: func(env map[string]string) {
			env["SHED_REMOTE_SOURCE_RANGES"] = " 203.0.113.7/32, 198.51.100.9/24 ,203.0.113.7/32,"
		}, wantKey: "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`, wantSources: "198.51.100.0/24,203.0.113.7/32"},
		{name: "source ranges from the file", mutateCfg: func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"192.0.2.44/32"}}
		}, mutateEnv: func(env map[string]string) {
			env["SHED_REMOTE_SOURCE_RANGES"] = "not read"
		}, wantKey: "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`, wantSources: "192.0.2.44/32"},
		{name: "source ranges from a named variable", mutateCfg: func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "MY_RANGES"}
		}, mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SOURCE_RANGES")
			env["MY_RANGES"] = "192.0.2.45/32"
		}, wantKey: "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`, wantSources: "192.0.2.45/32"},
		{name: "source ranges variable unset", mutateEnv: func(env map[string]string) { delete(env, "SHED_REMOTE_SOURCE_RANGES") },
			wantErr: "SHED_REMOTE_SOURCE_RANGES (remote.firewall.source_ranges_env) is not set"},
		{name: "source ranges variable only commas", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = " , ," },
			wantErr: "lists no source range"},
		{name: "source ranges variable not a cidr", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "203.0.113.7" },
			wantErr: "must be an IPv4 CIDR"},
		{name: "source ranges variable ipv6", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "2001:db8::1/128" },
			wantErr: "must be an IPv4 CIDR"},
		{name: "source ranges variable any address", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "0.0.0.0/0" },
			wantErr: "wider than a /24"},
		{name: "source ranges variable wide", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "203.0.113.7/32,198.51.0.0/23" },
			wantErr: "wider than a /24"},
		{name: "source ranges variable wide, acknowledged", mutateCfg: func(c *RemoteConfig) { c.Firewall.AcknowledgeAny = true },
			mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "0.0.0.0/0" },
			wantKey:   "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`, wantSources: "0.0.0.0/0"},
		{name: "auto mode reads no ranges", mutateCfg: func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example"}}
		}, mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "garbage" },
			wantKey: "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`, wantSources: "-"},
		{name: "fake provider with a bad list is still refused", mutateCfg: func(c *RemoteConfig) {
			c.Provider, c.AllowTestDrivers, c.GCE = RemoteProviderFake, true, nil
		}, mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "garbage" }, wantErr: "must be an IPv4 CIDR"},
		{name: "file variables", wantKey: "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`},
		{name: "base64 variables", mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
			delete(env, "SHED_GCE_CREDENTIALS_FILE")
			env["SHED_REMOTE_SSH_KEY"] = b64("KEY-INLINE")
			env["GOOGLE_CREDENTIALS_BASE64"] = b64(`{"inline":true}`)
		}, wantKey: "KEY-INLINE", wantCreds: `{"inline":true}`},
		{name: "wrapped and unpadded base64", mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
			env["SHED_REMOTE_SSH_KEY"] = "S0VZLUlO\nTElORQ\n"
		}, wantKey: "KEY-INLINE", wantCreds: `{"type":"service_account"}`},
		{name: "custom variable names", mutateCfg: func(c *RemoteConfig) {
			c.GCE.ProjectEnv = "MY_PROJECT"
			c.SSH.ClientKeyFileEnv = "MY_KEY_FILE"
		}, mutateEnv: func(env map[string]string) {
			env["MY_PROJECT"] = env["SHED_GCE_PROJECT"]
			env["MY_KEY_FILE"] = env["SHED_REMOTE_SSH_KEY_FILE"]
			delete(env, "SHED_GCE_PROJECT")
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
		}, wantKey: "KEY-FROM-FILE\n", wantCreds: `{"type":"service_account"}`},
		{name: "project unset", mutateEnv: func(env map[string]string) { delete(env, "SHED_GCE_PROJECT") }, wantErr: "SHED_GCE_PROJECT (remote.gce.project_env) is not set"},
		{name: "zone blank", mutateEnv: func(env map[string]string) { env["SHED_GCE_ZONE"] = "  " }, wantErr: "SHED_GCE_ZONE"},
		{name: "network unset", mutateEnv: func(env map[string]string) { delete(env, "SHED_GCE_NETWORK") }, wantErr: "SHED_GCE_NETWORK"},
		{name: "subnet unset", mutateEnv: func(env map[string]string) { delete(env, "SHED_GCE_SUBNET") }, wantErr: "SHED_GCE_SUBNET"},
		{name: "no key variable", mutateEnv: func(env map[string]string) { delete(env, "SHED_REMOTE_SSH_KEY_FILE") }, wantErr: "neither environment variable SHED_REMOTE_SSH_KEY"},
		{name: "both key variables", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SSH_KEY"] = b64("x") }, wantErr: "are both set"},
		{name: "no credentials variable", mutateEnv: func(env map[string]string) { delete(env, "SHED_GCE_CREDENTIALS_FILE") }, wantErr: "neither environment variable GOOGLE_CREDENTIALS_BASE64"},
		{name: "both credentials variables", mutateEnv: func(env map[string]string) { env["GOOGLE_CREDENTIALS_BASE64"] = b64("{}") }, wantErr: "are both set"},
		{name: "key not base64", mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
			env["SHED_REMOTE_SSH_KEY"] = "not base64!"
		}, wantErr: "SHED_REMOTE_SSH_KEY (remote.ssh.client_key_env) is not valid base64"},
		{name: "key file missing", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SSH_KEY_FILE"] = filepath.Join(dir, "absent") }, wantErr: "SHED_REMOTE_SSH_KEY_FILE"},
		{name: "credentials file empty", mutateEnv: func(env map[string]string) { env["SHED_GCE_CREDENTIALS_FILE"] = emptyFile }, wantErr: "is empty"},
		{name: "fake provider needs no gce variables", mutateCfg: func(c *RemoteConfig) {
			c.Provider, c.AllowTestDrivers, c.GCE = RemoteProviderFake, true, nil
		}, mutateEnv: func(env map[string]string) {
			for k := range env {
				if k != "SHED_REMOTE_SSH_KEY_FILE" {
					delete(env, k)
				}
			}
		}, wantKey: "KEY-FROM-FILE\n"},
		{name: "test drivers need nothing", mutateCfg: func(c *RemoteConfig) {
			c.Provider, c.Transport, c.AllowTestDrivers, c.GCE = RemoteProviderFake, RemoteTransportLocal, true, nil
		}, mutateEnv: func(env map[string]string) { clear(env) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validRemote()
			if tt.mutateCfg != nil {
				tt.mutateCfg(c)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("test config is invalid: %v", err)
			}
			env := full()
			if tt.mutateEnv != nil {
				tt.mutateEnv(env)
			}
			rt, err := c.Resolve(func(k string) string { return env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(rt.SSHClientKey) != tt.wantKey || string(rt.Credentials) != tt.wantCreds {
				t.Errorf("key %q creds %q, want %q %q", rt.SSHClientKey, rt.Credentials, tt.wantKey, tt.wantCreds)
			}
			if tt.wantSources != "" {
				want := tt.wantSources
				if want == "-" {
					want = ""
				}
				if got := strings.Join(rt.FirewallSources, ","); got != want {
					t.Errorf("FirewallSources = %q, want %q", got, want)
				}
			}
			if c.Provider == RemoteProviderGCE && (rt.Project != "proj" || rt.Zone != "zone-a" || rt.Network != "net" || rt.Subnet != "subnet") {
				t.Errorf("gce values = %+v", rt)
			}
		})
	}
}

// TestRemoteResolveErrorsHideValues checks that a start-up error names the
// variable and never echoes what it held.
func TestRemoteResolveErrorsHideValues(t *testing.T) {
	c := validRemote()
	env := map[string]string{
		"SHED_GCE_PROJECT": "proj", "SHED_GCE_ZONE": "zone-a", "SHED_GCE_NETWORK": "net", "SHED_GCE_SUBNET": "subnet",
		"GOOGLE_CREDENTIALS_BASE64": "c2VjcmV0LWNyZWRz", "SHED_REMOTE_SSH_KEY": "secret-key-material!",
	}
	_, err := c.Resolve(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("bad base64 accepted")
	}
	if strings.Contains(err.Error(), "secret-key-material") {
		t.Errorf("error echoes the secret: %v", err)
	}
}

// remoteYAML is contracts section 6's block, with the home-relative paths
// kept as written.
const remoteYAML = `name: t
default_backend: remote
remote:
  provider: gce
  transport: ssh
  allow_test_drivers: false
  environment: dev-one
  state_dir: ~/.shed/dev-remote/remote
  default_image: family/shed-extensions-amd64
  image_aliases: {cloud: family/shed-extensions-amd64}
  default_size: standard
  sizes:
    standard: {machine_type: e2-standard-2, cpus: 2, memory_mb: 8192, arch: amd64}
    large:    {machine_type: e2-standard-8, cpus: 8, memory_mb: 32768, arch: amd64}
  disk_gb: 50
  limits: {max_running: 2, max_sheds: 8}
  max_run_duration: 24h
  agent_dir: ~/.shed/dev-remote/agent
  agent_install: incompatible
  ssh:
    client_key_env: SHED_REMOTE_SSH_KEY
    client_key_file_env: SHED_REMOTE_SSH_KEY_FILE
  firewall:
    mode: static
    source_ranges: []
    acknowledge_any: false
    echo_urls: []
  gce:
    project_env: SHED_GCE_PROJECT
    zone_env: SHED_GCE_ZONE
    network_env: SHED_GCE_NETWORK
    subnet_env: SHED_GCE_SUBNET
    credentials_env: GOOGLE_CREDENTIALS_BASE64
    credentials_file_env: SHED_GCE_CREDENTIALS_FILE
`

const remoteClientYAML = `auth:
  mode: mtls
  ssh:
    github_users: [someone]
  clients:
    nocturn:
      policy:
        name_prefix: nocturn-
        max_sheds: 4
        max_age: 30h
        images: [cloud]
        network: isolated
`

func TestRemoteConfigFromYAML(t *testing.T) {
	// None of the variables the block names is set: a config must load
	// without them. Resolve, not the loader, reads the environment.
	for _, k := range []string{"SHED_GCE_PROJECT", "SHED_GCE_ZONE", "SHED_REMOTE_SSH_KEY", "SHED_REMOTE_SSH_KEY_FILE", "GOOGLE_CREDENTIALS_BASE64", "SHED_GCE_CREDENTIALS_FILE"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	minimal := "name: t\ndefault_backend: remote\nremote:\n  environment: dev-a\n  default_image: family/shed-extensions-amd64\n"
	minimalAliased := minimal + "  image_aliases: {cloud: family/shed-extensions-amd64}\n"
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"contract block", remoteYAML, ""},
		{"minimal block", minimal, ""},
		{"client policy naming alias cloud and network isolated", remoteYAML + "https_port: 18490\n" + remoteClientYAML, ""},
		{"isolated default profile", minimal + "network:\n  default_profile: isolated\n", ""},
		{"no remote block", "name: t\ndefault_backend: remote\n", "remote configuration is required"},
		{"firecracker block beside it", minimal + "firecracker: {}\n", "a firecracker: block is not allowed"},
		{"vz block beside it", minimal + "vz: {}\n", "a vz: block is not allowed"},
		{"remote block under firecracker", "name: t\ndefault_backend: firecracker\nfirecracker: {}\nremote:\n  environment: dev-a\n  default_image: x\n", "a remote: block needs default_backend: remote"},
		{"remote block under detect", "name: t\nremote:\n  environment: dev-a\n  default_image: x\n", "detect never picks remote"},
		{"fake provider without the flag", minimal + "  provider: fake\n", "allow_test_drivers"},
		{"local transport without the flag", minimal + "  transport: local\n", "allow_test_drivers"},
		{"test drivers with the flag", minimal + "  provider: fake\n  transport: local\n  allow_test_drivers: true\n", ""},
		{"bad environment", strings.Replace(minimal, "dev-a", "Dev_A", 1), "remote config: invalid environment"},
		{"env_file", minimal + "env_file: /dev/null\n", "env_file is not supported"},
		{"mounts", minimal + "mounts:\n  claude: {source: /tmp, target: /home/shed/.claude}\n", "mounts are not supported"},
		{"egress enabled", minimal + "egress:\n  enabled: true\n", "egress.enabled is not supported"},
		{"allow_cidrs", minimal + "network:\n  allow_cidrs: [192.168.1.0/24]\n", "network.allow_cidrs is not supported"},
		{"extensions", minimal + "extensions:\n  enabled: [ssh-agent]\n", "extensions.enabled is not supported"},
		{"client image that is not an alias", minimal + "https_port: 18490\n" + remoteClientYAML, "not an image alias of the remote backend"},
		{"client server_mounts", minimalAliased + "https_port: 18490\n" + remoteClientYAML + "        server_mounts: [claude]\n", "server_mounts"},
		{"firewall any beside a client", minimalAliased + "  firewall: {mode: any, acknowledge_any: true}\nhttps_port: 18490\n" + remoteClientYAML, "not allowed with auth.clients"},
		{"firewall static acknowledged beside a client", minimalAliased + "  firewall: {mode: static, source_ranges: [198.51.0.0/16], acknowledge_any: true}\nhttps_port: 18490\n" + remoteClientYAML, "acknowledge_any is not allowed with auth.clients"},
		{"firewall static acknowledged with isolated default", minimal + "  firewall: {mode: static, source_ranges: [198.51.0.0/16], acknowledge_any: true}\nnetwork:\n  default_profile: isolated\n", "not allowed with remote.firewall.acknowledge_any"},
		{"firewall static acknowledged alone", minimal + "  firewall: {mode: static, source_ranges: [198.51.0.0/16], acknowledge_any: true}\n", ""},
		{"firewall static with a ranges variable beside a client", minimalAliased + "  firewall: {mode: static, source_ranges_env: MY_RANGES}\nhttps_port: 18490\n" + remoteClientYAML, ""},
		{"firewall any with isolated default", minimal + "  firewall: {mode: any, acknowledge_any: true}\nnetwork:\n  default_profile: isolated\n", "not allowed with remote.firewall.mode: any"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(tt.body)
			cfg, err := LoadServerConfigFromPath(path)
			checkErr(t, err, tt.wantErr)
			// The image-only loader applies the same remote rules.
			_, cliErr := LoadServerConfigForCLI(path)
			checkErr(t, cliErr, tt.wantErr)
			if tt.wantErr != "" {
				return
			}
			if cfg.DefaultBackend != BackendRemote || cfg.Remote == nil {
				t.Fatalf("backend %q remote %v", cfg.DefaultBackend, cfg.Remote)
			}
		})
	}

	cfg, err := LoadServerConfigFromPath(write(remoteYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ActiveDefaultImage(); got != "family/shed-extensions-amd64" {
		t.Errorf("ActiveDefaultImage = %q", got)
	}
	if cpus, mem := cfg.ActiveDefaultResources(); cpus != 2 || mem != 8192 {
		t.Errorf("ActiveDefaultResources = %d, %d; want 2, 8192", cpus, mem)
	}
	home, _ := os.UserHomeDir()
	if got, want := cfg.Remote.StateDirPath("/unused"), filepath.Join(home, ".shed/dev-remote/remote"); got != want {
		t.Errorf("StateDirPath = %q, want %q", got, want)
	}
	if _, err := cfg.Remote.Resolve(nil); err == nil || !strings.Contains(err.Error(), "SHED_GCE_PROJECT") {
		t.Errorf("Resolve with an empty environment: %v, want an error naming SHED_GCE_PROJECT", err)
	}
}

func TestResolveBackendNeverPicksRemote(t *testing.T) {
	for _, platform := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}} {
		t.Run(platform[0]+"/"+platform[1], func(t *testing.T) {
			if got, _ := ResolveBackend(BackendDetect, platform[0], platform[1]); got == BackendRemote {
				t.Errorf("detect picked remote on %s/%s", platform[0], platform[1])
			}
		})
	}
	if got, err := ResolveBackend(BackendRemote, "linux", "amd64"); err != nil || got != BackendRemote {
		t.Errorf("explicit remote = %q, %v", got, err)
	}
	if !isValidBackend(BackendRemote) {
		t.Error("remote is not a valid default_backend")
	}
}

// TestRemoteFirewallEchoURLErrorHidesCredentials: a refused echo URL is quoted
// in a start-up error that reaches the log, without its userinfo.
func TestRemoteFirewallEchoURLErrorHidesCredentials(t *testing.T) {
	const secret = "s3cr3t-token"
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"user and password", "http://user:" + secret + "@b.example/ip", `"http://REDACTED@b.example/ip"`},
		{"token as the user", "http://" + secret + "@b.example", `"http://REDACTED@b.example"`},
		{"nothing to hide", "http://b.example/ip", `"http://b.example/ip"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validRemote()
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", tt.url}}
			err := c.Validate()
			if err == nil {
				t.Fatal("a plain-http echo URL was accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error carries the credential: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to quote %s", err, tt.want)
			}
		})
	}
}
