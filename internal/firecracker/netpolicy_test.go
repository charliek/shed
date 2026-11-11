//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/netpolicy"
)

type nftExit struct{}

func (nftExit) Error() string { return "exit status 1" }
func (nftExit) ExitCode() int { return 1 }

// fakeNFT stands in for nft: it records every call, keeps whether this
// server's tables are "installed", and can fail applies or be missing.
type fakeNFT struct {
	mu        sync.Mutex
	calls     []string
	scripts   []string
	installed bool
	applyErr  error
	missing   bool
}

func (f *fakeNFT) Available() bool { return !f.missing }

func (f *fakeNFT) Run(_ context.Context, stdin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.missing {
		return nil, exec.ErrNotFound
	}
	if args[0] == "list" {
		if f.installed {
			return nil, nil
		}
		return nil, nftExit{}
	}
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	f.scripts = append(f.scripts, stdin)
	f.installed = strings.Contains(stdin, "{")
	return nil, nil
}

func (f *fakeNFT) last(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scripts) == 0 {
		t.Fatal("nothing applied")
	}
	return f.scripts[len(f.scripts)-1]
}

func (f *fakeNFT) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// withFakeNetpol gives c a network-policy Manager over a fake nft, and
// records the TAP up/down calls.
func withFakeNetpol(c *Client, def netpolicy.Profile) (*fakeNFT, *[]string) {
	f := &fakeNFT{}
	c.netpol = netpolicy.NewManager(netpolicy.Config{ServerName: "test", Bridge: "fc-br0", Gateway: "172.30.0.1"}, f, def)
	var links []string
	var mu sync.Mutex
	c.setLinkState = func(name string, up bool) error {
		mu.Lock()
		defer mu.Unlock()
		state := "down"
		if up {
			state = "up"
		}
		links = append(links, name+" "+state)
		return nil
	}
	return f, &links
}

// writeNetInstance writes a shed's metadata with its own TAP, IP and CID.
func writeNetInstance(t *testing.T, dir, name, status, profile string, idx int) *Metadata {
	t.Helper()
	meta := testMetadata(name)
	meta.Status = status
	meta.CID = uint32(600 + idx)
	meta.IPAddress = "172.30.0." + string(rune('2'+idx))
	meta.TAPDevice = "fc-tap-" + string(rune('0'+idx))
	meta.NetworkProfile = profile
	if err := meta.Save(dir); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestNetpolCreateGuardsBeforeTheVM(t *testing.T) {
	c := &Client{}
	f, _ := withFakeNetpol(c, netpolicy.Open)
	ctx := context.Background()

	cleanup := backend.NewCleanup()
	if err := c.guardNewShed(ctx, "iso", netpolicy.Isolated, "fc-tap-3", 603, "172.30.0.5", cleanup); err != nil {
		t.Fatal(err)
	}
	script := f.last(t)
	if !strings.Contains(script, `"fc-tap-3" . 02:fc:00:00:02:5b . 172.30.0.5`) {
		t.Errorf("entry not applied:\n%s", script)
	}
	// A failed create unwinds the entry.
	cleanup.Run()
	if c.netpol.IsGuarded("iso") || f.installed {
		t.Error("create cleanup did not remove the entry")
	}

	// An open shed makes no call.
	n := f.callCount()
	if err := c.guardNewShed(ctx, "open", netpolicy.Open, "fc-tap-4", 604, "172.30.0.6", backend.NewCleanup()); err != nil {
		t.Fatal(err)
	}
	if f.callCount() != n {
		t.Error("open shed called nft")
	}

	// Rules that cannot be applied fail the create with the sentinel (503).
	f.applyErr = errors.New("exit status 1")
	err := c.guardNewShed(ctx, "iso2", netpolicy.Isolated, "fc-tap-5", 605, "172.30.0.7", backend.NewCleanup())
	if !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Errorf("err = %v, want the enforcement sentinel", err)
	}
}

func TestNetpolResolveProfileDefaultsToServer(t *testing.T) {
	c := &Client{serverCfg: &config.ServerConfig{}}
	if p, err := c.resolveNetworkProfile(""); err != nil || p != netpolicy.Open {
		t.Errorf("empty = %q, %v; want open", p, err)
	}
	c.serverCfg.Network = &config.NetworkConfig{DefaultProfile: "isolated"}
	if p, err := c.resolveNetworkProfile(""); err != nil || p != netpolicy.Isolated {
		t.Errorf("empty with isolated default = %q, %v", p, err)
	}
	if p, err := c.resolveNetworkProfile("open"); err != nil || p != netpolicy.Open {
		t.Errorf("explicit open = %q, %v", p, err)
	}
	if _, err := c.resolveNetworkProfile("none"); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
		t.Errorf("unknown = %v", err)
	}
}

