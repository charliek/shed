package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validRemote returns a remote block that passes Validate, as a config file
// with an empty remote block would load.
func validRemote() *RemoteConfig {
	c := &RemoteConfig{}
	c.applyDefaults()
	return c
}

func TestRemoteDefaults(t *testing.T) {
	c := validRemote()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
	if c.Provider != RemoteProviderService || c.Transport != RemoteTransportSSH {
		t.Errorf("drivers = %s/%s, want service/ssh", c.Provider, c.Transport)
	}
	// The account service holds the quotas and the sizes' disks: every
	// limit is off and no disk is asked for (R16).
	if c.DiskGB != 0 || c.Limits != (RemoteLimitsConfig{}) {
		t.Errorf("disk %d limits %+v, want 0 and off", c.DiskGB, c.Limits)
	}
	if c.AgentInstall != RemoteAgentInstallIncompatible || c.Firewall.Mode != RemoteFirewallStatic {
		t.Errorf("agent_install %q, firewall %q", c.AgentInstall, c.Firewall.Mode)
	}
	if c.SSH != (RemoteSSHConfig{ClientKeyEnv: "SHED_REMOTE_SSH_KEY", ClientKeyFileEnv: "SHED_REMOTE_SSH_KEY_FILE"}) {
		t.Errorf("ssh = %+v", c.SSH)
	}
	if allow, err := c.Access.Prefixes(); err != nil || allow != nil {
		t.Errorf("access.allow = %v, %v; want none (every unicast address outside the refused classes)", allow, err)
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
}

func TestRemoteValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *RemoteConfig)
		wantErr string
	}{
		{"defaults", func(c *RemoteConfig) {}, ""},
		{"unknown provider", func(c *RemoteConfig) { c.Provider = "aws" }, "invalid provider"},
		{"the removed in-tree provider", func(c *RemoteConfig) { c.Provider = "gce" }, "docs/upgrades/remote-compute.md"},
		{"unknown transport", func(c *RemoteConfig) { c.Transport = "tailscale" }, "invalid transport"},
		{"fake without flag", func(c *RemoteConfig) { c.Provider = RemoteProviderFake }, "allow_test_drivers"},
		{"local without flag", func(c *RemoteConfig) { c.Transport = RemoteTransportLocal }, "allow_test_drivers"},
		{"fake and local with flag", func(c *RemoteConfig) {
			c.Provider, c.Transport, c.AllowTestDrivers = RemoteProviderFake, RemoteTransportLocal, true
		}, ""},
		{"relative state_dir", func(c *RemoteConfig) { c.StateDir = "state/remote" }, "state_dir must be an absolute path"},
		{"home state_dir", func(c *RemoteConfig) { c.StateDir = "~/.shed/dev-remote/remote" }, ""},
		{"relative agent_dir", func(c *RemoteConfig) { c.AgentDir = "agent" }, "agent_dir must be an absolute path"},
		{"disk too small", func(c *RemoteConfig) { c.DiskGB = 5 }, "invalid disk_gb"},
		{"disk too large", func(c *RemoteConfig) { c.DiskGB = 501 }, "invalid disk_gb"},
		{"disk set", func(c *RemoteConfig) { c.DiskGB = 80 }, ""},
		{"limits set", func(c *RemoteConfig) { c.Limits = RemoteLimitsConfig{MaxRunning: 2, MaxSheds: 4} }, ""},
		{"max_running alone", func(c *RemoteConfig) { c.Limits = RemoteLimitsConfig{MaxRunning: 2} }, ""},
		{"access.allow", func(c *RemoteConfig) { c.Access.Allow = []string{"203.0.113.0/24", "2001:db8::/32"} }, ""},
		{"access.allow not a cidr", func(c *RemoteConfig) { c.Access.Allow = []string{"203.0.113.7"} }, "invalid access.allow entry"},
		{"negative max_running", func(c *RemoteConfig) { c.Limits.MaxRunning = -1 }, "limits.max_running"},
		{"negative max_sheds", func(c *RemoteConfig) { c.Limits.MaxSheds = -1 }, "limits.max_sheds"},
		{"max_running above max_sheds", func(c *RemoteConfig) { c.Limits = RemoteLimitsConfig{MaxRunning: 9, MaxSheds: 8} }, "must not exceed"},
		{"agent_install unknown", func(c *RemoteConfig) { c.AgentInstall = "sometimes" }, "invalid agent_install"},
		{"agent_install never", func(c *RemoteConfig) { c.AgentInstall = RemoteAgentInstallNever }, ""},
		{"ssh env not a name", func(c *RemoteConfig) { c.SSH.ClientKeyEnv = "MY KEY" }, "ssh.client_key_env"},
		{"ssh pair same name", func(c *RemoteConfig) { c.SSH.ClientKeyFileEnv = c.SSH.ClientKeyEnv }, "different variables"},
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
		{"firewall static with the guest subnet", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"203.0.113.7/32", "10.10.0.0/24"}}
		}, "not public address space"},
		{"firewall static with one private address", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"192.168.1.5/32"}}
		}, "not public address space"},
		{"firewall static with carrier-grade NAT space", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"100.64.3.0/24"}}
		}, "not public address space"},
		{"firewall static with benchmarking space", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"198.18.4.0/24"}}
		}, "not public address space"},
		{"firewall static with the protocol assignments", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"192.0.0.8/32"}}
		}, "not public address space"},
		{"firewall static with a private range, acknowledged", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"10.10.0.0/24"}, AcknowledgeAny: true}
		}, ""},
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
		{"firewall auto with a host name outside ASCII", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://b\u00fccher.example/", "https://xn--bcher-kva.example/"}}
		}, "ASCII (punycode) form"},
		{"firewall auto with a punycode host name", func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://xn--bcher-kva.example/", "https://b.example/"}}
		}, ""},
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

