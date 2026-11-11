package netpolicy

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func testConfig() Config {
	return Config{ServerName: "mini3-dev", Bridge: "shed-br0", Gateway: "172.30.0.1"}
}

func mixedSheds() []Shed {
	return []Shed{
		{Name: "web", TAP: "shed-tap-2", MAC: "02:FC:00:00:02:5A", IP: "172.30.0.4", Profile: Isolated},
		{Name: "dev", TAP: "shed-tap-1", MAC: "02:FC:00:00:02:59", IP: "172.30.0.3", Profile: Open, EgressPort: 40002},
		{Name: "agent", TAP: "shed-tap-0", MAC: "02:FC:00:00:02:58", IP: "172.30.0.2", Profile: Isolated, EgressPort: 40001, P9Ports: []uint16{41007, 41003, 41003}},
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("render differs from %s (run with -update after checking):\n%s", path, got)
	}
}

func TestRenderGoldens(t *testing.T) {
	withAllow := testConfig()
	withAllow.AllowCIDRs = []string{"192.168.77.5/24", "10.20.0.0/16"}
	cases := []struct {
		golden string
		cfg    Config
		sheds  []Shed
	}{
		{"mixed_allow.nft", withAllow, mixedSheds()},
		{"mixed_noallow.nft", testConfig(), mixedSheds()},
		{"no_guarded.nft", testConfig(), []Shed{{Name: "dev", TAP: "shed-tap-1", MAC: "02:fc:00:00:02:59", IP: "172.30.0.3", Profile: Open}}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := Render(tc.cfg, tc.sheds)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, tc.golden, got)
		})
	}
}

// Nested and duplicate allow_cidrs render as the outermost ranges only, so
// the interval set has no overlapping elements (nft refuses those without
// auto-merge), and the script equals the one for the non-overlapping list.
func TestRenderDropsCoveredAllowCIDRs(t *testing.T) {
	overlapping := testConfig()
	overlapping.AllowCIDRs = []string{"10.20.0.0/16", "192.168.77.5/24", "10.0.0.0/8", "10.20.30.0/24", "192.168.77.0/24", "10.0.0.0/8"}
	got, err := Render(overlapping, mixedSheds())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "elements = { 192.168.77.0/24, 10.0.0.0/8 }") {
		t.Errorf("allow4 should hold only 192.168.77.0/24 and 10.0.0.0/8:\n%s", got)
	}
	plain := testConfig()
	plain.AllowCIDRs = []string{"192.168.77.0/24", "10.0.0.0/8"}
	want, err := Render(plain, mixedSheds())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("overlapping allow_cidrs render differently from their outermost ranges")
	}
}

// The order of the input never changes the script, so an unchanged state
// renders the same bytes.
func TestRenderDeterministic(t *testing.T) {
	a, err := Render(testConfig(), mixedSheds())
	if err != nil {
		t.Fatal(err)
	}
	rev := mixedSheds()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	b, err := Render(testConfig(), rev)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("render depends on input order")
	}
}