func TestNetpolDeleteRemovesEntry(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, _ := withFakeNetpol(c, netpolicy.Open)
	writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusStopped, "isolated", 0)
	writeNetInstance(t, cfg.InstanceDir, "iso2", config.StatusStopped, "isolated", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	if !strings.Contains(f.last(t), "172.30.0.2") {
		t.Fatal("fixture: shed not guarded")
	}

	if err := c.DeleteShed(context.Background(), "iso"); err != nil {
		t.Fatal(err)
	}
	if c.netpol.IsGuarded("iso") || strings.Contains(f.last(t), "172.30.0.2") {
		t.Errorf("deleted shed still applied:\n%s", f.last(t))
	}
	if !strings.Contains(f.last(t), "172.30.0.3") {
		t.Error("the other shed's entry was lost")
	}
	if err := c.DeleteShed(context.Background(), "iso2"); err != nil {
		t.Fatal(err)
	}
	if f.installed {
		t.Error("tables kept with nothing guarded and an open default")
	}
}

func TestNetpolHostAllowFollows9PAndEgress(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, _ := withFakeNetpol(c, netpolicy.Open)
	meta := writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusRunning, "isolated", 0)
	if err := c.netpol.Upsert(context.Background(), networkPolicyShed(meta, netpolicy.Isolated)); err != nil {
		t.Fatal(err)
	}

	srv, err := c.startP9Server("iso", "127.0.0.1", t.TempDir(), "/home/shed/x", false)
	if err != nil {
		t.Fatal(err)
	}
	port := srv.Addr()[strings.LastIndex(srv.Addr(), ":")+1:]
	if !strings.Contains(f.last(t), "172.30.0.2 . "+port) {
		t.Errorf("9P port %s not allowed:\n%s", port, f.last(t))
	}

	c.syncEgressPort("iso", 40123)
	if !strings.Contains(f.last(t), "172.30.0.2 . 40123") || !strings.Contains(f.last(t), "172.30.0.2 . "+port) {
		t.Errorf("egress port not added beside the 9P port:\n%s", f.last(t))
	}

	c.stopP9Servers("iso")
	if strings.Contains(f.last(t), "172.30.0.2 . "+port) || !strings.Contains(f.last(t), "172.30.0.2 . 40123") {
		t.Errorf("closing the 9P servers should drop only their ports:\n%s", f.last(t))
	}
	c.syncEgressPort("iso", 0)
	if strings.Contains(f.last(t), "40123") {
		t.Error("cleared egress port still allowed")
	}

	// An open shed's 9P servers and egress port never reach nft.
	n := f.callCount()
	srv2, err := c.startP9Server("open", "127.0.0.1", t.TempDir(), "/home/shed/y", false)
	if err != nil {
		t.Fatal(err)
	}
	c.syncEgressPort("open", 40124)
	c.stopP9Servers("open")
	_ = srv2
	if f.callCount() != n {
		t.Errorf("open shed caused %d nft calls", f.callCount()-n)
	}

	// When the allowance cannot be applied, the 9P server is not left open
	// for a mount that would fail anyway.
	f.applyErr = errors.New("exit status 1")
	if _, err := c.startP9Server("iso", "127.0.0.1", t.TempDir(), "/home/shed/z", false); !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Errorf("startP9Server with failing nft = %v", err)
	}
	if len(c.p9Ports("iso")) != 0 {
		t.Error("failed 9P server left registered")
	}
}

func TestNetpolServeStartRebuildsFromMetadata(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, _ := withFakeNetpol(c, netpolicy.Open)
	writeNetInstance(t, cfg.InstanceDir, "run-iso", config.StatusRunning, "isolated", 0)
	stopped := writeNetInstance(t, cfg.InstanceDir, "stop-iso", config.StatusStopped, "isolated", 1)
	stopped.EgressPort = 40555
	if err := stopped.Save(cfg.InstanceDir); err != nil {
		t.Fatal(err)
	}
	writeNetInstance(t, cfg.InstanceDir, "open", config.StatusRunning, "open", 2)
	writeNetInstance(t, cfg.InstanceDir, "legacy", config.StatusStopped, "", 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	script := f.last(t)
	for _, want := range []string{"172.30.0.2", "172.30.0.3", "172.30.0.3 . 40555", `"fc-tap-1"`} {
		if !strings.Contains(script, want) {
			t.Errorf("rebuild lacks %q", want)
		}
	}
	for _, unwanted := range []string{"172.30.0.4", "172.30.0.5"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("open or legacy shed %s guarded", unwanted)
		}
	}
	if got := metadataToShed(stopped).NetworkProfile; got != "isolated" {
		t.Errorf("Shed.NetworkProfile = %q", got)
	}
	legacy, _ := LoadMetadata(cfg.InstanceDir, "legacy")
	if got := metadataToShed(legacy).NetworkProfile; got != "open" {
		t.Errorf("legacy Shed.NetworkProfile = %q, want open", got)
	}
}