func TestRemoteResolveDiskGB(t *testing.T) {
	const gb = int64(1) << 30
	c := validRemote()
	tests := []struct {
		name    string
		bytes   int64
		want    int
		wantErr bool
	}{
		{"zero is disk_gb (unset: the size's own)", 0, 0, false},
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

func TestRemoteResolve(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "link_ed25519")
	emptyFile := filepath.Join(dir, "empty")
	for path, body := range map[string]string{keyFile: "KEY-FROM-FILE\n", emptyFile: "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	full := func() map[string]string {
		return map[string]string{
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
		// wantSources is compared when set; "-" means none.
		wantSources string
	}{
		{name: "source ranges from the variable, canonical and sorted", mutateEnv: func(env map[string]string) {
			env["SHED_REMOTE_SOURCE_RANGES"] = " 203.0.113.7/32, 198.51.100.9/24 ,203.0.113.7/32,"
		}, wantKey: "KEY-FROM-FILE\n", wantSources: "198.51.100.0/24,203.0.113.7/32"},
		{name: "source ranges from the file", mutateCfg: func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRanges: []string{"192.0.2.44/32"}}
		}, mutateEnv: func(env map[string]string) {
			env["SHED_REMOTE_SOURCE_RANGES"] = "not read"
		}, wantKey: "KEY-FROM-FILE\n", wantSources: "192.0.2.44/32"},
		{name: "source ranges from a named variable", mutateCfg: func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallStatic, SourceRangesEnv: "MY_RANGES"}
		}, mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SOURCE_RANGES")
			env["MY_RANGES"] = "192.0.2.45/32"
		}, wantKey: "KEY-FROM-FILE\n", wantSources: "192.0.2.45/32"},
		{name: "source ranges variable unset", mutateEnv: func(env map[string]string) { delete(env, "SHED_REMOTE_SOURCE_RANGES") },
			wantErr: "SHED_REMOTE_SOURCE_RANGES (remote.firewall.source_ranges_env) is not set"},
		{name: "source ranges variable only commas", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = " , ," },
			wantErr: "lists no source range"},
		{name: "source ranges variable not a cidr", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "203.0.113.7" },
			wantErr: "must be an IPv4 CIDR"},
		{name: "source ranges variable any address", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "0.0.0.0/0" },
			wantErr: "wider than a /24"},
		{name: "source ranges variable wide, acknowledged", mutateCfg: func(c *RemoteConfig) { c.Firewall.AcknowledgeAny = true },
			mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "0.0.0.0/0" },
			wantKey:   "KEY-FROM-FILE\n", wantSources: "0.0.0.0/0"},
		{name: "auto mode reads no ranges", mutateCfg: func(c *RemoteConfig) {
			c.Firewall = RemoteFirewallConfig{Mode: RemoteFirewallAuto, EchoURLs: []string{"https://a.example", "https://b.example"}}
		}, mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "garbage" },
			wantKey: "KEY-FROM-FILE\n", wantSources: "-"},
		{name: "fake provider with a bad list is still refused", mutateCfg: func(c *RemoteConfig) {
			c.Provider, c.AllowTestDrivers = RemoteProviderFake, true
		}, mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SOURCE_RANGES"] = "garbage" }, wantErr: "must be an IPv4 CIDR"},
		{name: "fake provider may be given no list", mutateCfg: func(c *RemoteConfig) {
			c.Provider, c.AllowTestDrivers = RemoteProviderFake, true
		}, mutateEnv: func(env map[string]string) { delete(env, "SHED_REMOTE_SOURCE_RANGES") }, wantKey: "KEY-FROM-FILE\n", wantSources: "-"},
		{name: "file variable", wantKey: "KEY-FROM-FILE\n"},
		{name: "base64 variable", mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
			env["SHED_REMOTE_SSH_KEY"] = b64("KEY-INLINE")
		}, wantKey: "KEY-INLINE"},
		{name: "wrapped and unpadded base64", mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
			env["SHED_REMOTE_SSH_KEY"] = "S0VZLUlO\nTElORQ\n"
		}, wantKey: "KEY-INLINE"},
		{name: "custom variable names", mutateCfg: func(c *RemoteConfig) {
			c.SSH.ClientKeyFileEnv = "MY_KEY_FILE"
		}, mutateEnv: func(env map[string]string) {
			env["MY_KEY_FILE"] = env["SHED_REMOTE_SSH_KEY_FILE"]
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
		}, wantKey: "KEY-FROM-FILE\n"},
		{name: "no key variable", mutateEnv: func(env map[string]string) { delete(env, "SHED_REMOTE_SSH_KEY_FILE") }, wantErr: "neither environment variable SHED_REMOTE_SSH_KEY"},
		{name: "both key variables", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SSH_KEY"] = b64("x") }, wantErr: "are both set"},
		{name: "key not base64", mutateEnv: func(env map[string]string) {
			delete(env, "SHED_REMOTE_SSH_KEY_FILE")
			env["SHED_REMOTE_SSH_KEY"] = "not base64!"
		}, wantErr: "SHED_REMOTE_SSH_KEY (remote.ssh.client_key_env) is not valid base64"},
		{name: "key file missing", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SSH_KEY_FILE"] = filepath.Join(dir, "absent") }, wantErr: "SHED_REMOTE_SSH_KEY_FILE"},
		{name: "key file empty", mutateEnv: func(env map[string]string) { env["SHED_REMOTE_SSH_KEY_FILE"] = emptyFile }, wantErr: "is empty"},
		{name: "test drivers need nothing", mutateCfg: func(c *RemoteConfig) {
			c.Provider, c.Transport, c.AllowTestDrivers = RemoteProviderFake, RemoteTransportLocal, true
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
			if string(rt.SSHClientKey) != tt.wantKey {
				t.Errorf("key %q, want %q", rt.SSHClientKey, tt.wantKey)
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
		})
	}
}

// TestRemoteResolveErrorsHideValues checks that a start-up error names the
// variable and never echoes what it held.
func TestRemoteResolveErrorsHideValues(t *testing.T) {
	c := validRemote()
	env := map[string]string{"SHED_REMOTE_SSH_KEY": "secret-key-material!"}
	_, err := c.Resolve(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("bad base64 accepted")
	}
	if strings.Contains(err.Error(), "secret-key-material") {
		t.Errorf("error echoes the secret: %v", err)
	}
}

// remoteYAML is a whole remote block, with the home-relative paths kept as
// written.
const remoteYAML = `remote:
  provider: service
  transport: ssh
  allow_test_drivers: false
  state_dir: ~/.shed/dev-remote/remote
  disk_gb: 50
  limits: {max_running: 2, max_sheds: 8}
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
  access:
    allow: [203.0.113.0/24]
`

const remoteClientYAML = `  clients:
    nocturn:
      policy:
        name_prefix: nocturn-
        max_sheds: 4
        max_age: 30h
        images: [cloud, a-name-only-the-service-knows]
        network: isolated
`

func TestRemoteConfigFromYAML(t *testing.T) {
	// None of the variables the block names is set: a config must load
	// without them. Resolve, not the loader, reads the environment.
	for _, k := range []string{"SHED_REMOTE_SSH_KEY", "SHED_REMOTE_SSH_KEY_FILE", "SHED_REMOTE_SOURCE_RANGES"} {
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
	head := "name: t\ndefault_backend: remote\n"
	directory := "auth:\n  mode: mtls\n" + directoryAdmin(t) + "  directory:\n    url: https://accounts.example.test\n"
	minimal := head + directory + "remote: {}\n"
	fakeOnly := head + "remote:\n  provider: fake\n  allow_test_drivers: true\n"
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"a whole block", head + directory + remoteYAML, ""},
		{"an empty block", minimal, ""},
		{"a client naming images only the service knows", head + directory + remoteClientYAML + "https_port: 18490\n" + remoteYAML, ""},
		{"isolated default profile", minimal + "network:\n  default_profile: isolated\n", ""},
		{"no remote block", head + directory, "remote configuration is required"},
		{"the service with no directory", head + "remote: {}\n", "needs auth.directory"},
		{"the fake with no directory", fakeOnly, ""},
		{"firecracker block beside it", minimal + "firecracker: {}\n", "a firecracker: block is not allowed"},
		{"vz block beside it", minimal + "vz: {}\n", "a vz: block is not allowed"},
		{"remote block under firecracker", "name: t\ndefault_backend: firecracker\nfirecracker: {}\nremote: {}\n", "a remote: block needs default_backend: remote"},
		{"remote block under detect", "name: t\nremote: {}\n", "detect never picks remote"},
		{"fake provider without the flag", head + "remote:\n  provider: fake\n", "allow_test_drivers"},
		{"local transport without the flag", head + directory + "remote:\n  transport: local\n", "allow_test_drivers"},
		{"test drivers with the flag", head + directory + "remote:\n  provider: fake\n  transport: local\n  allow_test_drivers: true\n", ""},
		{"env_file", minimal + "env_file: /dev/null\n", "env_file is not supported"},
		{"mounts", minimal + "mounts:\n  claude: {source: /tmp, target: /home/shed/.claude}\n", "mounts are not supported"},
		{"egress enabled", minimal + "egress:\n  enabled: true\n", "egress.enabled is not supported"},
		{"allow_cidrs", minimal + "network:\n  allow_cidrs: [192.168.1.0/24]\n", "network.allow_cidrs is not supported"},
		{"extensions", minimal + "extensions:\n  enabled: [my-extension]\n", "extensions.enabled is not supported"},
		{"client server_mounts", head + directory + remoteClientYAML + "        server_mounts: [claude]\nhttps_port: 18490\nremote: {}\n", "server_mounts"},
		{"firewall any beside a client", head + directory + remoteClientYAML + "https_port: 18490\nremote:\n  firewall: {mode: any, acknowledge_any: true}\n", "not allowed with auth.clients"},
		{"firewall static acknowledged beside a client", head + directory + remoteClientYAML + "https_port: 18490\nremote:\n  firewall: {mode: static, source_ranges: [198.51.0.0/16], acknowledge_any: true}\n", "acknowledge_any is not allowed with auth.clients"},
		{"firewall static acknowledged with isolated default", head + directory + "remote:\n  firewall: {mode: static, source_ranges: [198.51.0.0/16], acknowledge_any: true}\nnetwork:\n  default_profile: isolated\n", "not allowed with remote.firewall.acknowledge_any"},
		{"firewall static acknowledged alone", head + directory + "remote:\n  firewall: {mode: static, source_ranges: [198.51.0.0/16], acknowledge_any: true}\n", ""},
		{"firewall any with isolated default", head + directory + "remote:\n  firewall: {mode: any, acknowledge_any: true}\nnetwork:\n  default_profile: isolated\n", "not allowed with remote.firewall.mode: any"},
		{"a misspelt key", head + directory + "remote:\n  disk_gbs: 50\n", "disk_gbs"},
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

	cfg, err := LoadServerConfigFromPath(write(head + directory + remoteYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ActiveDefaultImage(); got != "" {
		t.Errorf("ActiveDefaultImage = %q, want none (the service's catalog has it)", got)
	}
	if cpus, mem := cfg.ActiveDefaultResources(); cpus != 0 || mem != 0 {
		t.Errorf("ActiveDefaultResources = %d, %d; want none (the service's catalog has them)", cpus, mem)
	}
	home, _ := os.UserHomeDir()
	if got, want := cfg.Remote.StateDirPath("/unused"), filepath.Join(home, ".shed/dev-remote/remote"); got != want {
		t.Errorf("StateDirPath = %q, want %q", got, want)
	}
	if _, err := cfg.Remote.Resolve(nil); err == nil || !strings.Contains(err.Error(), "SHED_REMOTE_SSH_KEY") {
		t.Errorf("Resolve with an empty environment: %v, want an error naming SHED_REMOTE_SSH_KEY", err)
	}
}

// TestRemoteRemovedKeysRefused: a config naming a key the in-tree cloud
// provider took fails to load, naming the key and the upgrade page, through
// both loaders (plan 041 criterion 2).
func TestRemoteRemovedKeysRefused(t *testing.T) {
	directory := "auth:\n  mode: mtls\n" + directoryAdmin(t) + "  directory:\n    url: https://accounts.example.test\n"
	values := map[string]string{
		"environment":      "dev-a",
		"sizes":            "{standard: {machine_type: e2-standard-2, cpus: 2, memory_mb: 8192}}",
		"default_size":     "standard",
		"default_image":    "family/shed-full-amd64",
		"image_aliases":    "{cloud: family/shed-full-amd64}",
		"max_run_duration": "24h",
		"gce":              "{project_env: A_PROJECT_VARIABLE}",
	}
	for _, key := range RemoteRemovedKeys {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			body := "name: t\ndefault_backend: remote\n" + directory + "remote:\n  " + key + ": " + values[key] + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, load := range []func(string) (*ServerConfig, error){LoadServerConfigFromPath, LoadServerConfigForCLI} {
				cfg, err := load(path)
				if err == nil || cfg != nil {
					t.Fatalf("a config naming remote.%s loaded", key)
				}
				for _, want := range []string{"remote." + key, "was removed", RemoteUpgradePage} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %v lacks %q", err, want)
					}
				}
			}
		})
	}
	t.Run("provider gce", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "server.yaml")
		if err := os.WriteFile(path, []byte("name: t\ndefault_backend: remote\n"+directory+"remote:\n  provider: gce\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadServerConfigFromPath(path)
		if err == nil || !strings.Contains(err.Error(), "remote.provider: gce was removed") || !strings.Contains(err.Error(), RemoteUpgradePage) {
			t.Fatalf("provider gce = %v", err)
		}
	})
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