func TestRenderShape(t *testing.T) {
	got, err := Render(testConfig(), mixedSheds())
	if err != nil {
		t.Fatal(err)
	}
	// The open shed is in no set.
	if strings.Contains(got, "172.30.0.3") || strings.Contains(got, "shed-tap-1") || strings.Contains(got, "40002") {
		t.Error("an open shed was rendered into a set")
	}
	// The script replaces only this server's two tables, atomically.
	for _, want := range []string{
		"add table bridge shed_mini3_dev\ndelete table bridge shed_mini3_dev\n",
		"add table inet shed_mini3_dev\ndelete table inet shed_mini3_dev\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "table ") && !strings.Contains(line, "shed_mini3_dev") && !strings.HasPrefix(line, "#") {
			t.Errorf("names another table: %q", line)
		}
		// Only drops are added: an accept is only ever a chain's own verdict.
		if strings.Contains(line, "flush") || strings.Contains(line, "ruleset") {
			t.Errorf("touches more than its tables: %q", line)
		}
	}
	// MACs are lowercased; host ports are deduplicated.
	if !strings.Contains(got, `"shed-tap-0" . 02:fc:00:00:02:58 . 172.30.0.2`) {
		t.Error("addr4 element missing or not lowercased")
	}
	// A guarded shed's MAC is bound to its port whatever the frame carries:
	// after the guarded ports have jumped to their own verification, any
	// other port sending with a guarded MAC is dropped before the bridge can
	// learn it, ahead of the claimed-IP checks.
	if !strings.Contains(got, "elements = { 02:fc:00:00:02:58, 02:fc:00:00:02:5a }") {
		t.Error("guarded_macs set missing or wrong")
	}
	jump := strings.Index(got, "iifname @guarded_ports jump from_guarded")
	macDrop := strings.Index(got, "ether saddr @guarded_macs drop")
	arp := strings.Index(got, "ether type arp arp saddr ip @guarded_ip4 jump verify_arp")
	if jump < 0 || macDrop < jump || arp < macDrop {
		t.Errorf("prerouting order: jump %d, MAC drop %d, ARP check %d", jump, macDrop, arp)
	}
	if strings.Count(got, "172.30.0.2 . 41003") != 1 {
		t.Error("host port not deduplicated")
	}
	if strings.Contains(got, "elements = {  }") || strings.Contains(got, "elements = { }") {
		t.Error("empty elements clause")
	}
}

func TestRenderRefusesBadInput(t *testing.T) {
	ok := Shed{Name: "a", TAP: "shed-tap-0", MAC: "02:fc:00:00:00:02", IP: "172.30.0.2", Profile: Isolated}
	cases := map[string]func(*Config, *Shed){
		"bridge with quote": func(c *Config, _ *Shed) { c.Bridge = `br"0` },
		"bridge too long":   func(c *Config, _ *Shed) { c.Bridge = "a-very-long-bridge-name" },
		"gateway v6":        func(c *Config, _ *Shed) { c.Gateway = "fd00::1" },
		"allow not cidr":    func(c *Config, _ *Shed) { c.AllowCIDRs = []string{"192.168.1.1"} },
		"tap injection":     func(_ *Config, s *Shed) { s.TAP = `x" accept;` },
		"bad mac":           func(_ *Config, s *Shed) { s.MAC = "02:fc:00:00:00" },
		"bad ip":            func(_ *Config, s *Shed) { s.IP = "172.30.0.300" },
	}
	for name, mut := range cases {
		cfg, s := testConfig(), ok
		mut(&cfg, &s)
		if _, err := Render(cfg, []Shed{s}); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	// An open shed is never validated or rendered.
	bad := Shed{Name: "o", TAP: `"`, Profile: Open}
	if _, err := Render(testConfig(), []Shed{bad}); err != nil {
		t.Errorf("open shed validated: %v", err)
	}
}

func TestTableName(t *testing.T) {
	for in, want := range map[string]string{
		"mini3-dev":    "shed_mini3_dev",
		"mini3":        "shed_mini3",
		"my.server 1!": "shed_my_server_1_",
		"A_b":          "shed_A_b",
	} {
		if got := TableName(in); got != want {
			t.Errorf("TableName(%q) = %q, want %q", in, got, want)
		}
	}
	got := RenderTeardown("mini3-dev")
	want := "add table bridge shed_mini3_dev\ndelete table bridge shed_mini3_dev\nadd table inet shed_mini3_dev\ndelete table inet shed_mini3_dev\n"
	if got != want {
		t.Errorf("teardown = %q", got)
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]Profile{"": Open, "open": Open, "isolated": Isolated} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"none", "proxy-only", "Isolated"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
	if Open.Guarded() || !Isolated.Guarded() {
		t.Error("Guarded")
	}
}