func TestNetpolOpenOnlyServerWithoutNFTMakesNoCalls(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, links := withFakeNetpol(c, netpolicy.Open)
	f.missing = true
	open := writeNetInstance(t, cfg.InstanceDir, "open", config.StatusStopped, "open", 0)
	writeNetInstance(t, cfg.InstanceDir, "legacy", config.StatusStopped, "", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	c.checkNetworkPolicy(ctx)
	if err := c.ensureGuardedForStart(ctx, open); err != nil {
		t.Fatal(err)
	}
	if err := c.guardNewShed(ctx, "o2", netpolicy.Open, "fc-tap-9", 609, "172.30.0.11", backend.NewCleanup()); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteShed(ctx, "open"); err != nil {
		t.Fatal(err)
	}
	if n := f.callCount(); n != 0 {
		t.Errorf("open-only server without nft made %d nft calls: %v", n, f.calls)
	}
	if len(*links) != 0 {
		t.Errorf("TAPs touched: %v", *links)
	}

	// The same server asked for an isolated shed refuses it.
	if err := c.guardNewShed(ctx, "iso", netpolicy.Isolated, "fc-tap-8", 608, "172.30.0.10", backend.NewCleanup()); !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Errorf("isolated without nft = %v", err)
	}
}

func TestNetpolStartEnsuresRulesAndTAP(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, links := withFakeNetpol(c, netpolicy.Open)
	meta := writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusStopped, "isolated", 0)
	ctx := context.Background()

	if err := c.ensureGuardedForStart(ctx, meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.last(t), "172.30.0.2") {
		t.Error("start did not apply the entry")
	}
	if len(*links) != 1 || (*links)[0] != "fc-tap-0 up" {
		t.Errorf("links = %v, want the TAP set up", *links)
	}

	f.applyErr = errors.New("exit status 1")
	if err := c.ensureGuardedForStart(ctx, meta); !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Errorf("start with failing nft = %v, want the sentinel (503)", err)
	}
}

func TestNetpolCheckReappliesAndCutsOffWhenItCannot(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, links := withFakeNetpol(c, netpolicy.Open)
	writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusRunning, "isolated", 0)
	writeNetInstance(t, cfg.InstanceDir, "iso2", config.StatusStopped, "isolated", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	applied := len(f.scripts)
	if got := strings.Join(*links, ","); got != "fc-tap-0 up,fc-tap-1 up" {
		t.Errorf("links after start = %q, want the guarded TAPs up", got)
	}
	*links = nil

	// Present: nothing to do.
	c.checkNetworkPolicy(ctx)
	if len(f.scripts) != applied {
		t.Error("check re-applied tables that were present")
	}

	// Flushed by something else: reapplied.
	f.installed = false
	c.checkNetworkPolicy(ctx)
	if !f.installed || len(f.scripts) != applied+1 {
		t.Error("missing tables not reapplied")
	}

	// Flushed and nft now failing: the guarded TAPs go down.
	f.installed = false
	f.applyErr = errors.New("exit status 1")
	c.checkNetworkPolicy(ctx)
	if got := strings.Join(*links, ","); got != "fc-tap-0 down,fc-tap-1 down" {
		t.Errorf("links = %q, want both guarded TAPs down", got)
	}

	// nft works again: reapplied, and the TAPs come back up.
	f.applyErr = nil
	c.checkNetworkPolicy(ctx)
	if !f.installed {
		t.Error("not reapplied after recovery")
	}
	if got := strings.Join(*links, ","); got != "fc-tap-0 down,fc-tap-1 down,fc-tap-0 up,fc-tap-1 up" {
		t.Errorf("links = %q, want the TAPs back up", got)
	}
}

