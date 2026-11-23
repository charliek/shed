//go:build linux

package firecracker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/vmutil"
)

// A shed's id and delivery record on Firecracker: a shed from before
// delivery reports no id and delivery none; the request names a repo shed's
// project directory and a --local-dir shed's base name; provision refuses a
// stopped shed before it touches anything and gives an id-less shed its id.
func TestFirecrackerDeliveryRecord(t *testing.T) {
	old := testMetadata("old")
	if s := metadataToShed(old); s.ID != "" || s.Delivery == nil || s.Delivery.State != delivery.StateNone || s.Delivery.Steps != nil {
		t.Fatalf("a shed from before delivery reports id %q delivery %+v", s.ID, s.Delivery)
	}

	repo := testMetadata("widgets")
	repo.ID = "abcdefghijklmnopqrstuvwxyz"
	repo.Repo = "https://token@git.example.test/example/widgets.git"
	repo.LandingDir = config.HomePath + "/widgets"
	rs := requestShed(repo)
	if rs.ID != repo.ID || rs.Backend != delivery.BackendFirecracker || rs.ProjectDir != "/home/shed/widgets" {
		t.Fatalf("request shed = %+v", rs)
	}
	if rs.Repo == nil || rs.Repo.URL != "https://git.example.test/example/widgets.git" || rs.Repo.Name != "example/widgets" {
		t.Fatalf("request repo = %+v (the URL's credential must not travel)", rs.Repo)
	}

	local := testMetadata("local")
	local.LandingDir = config.HomePath + "/proj"
	local.ProjectMounts = []config.MountConfig{{Source: "/srv/src/proj", Target: config.HomePath + "/proj"}}
	if ls := requestShed(local); ls.LocalDir != "proj" || ls.Repo != nil {
		t.Fatalf("local-dir request shed = %+v", ls)
	}

	dir := t.TempDir()
	stopped := testMetadata("stopped")
	if err := stopped.Save(dir); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: &config.FirecrackerConfig{InstanceDir: dir}, serverCfg: &config.ServerConfig{Name: "test"}}
	if _, err := c.ProvisionShed(context.Background(), "stopped"); !errors.Is(err, config.ErrShedNotRunningSentinel) {
		t.Fatalf("provision of a stopped shed: %v", err)
	}
	got, err := LoadMetadata(dir, "stopped")
	if err != nil {
		t.Fatal(err)
	}
	// The refused provision still gave the shed its permanent id: it is
	// minted once, under the lock, and kept.
	if got.ID == "" || !config.ValidShedID(got.ID) || got.Delivery != nil {
		t.Fatalf("after a refused provision: id %q, delivery %+v", got.ID, got.Delivery)
	}
}

// A provision's clone runs with the shed's egress proxy env, rebuilt from its
// persisted assignment exactly as the create's clone got it, whether or not
// the server's env_file variables are withheld; none when egress is off.
func TestFirecrackerProvisionCloneEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := testFirecrackerConfig(dir)
	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: cfg, serverCfg: &config.ServerConfig{Name: "test"}, netMgr: netMgr, vms: map[string]*VM{}}

	meta := testMetadata("egressed")
	meta.EgressPort, meta.EgressToken = 41234, "tok"
	if err := meta.Save(dir); err != nil {
		t.Fatal(err)
	}
	if env := c.cloneEnvFor(meta); env != nil {
		t.Fatalf("with egress off on the server: %v", env)
	}

	c.egressMgr = newTestEgressManager(t)
	gateway, subnet := c.egressGatewaySubnet()
	want := vmutil.EgressProxyEnv{Gateway: gateway, Port: 41234, Token: "tok", NoProxy: vmutil.BuildNoProxy(gateway, subnet)}.EnvPairs()
	got := c.cloneEnvFor(meta)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("clone env = %v, want %v", got, want)
	}
	ls, err := c.LoadShed(context.Background(), "egressed", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ls.CloneEnv, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the provision's clone env = %v", ls.CloneEnv)
	}

	off := testMetadata("open")
	if err := off.Save(dir); err != nil {
		t.Fatal(err)
	}
	if ls, err := c.LoadShed(context.Background(), "open", false); err != nil || ls.CloneEnv != nil {
		t.Fatalf("a shed with egress off: %v %v", ls, err)
	}
}
