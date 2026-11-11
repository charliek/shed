//go:build linux && netns

// The network-namespace test applies the real rule set with the real nft and
// probes it with real TCP. It needs root, nft and ip, and runs ENTIRELY inside
// network namespaces it creates and deletes: every link is created with
// `ip -n <ns>` (veth peers placed with `netns <ns>`) and nft only ever runs
// through a Runner that prefixes `ip netns exec <host ns>` (asserted on every
// call). The only root-namespace actions are `ip netns add` and `ip netns del`.
//
// Run it with `make test-netns-remote` (on the FC host), or by hand:
//
//	go test -c -tags netns -o netpolicy-netns.test ./internal/netpolicy
//	sudo ./netpolicy-netns.test -test.v
package netpolicy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	helperEnv  = "SHED_NETNS_HELPER"
	listenEnv  = "SHED_NETNS_LISTEN"
	dialEnv    = "SHED_NETNS_DIAL"
	dialSrcEnv = "SHED_NETNS_SRC"
)

// TestMain doubles as the probe helper: re-executed inside a namespace with
// SHED_NETNS_HELPER set, the binary listens or dials instead of testing, so
// nothing depends on nc or ping being installed.
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "listen":
		helperListen()
		return
	case "dial":
		os.Exit(helperDial())
	}
	os.Exit(m.Run())
}

// helperListen listens on every address in SHED_NETNS_LISTEN ("tcp4|addr" or
// "tcp6|addr", comma-separated), prints "ready", and accepts (and closes)
// connections until killed.
func helperListen() {
	for _, spec := range strings.Split(os.Getenv(listenEnv), ",") {
		network, addr, _ := strings.Cut(spec, "|")
		ln, err := net.Listen(network, addr)
		if err != nil {
			fmt.Printf("listen %s %s: %v\n", network, addr, err)
			os.Exit(2)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	fmt.Println("ready")
	select {}
}

// helperDial dials SHED_NETNS_DIAL (from SHED_NETNS_SRC when set) and exits 0
// when the TCP handshake completes, 1 otherwise.
func helperDial() int {
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	if src := os.Getenv(dialSrcEnv); src != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(src)}
	}
	c, err := d.Dial("tcp", os.Getenv(dialEnv))
	if err != nil {
		return 1
	}
	_ = c.Close()
	return 0
}

// topo is the namespaces of one run.
type topo struct {
	t      *testing.T
	prefix string
	exe    string
}

func (tp *topo) ns(name string) string { return tp.prefix + name }

// run runs a command and fails the test on error.
func (tp *topo) run(args ...string) {
	tp.t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		tp.t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// ipn runs `ip -n <ns> ...`.
func (tp *topo) ipn(ns string, args ...string) {
	tp.t.Helper()
	tp.run(append([]string{"ip", "-n", tp.ns(ns)}, args...)...)
}

// listen starts a listener helper in ns and waits for it.
func (tp *topo) listen(ns string, specs ...string) {
	tp.t.Helper()
	cmd := exec.Command("ip", "netns", "exec", tp.ns(ns), tp.exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=listen", listenEnv+"="+strings.Join(specs, ","))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		tp.t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		tp.t.Fatal(err)
	}
	tp.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		tp.t.Fatalf("listener in %s: %q %v", ns, line, err)
	}
}

// canDial reports whether a TCP connection from ns (optionally from src)
// to addr completes.
func (tp *topo) canDial(ns, addr, src string) bool {
	tp.t.Helper()
	cmd := exec.Command("ip", "netns", "exec", tp.ns(ns), tp.exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=dial", dialEnv+"="+addr, dialSrcEnv+"="+src)
	err := cmd.Run()
	if err == nil {
		return true
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false
	}
	tp.t.Fatalf("dial helper in %s: %v", ns, err)
	return false
}

func (tp *topo) expect(ns, addr, src string, want bool) {
	tp.t.Helper()
	if got := tp.canDial(ns, addr, src); got != want {
		from := ns
		if src != "" {
			from += " (from " + src + ")"
		}
		verdict := map[bool]string{true: "reachable", false: "blocked"}
		tp.t.Errorf("%s -> %s: got %s, want %s", from, addr, verdict[got], verdict[want])
	}
}

// nsRunner is the only way this test runs nft: always inside the host
// namespace. Every call is recorded and checked for the prefix.
type nsRunner struct {
	t     *testing.T
	inner ExecRunner
	mu    sync.Mutex
	calls [][]string
}

func (r *nsRunner) Available() bool { return r.inner.Available() }

func (r *nsRunner) Run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	full := append(append([]string{r.inner.Bin}, r.inner.Prefix...), args...)
	if len(full) < 5 || full[0] != "ip" || full[1] != "netns" || full[2] != "exec" || !strings.HasPrefix(full[3], "shedtest-") || full[4] != "nft" {
		r.t.Fatalf("nft call outside the test namespace: %v", full)
	}
	r.mu.Lock()
	r.calls = append(r.calls, full)
	r.mu.Unlock()
	return r.inner.Run(ctx, stdin, args...)
}