func TestNetpolStartFailureSetsTAPsDown(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, links := withFakeNetpol(c, netpolicy.Open)
	f.applyErr = errors.New("exit status 1")
	writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusRunning, "isolated", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	if got := strings.Join(*links, ","); got != "fc-tap-0 down" {
		t.Errorf("links = %q, want the guarded TAP down", got)
	}
}

// A failed replacement leaves the old (more permissive) tables in place, so
// their presence must not end the cut-off: the check retries the desired
// state and keeps the TAPs down until it applies.
func TestNetpolFailedReplacementWithOldTablesPresent(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, links := withFakeNetpol(c, netpolicy.Open)
	f.installed = true // the previous process's tables
	f.applyErr = errors.New("nft timed out")
	writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusRunning, "isolated", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	if got := strings.Join(*links, ","); got != "fc-tap-0 down" {
		t.Fatalf("links = %q, want the guarded TAP down", got)
	}

	// Both tables exist, nft still fails: retried, and the TAP stays down.
	calls := f.callCount()
	c.checkNetworkPolicy(ctx)
	f.mu.Lock()
	retried := slices.Contains(f.calls[calls:], "-f -")
	f.mu.Unlock()
	if !retried {
		t.Error("check did not retry the failed apply")
	}
	if got := strings.Join(*links, ","); strings.Contains(got, "up") {
		t.Errorf("links = %q: TAP raised over the stale tables", got)
	}

	// nft works: the desired state is installed, then the TAP comes up.
	f.applyErr = nil
	applied := len(f.scripts)
	c.checkNetworkPolicy(ctx)
	if len(f.scripts) != applied+1 || c.netpol.Pending() {
		t.Error("desired state not applied on recovery")
	}
	if got := strings.Join(*links, ","); !strings.HasSuffix(got, "fc-tap-0 up") {
		t.Errorf("links = %q, want the TAP back up", got)
	}
}

// A process that set the guarded TAPs down and exited leaves them down; the
// next process, once its tables are installed, sets them up (a running VM
// survives the restart and cannot be started again to recover).
func TestNetpolRestartRestoresDownTAPs(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, links := withFakeNetpol(c, netpolicy.Open)
	writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusRunning, "isolated", 0)
	writeNetInstance(t, cfg.InstanceDir, "open", config.StatusRunning, "open", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx)
	if !f.installed {
		t.Fatal("tables not installed")
	}
	if got := strings.Join(*links, ","); got != "fc-tap-0 up" {
		t.Errorf("links = %q, want only the guarded TAP set up", got)
	}
	// Later checks with the tables in place change nothing.
	*links = nil
	c.checkNetworkPolicy(ctx)
	if len(*links) != 0 {
		t.Errorf("links = %v after a clean check", *links)
	}
}

// A raise that fails (other than for a TAP that does not exist) keeps the
// cut-off pending, so the next check retries it instead of treating the TAPs
// as restored.
func TestNetpolFailedRaiseIsRetried(t *testing.T) {
	c, cfg := newP9TeardownClient(t)
	f, _ := withFakeNetpol(c, netpolicy.Open)
	f.applyErr = errors.New("exit status 1")
	writeNetInstance(t, cfg.InstanceDir, "iso", config.StatusRunning, "isolated", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartNetworkPolicy(ctx) // apply fails: the TAP goes down

	var links []string
	raiseErr := errors.New("netlink: resource busy")
	c.setLinkState = func(name string, up bool) error {
		state := "down"
		if up {
			state = "up"
		}
		links = append(links, name+" "+state)
		if up {
			return raiseErr
		}
		return nil
	}
	f.applyErr = nil
	c.checkNetworkPolicy(ctx) // tables apply; the raise fails
	if got := strings.Join(links, ","); got != "fc-tap-0 up" {
		t.Fatalf("links = %q, want one raise", got)
	}
	raiseErr = nil
	c.checkNetworkPolicy(ctx) // tables in place; the raise is retried
	if got := strings.Join(links, ","); got != "fc-tap-0 up,fc-tap-0 up" {
		t.Fatalf("links = %q, want the failed raise retried", got)
	}
	c.checkNetworkPolicy(ctx) // restored: nothing more
	if len(links) != 2 {
		t.Errorf("links = %v, want no change after the raise succeeded", links)
	}
}

func TestNetpolLoopStopsWithContext(t *testing.T) {
	c := &Client{}
	withFakeNetpol(c, netpolicy.Open)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.networkPolicyLoop(ctx, time.Millisecond)
		close(done)
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
}