const (
	g1IP  = "172.30.0.2"
	g1MAC = "02:fc:00:00:00:02"
	g2IP  = "172.30.0.3"
	g2MAC = "02:fc:00:00:00:03"
	g3IP  = "172.30.0.4"
	g3MAC = "02:fc:00:00:00:04"
	gw    = "172.30.0.1"
	wanIP = "203.0.113.10"
	lanIP = "192.168.77.10"
	tsIP  = "100.64.1.1" // a tailnet stand-in behind the LAN
)

func TestNetnsIsolatedProfile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	for _, bin := range []string{"nft", "ip"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s", bin)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tp := &topo{t: t, prefix: fmt.Sprintf("shedtest-%d-", os.Getpid()), exe: exe}

	names := []string{"host", "g1", "g2", "g3", "wan", "lan"}
	for _, n := range names {
		tp.run("ip", "netns", "add", tp.ns(n))
		ns := tp.ns(n)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
		tp.ipn(n, "link", "set", "lo", "up")
	}

	// host: the bridge, with veth ports standing in for TAPs.
	tp.ipn("host", "link", "add", "shed-br0", "type", "bridge")
	tp.ipn("host", "addr", "add", gw+"/24", "dev", "shed-br0")
	tp.ipn("host", "addr", "add", "fe80::1/64", "dev", "shed-br0", "nodad")
	tp.ipn("host", "link", "set", "shed-br0", "up")
	guests := []struct{ ns, ip, mac, ll string }{
		{"g1", g1IP, g1MAC, "fe80::2"},
		{"g2", g2IP, g2MAC, "fe80::3"},
		{"g3", g3IP, g3MAC, "fe80::4"},
	}
	for i, g := range guests {
		tap := fmt.Sprintf("shed-tap-%d", i)
		tp.ipn("host", "link", "add", tap, "type", "veth", "peer", "name", "eth0", "netns", tp.ns(g.ns))
		tp.ipn("host", "link", "set", tap, "master", "shed-br0")
		tp.ipn("host", "link", "set", tap, "up")
		tp.ipn(g.ns, "link", "set", "eth0", "address", g.mac)
		tp.ipn(g.ns, "addr", "add", g.ip+"/24", "dev", "eth0")
		tp.ipn(g.ns, "addr", "add", g.ll+"/64", "dev", "eth0", "nodad")
		tp.ipn(g.ns, "link", "set", "eth0", "up")
		tp.ipn(g.ns, "route", "add", "default", "via", gw)
	}
	// wan: "the internet", routed through host (no NAT needed).
	tp.ipn("host", "link", "add", "host-wan", "type", "veth", "peer", "name", "eth0", "netns", tp.ns("wan"))
	tp.ipn("host", "addr", "add", "203.0.113.1/24", "dev", "host-wan")
	tp.ipn("host", "link", "set", "host-wan", "up")
	tp.ipn("wan", "addr", "add", wanIP+"/24", "dev", "eth0")
	tp.ipn("wan", "link", "set", "eth0", "up")
	tp.ipn("wan", "route", "add", "172.30.0.0/24", "via", "203.0.113.1")
	// lan: an RFC 1918 network and a CGNAT (tailnet) address behind it.
	tp.ipn("host", "link", "add", "host-lan", "type", "veth", "peer", "name", "eth0", "netns", tp.ns("lan"))
	tp.ipn("host", "addr", "add", "192.168.77.1/24", "dev", "host-lan")
	tp.ipn("host", "link", "set", "host-lan", "up")
	tp.ipn("host", "route", "add", tsIP+"/32", "via", lanIP)
	tp.ipn("lan", "addr", "add", lanIP+"/24", "dev", "eth0")
	tp.ipn("lan", "addr", "add", tsIP+"/32", "dev", "lo")
	tp.ipn("lan", "link", "set", "eth0", "up")
	tp.ipn("lan", "route", "add", "172.30.0.0/24", "via", "192.168.77.1")
	tp.run("ip", "netns", "exec", tp.ns("host"), "sysctl", "-qw", "net.ipv4.ip_forward=1")

	// Listeners: the host on 7001 (never allowed), 7002 (g1's "9P port") and
	// 7003 over IPv6; each guest, wan and lan on 7100.
	tp.listen("host", "tcp4|0.0.0.0:7001", "tcp4|0.0.0.0:7002", "tcp6|[::]:7003")
	for _, n := range []string{"g1", "g2", "g3", "wan", "lan"} {
		tp.listen(n, "tcp4|0.0.0.0:7100")
	}
	hostPort := func(port int) string { return fmt.Sprintf("%s:%d", gw, port) }
	at := func(ip string) string { return ip + ":7100" }
	ll := "[fe80::1%eth0]:7003"

	// Controls with no rules: the topology works.
	tp.expect("g1", at(wanIP), "", true)
	tp.expect("g1", at(lanIP), "", true)
	tp.expect("g1", at(tsIP), "", true)
	tp.expect("g1", hostPort(7001), "", true)
	tp.expect("g1", at(g2IP), "", true)
	tp.expect("g1", ll, "", true)
	if t.Failed() {
		t.Fatal("the test topology is broken before any rule is applied")
	}
	// Control for the spoofing case: with no rules, an open guest that takes
	// g1's address does reach the host from it, and moves the host's
	// neighbour entry for that address to itself.
	tp.expect("host", at(g1IP), "", true)
	tp.ipn("g2", "addr", "add", g1IP+"/32", "dev", "eth0")
	tp.ipn("g2", "neigh", "flush", "dev", "eth0")
	tp.expect("g2", hostPort(7001), g1IP, true)
	if out, _ := exec.Command("ip", "-n", tp.ns("host"), "neigh", "show", g1IP, "dev", "shed-br0").CombinedOutput(); !strings.Contains(strings.ToLower(string(out)), g2MAC) {
		t.Errorf("control: host neighbour entry for %s is %q, want the spoofer's MAC %s", g1IP, strings.TrimSpace(string(out)), g2MAC)
	}
	tp.ipn("g2", "addr", "del", g1IP+"/32", "dev", "eth0")
	tp.ipn("host", "neigh", "flush", "dev", "shed-br0")
	if t.Failed() {
		t.Fatal("the spoofing control did not behave as expected")
	}

	runner := &nsRunner{t: t, inner: ExecRunner{Bin: "ip", Prefix: []string{"netns", "exec", tp.ns("host"), "nft"}}}
	cfg := Config{ServerName: "shedtest", Bridge: "shed-br0", Gateway: gw}
	sheds := []Shed{
		{Name: "g1", TAP: "shed-tap-0", MAC: g1MAC, IP: g1IP, Profile: Isolated, P9Ports: []uint16{7002}},
		{Name: "g2", TAP: "shed-tap-1", MAC: g2MAC, IP: g2IP, Profile: Open},
		{Name: "g3", TAP: "shed-tap-2", MAC: g3MAC, IP: g3IP, Profile: Isolated},
	}
	ctx := context.Background()
	m := NewManager(cfg, runner, Open)
	if err := m.Rebuild(ctx, sheds); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ok, err := m.Present(ctx); err != nil || !ok {
		t.Fatalf("Present after apply = %v, %v", ok, err)
	}

	t.Run("internet yes, private and tailnet no", func(t *testing.T) {
		tp.t = t
		tp.expect("g1", at(wanIP), "", true)
		tp.expect("g1", at(lanIP), "", false)
		tp.expect("g1", at(tsIP), "", false)
	})
	t.Run("host only on its own ports", func(t *testing.T) {
		tp.t = t
		tp.expect("g1", hostPort(7001), "", false)
		tp.expect("g1", hostPort(7002), "", true)
		tp.expect("g1", "203.0.113.1:7001", "", false)  // the host's other address
		tp.expect("g1", "192.168.77.1:7001", "", false) // and its LAN address
		tp.expect("g3", hostPort(7002), "", false)      // 7002 is g1's, not g3's
	})
	t.Run("guest to guest", func(t *testing.T) {
		tp.t = t
		tp.expect("g1", at(g2IP), "", false)
		tp.expect("g1", at(g3IP), "", false)
		tp.expect("g2", at(g1IP), "", false)
		tp.expect("g2", at(g3IP), "", false)
		tp.expect("g3", at(g1IP), "", false)
	})
	t.Run("open guest unchanged", func(t *testing.T) {
		tp.t = t
		tp.expect("g2", hostPort(7001), "", true)
		tp.expect("g2", at(wanIP), "", true)
		tp.expect("g2", at(lanIP), "", true)
		tp.expect("g2", ll, "", true)
		tp.expect("wan", at(g2IP), "", true)
	})
	t.Run("no new inbound", func(t *testing.T) {
		tp.t = t
		tp.expect("wan", at(g1IP), "", false)
		tp.expect("lan", at(g1IP), "", false)
	})
	t.Run("guarded guest cannot use another source address", func(t *testing.T) {
		tp.t = t
		tp.ipn("g1", "addr", "add", "172.30.0.9/24", "dev", "eth0")
		tp.expect("g1", at(wanIP), "172.30.0.9", false)
		tp.expect("g1", at(wanIP), g1IP, true)
		tp.ipn("g1", "addr", "del", "172.30.0.9/24", "dev", "eth0")
	})
	t.Run("open guest cannot take a guarded address", func(t *testing.T) {
		tp.t = t
		// The host reaches g1 itself (vsock stands in for this on a real
		// host, but it fills the host's neighbour entry for g1's IP).
		tp.expect("host", at(g1IP), "", true)
		tp.ipn("g2", "addr", "add", g1IP+"/32", "dev", "eth0")
		tp.ipn("g2", "neigh", "flush", "dev", "eth0") // so g2 ARPs claiming g1's IP
		tp.expect("g2", hostPort(7001), g1IP, false)
		tp.expect("g2", at(wanIP), g1IP, false)
		tp.ipn("g2", "addr", "del", g1IP+"/32", "dev", "eth0")
		out, err := exec.Command("ip", "-n", tp.ns("host"), "neigh", "show", g1IP, "dev", "shed-br0").CombinedOutput()
		if err != nil {
			t.Fatalf("neigh show: %v %s", err, out)
		}
		if !strings.Contains(strings.ToLower(string(out)), g1MAC) {
			t.Errorf("host neighbour entry for %s is %q, want g1's MAC %s", g1IP, strings.TrimSpace(string(out)), g1MAC)
		}
		tp.expect("host", at(g1IP), "", true)
	})
	t.Run("no IPv6", func(t *testing.T) {
		tp.t = t
		tp.expect("g1", ll, "", false)
	})
	t.Run("allow_cidrs is forward-only", func(t *testing.T) {
		tp.t = t
		withAllow := cfg
		withAllow.AllowCIDRs = []string{"192.168.77.0/24"}
		m = NewManager(withAllow, runner, Open)
		if err := m.Rebuild(ctx, sheds); err != nil {
			t.Fatalf("apply: %v", err)
		}
		tp.expect("g1", at(lanIP), "", true)
		tp.expect("g1", at(tsIP), "", false)
		tp.expect("g1", "192.168.77.1:7001", "", false) // the host itself stays closed
		tp.expect("g1", at(wanIP), "", true)
	})
	t.Run("removal makes the guest open", func(t *testing.T) {
		tp.t = t
		if err := m.Remove(ctx, "g1"); err != nil {
			t.Fatal(err)
		}
		tp.expect("g1", hostPort(7001), "", true)
		tp.expect("g1", at(tsIP), "", true)
		tp.expect("g1", at(g2IP), "", true)
		tp.expect("g1", ll, "", true)
		tp.expect("g1", at(g3IP), "", false) // g3 is still guarded
		tp.expect("wan", at(g1IP), "", true)
	})
	t.Run("teardown removes only this server's tables", func(t *testing.T) {
		tp.t = t
		if _, err := runner.Run(ctx, "", "add", "table", "inet", "keepme"); err != nil {
			t.Fatal(err)
		}
		if err := m.Teardown(ctx); err != nil {
			t.Fatal(err)
		}
		if ok, err := m.Present(ctx); err != nil || ok {
			t.Errorf("Present after teardown = %v, %v", ok, err)
		}
		for _, family := range []string{"bridge", "inet"} {
			if _, err := runner.Run(ctx, "", "list", "table", family, "shed_shedtest"); err == nil {
				t.Errorf("table %s shed_shedtest survived teardown", family)
			}
		}
		if out, err := runner.Run(ctx, "", "list", "table", "inet", "keepme"); err != nil {
			t.Errorf("decoy table gone: %v %s", err, out)
		}
		// A second teardown, with nothing left, is still success.
		if err := m.Teardown(ctx); err != nil {
			t.Errorf("idempotent teardown: %v", err)
		}
		tp.expect("g3", hostPort(7001), "", true)
	})
	tp.t = t

	runner.mu.Lock()
	n := len(runner.calls)
	runner.mu.Unlock()
	if n == 0 {
		t.Error("no nft call went through the namespace runner")
	}
}
