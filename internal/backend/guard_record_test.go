package backend

import (
	"context"
	"runtime"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// The report step: every shed record a Guard returns carries the members
// computed when it is reported (isolation, and arch where the backend knows
// it), on every method that returns one and for every principal, and the
// backend's own record is never changed.

// reportBackend is a recBackend of a given type whose lifecycle and egress
// calls return the shed they acted on, as the real backends do, and which
// is asynchronous (WaitSettled returns the shed as held).
type reportBackend struct {
	*recBackend
	typ Type
}

func (b reportBackend) Type() Type { return b.typ }

func (b reportBackend) returning(ctx context.Context, op, name string) (*config.Shed, error) {
	if err := b.act(ctx, op, name); err != nil {
		return nil, err
	}
	sh, err := b.checked(ctx, name)
	if err != nil {
		return nil, err
	}
	return &sh, nil
}

func (b reportBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.returning(ctx, "StartShed", name)
}

func (b reportBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.returning(ctx, "StopShed", name)
}

func (b reportBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.returning(ctx, "ResetShed", name)
}

func (b reportBackend) SetShedEgress(ctx context.Context, name string, _ []string) (*config.Shed, error) {
	return b.returning(ctx, "SetShedEgress", name)
}

func (b reportBackend) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	return b.returning(ctx, "ClearShedEgress", name)
}

func (b reportBackend) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	return b.GetShed(ctx, name)
}

// remoteReport is a reportBackend that reports the remote backend's
// capabilities. isolated is whether it offers the isolated profile now (a
// remote server whose firewall config admits more than the server does not).
type remoteReport struct {
	reportBackend
	isolated bool
}

func (remoteReport) Capabilities() Capabilities { return RemoteCapabilities() }

func (b remoteReport) SupportsNetworkProfile(p string) bool {
	return p == config.NetworkProfileOpen || (p == config.NetworkProfileIsolated && b.isolated)
}

// reportFixture holds, on a backend of typ, alice's open shed "box", her
// isolated shed "iso", a shed with no stored profile "old", one whose
// record carries its own arch "arm", and an unassigned shed "free" (for
// assign). It is wrapped as remote (with isolation in force or not) when
// remote is set.
func reportFixture(typ Type, remote, isolatedOK bool) (*recBackend, *Guard) {
	b := newRecBackend()
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0, Status: config.StatusRunning, NetworkProfile: config.NetworkProfileOpen})
	b.put(config.Shed{Name: "iso", User: "alice", CreatedAt: t0, Status: config.StatusRunning, NetworkProfile: config.NetworkProfileIsolated})
	b.put(config.Shed{Name: "old", User: "alice", CreatedAt: t0, Status: config.StatusStopped})
	b.put(config.Shed{Name: "arm", User: "alice", CreatedAt: t0, Status: config.StatusStopped, NetworkProfile: config.NetworkProfileOpen, Arch: "arm64"})
	b.put(config.Shed{Name: "free", CreatedAt: t0, Status: config.StatusStopped, NetworkProfile: config.NetworkProfileIsolated, Mounts: config.SelectedMounts(nil)})
	rb := reportBackend{recBackend: b, typ: typ}
	if remote {
		return b, NewGuard(remoteReport{reportBackend: rb, isolated: isolatedOK})
	}
	return b, NewGuard(rb)
}

func TestIsolationOf(t *testing.T) {
	fw := Capabilities{Isolation: IsolationFirewall}
	topo := Capabilities{Isolation: IsolationTopology}
	for _, tc := range []struct {
		name    string
		caps    Capabilities
		profile string
		want    string
	}{
		{"open on firecracker", fw, config.NetworkProfileOpen, IsolationNone},
		{"no stored profile is open", fw, "", IsolationNone},
		{"isolated on firecracker", fw, config.NetworkProfileIsolated, IsolationFirewall},
		{"isolated on remote", topo, config.NetworkProfileIsolated, IsolationTopology},
		{"open on remote", topo, config.NetworkProfileOpen, IsolationNone},
		// Fail closed: never the profile's name, never a kind it does not know.
		{"isolated on a backend with no isolation kind", Capabilities{}, config.NetworkProfileIsolated, IsolationNone},
		{"isolated on an unknown kind", Capabilities{Isolation: "wishful"}, config.NetworkProfileIsolated, IsolationNone},
		{"an unknown profile", fw, "sealed", IsolationNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsolationOf(tc.caps, tc.profile); got != tc.want {
				t.Errorf("IsolationOf(%+v, %q) = %q, want %q", tc.caps, tc.profile, got, tc.want)
			}
		})
	}
}

// TestGuardRecordIsolationPerBackend: what each backend's records report,
// read through GetShed and ListSheds alike. An isolated shed on a backend
// that holds no isolation reports none (fail closed).
func TestGuardRecordIsolationPerBackend(t *testing.T) {
	for _, tc := range []struct {
		name                string
		typ                 Type
		remote, isolatedOK  bool
		open, iso, old      string
		wantArch, wantOther string
	}{
		{"firecracker", TypeFirecracker, false, false, IsolationNone, IsolationFirewall, IsolationNone, runtime.GOARCH, "arm64"},
		{"vz", TypeVZ, false, false, IsolationNone, IsolationNone, IsolationNone, runtime.GOARCH, "arm64"},
		{"remote", TypeRemote, true, true, IsolationNone, IsolationTopology, IsolationNone, "", "arm64"},
		{"remote whose firewall admits more than the server", TypeRemote, true, false, IsolationNone, IsolationNone, IsolationNone, "", "arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, g := reportFixture(tc.typ, tc.remote, tc.isolatedOK)
			ctx := as(alice)
			want := map[string]string{"box": tc.open, "iso": tc.iso, "old": tc.old, "arm": tc.open}
			for name, w := range want {
				sh, err := g.GetShed(ctx, name)
				if err != nil {
					t.Fatalf("GetShed %s: %v", name, err)
				}
				if sh.Isolation != w {
					t.Errorf("GetShed %s: isolation %q, want %q", name, sh.Isolation, w)
				}
			}
			list, err := g.ListSheds(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, sh := range list {
				if w, ok := want[sh.Name]; ok && sh.Isolation != w {
					t.Errorf("ListSheds %s: isolation %q, want %q", sh.Name, sh.Isolation, w)
				}
				// network_profile is never rewritten.
				if sh.Name == "old" && sh.NetworkProfile != "" {
					t.Errorf("ListSheds old: network_profile %q, want it as stored (empty)", sh.NetworkProfile)
				}
				if sh.Name == "iso" && sh.NetworkProfile != config.NetworkProfileIsolated {
					t.Errorf("ListSheds iso: network_profile %q", sh.NetworkProfile)
				}
				wantArch := tc.wantArch
				if sh.Name == "arm" {
					wantArch = tc.wantOther
				}
				if sh.Arch != wantArch {
					t.Errorf("ListSheds %s: arch %q, want %q", sh.Name, sh.Arch, wantArch)
				}
			}
		})
	}
}

// TestGuardListStampsEveryPrincipal: ListSheds stamps every row for every
// kind of caller, the unrestricted ones (the system, the implicit user)
// included, whose rows take a path of their own.
func TestGuardListStampsEveryPrincipal(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Principal
		rows int
	}{
		{"system principal", SystemPrincipal(), 5},
		{"implicit user", ImplicitPrincipal(), 5},
		{"named admin", root, 5},
		{"user", alice, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, g := reportFixture(TypeFirecracker, false, false)
			list, err := g.ListSheds(as(tc.p))
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != tc.rows {
				t.Fatalf("listed %d sheds, want %d", len(list), tc.rows)
			}
			for _, sh := range list {
				if sh.Isolation == "" || sh.Arch == "" {
					t.Errorf("%s: isolation %q arch %q, want both set", sh.Name, sh.Isolation, sh.Arch)
				}
			}
		})
	}
	// A client sees only its own sheds, each stamped too.
	b, g := reportFixture(TypeFirecracker, false, false)
	b.put(config.Shed{Name: "ci-1", Owner: "ci", CreatedAt: t0, NetworkProfile: config.NetworkProfileIsolated})
	list, err := g.ListSheds(as(ClientPrincipal("ci")))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Isolation != IsolationFirewall || list[0].Arch != runtime.GOARCH {
		t.Errorf("client's list = %+v, want ci-1 with isolation firewall and arch %s", list, runtime.GOARCH)
	}
}

// TestGuardEveryRecordMethodStamps: every Guard method that returns a shed
// record returns it stamped.
func TestGuardEveryRecordMethodStamps(t *testing.T) {
	_, g := reportFixture(TypeFirecracker, false, false)
	al, ok := IsAsync(g)
	if !ok {
		t.Fatal("the fixture's guard is not asynchronous")
	}
	ec, ok := AsEgressController(g)
	if !ok {
		t.Fatal("the fixture's guard has no egress controller")
	}
	ctx := as(alice)
	for _, tc := range []struct {
		name string
		call func() (*config.Shed, error)
		want string
	}{
		{"Check", func() (*config.Shed, error) { return g.Check(ctx, "iso", ActionEnter) }, IsolationFirewall},
		{"GetShed", func() (*config.Shed, error) { return g.GetShed(ctx, "iso") }, IsolationFirewall},
		{"CreateShed", func() (*config.Shed, error) {
			return g.CreateShed(ctx, config.CreateShedRequest{Name: "new", ResolvedMounts: config.SelectedMounts(nil)})
		}, IsolationNone},
		{"StartShed", func() (*config.Shed, error) { return g.StartShed(ctx, "iso") }, IsolationFirewall},
		{"StopShed", func() (*config.Shed, error) { return g.StopShed(ctx, "iso") }, IsolationFirewall},
		{"ResetShed", func() (*config.Shed, error) { return g.ResetShed(ctx, "iso") }, IsolationFirewall},
		{"AssignUser", func() (*config.Shed, error) {
			return g.AssignUser(as(root), "free", config.AssignShedRequest{User: "bob"})
		}, IsolationFirewall},
		{"WaitSettled", func() (*config.Shed, error) { return al.WaitSettled(ctx, "iso") }, IsolationFirewall},
		{"SetShedEgress", func() (*config.Shed, error) { return ec.SetShedEgress(ctx, "iso", []string{"base"}) }, IsolationFirewall},
		{"ClearShedEgress", func() (*config.Shed, error) { return ec.ClearShedEgress(ctx, "iso") }, IsolationFirewall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, err := tc.call()
			if err != nil {
				t.Fatal(err)
			}
			if sh.Isolation != tc.want || sh.Arch != runtime.GOARCH {
				t.Errorf("isolation %q arch %q, want %q and %q", sh.Isolation, sh.Arch, tc.want, runtime.GOARCH)
			}
		})
	}
}

// TestGuardReportLeavesTheStoredRecord: a backend that hands out its own
// record by pointer (as one with a cache could) gets it back unchanged; the
// guard reports a copy.
func TestGuardReportLeavesTheStoredRecord(t *testing.T) {
	b := newRecBackend()
	stored := config.Shed{Name: "iso", User: "alice", CreatedAt: t0, NetworkProfile: config.NetworkProfileIsolated}
	b.put(stored)
	held := stored
	g := NewGuard(heldRecord{recBackend: b, held: &held})
	sh, err := g.GetShed(as(alice), "iso")
	if err != nil {
		t.Fatal(err)
	}
	if sh == &held {
		t.Fatal("the guard returned the backend's own record")
	}
	if sh.Isolation != IsolationFirewall || sh.Arch != runtime.GOARCH {
		t.Errorf("reported isolation %q arch %q", sh.Isolation, sh.Arch)
	}
	if held.Isolation != "" || held.Arch != "" || held.NetworkProfile != config.NetworkProfileIsolated {
		t.Errorf("the backend's record changed: %+v", held)
	}
}

// TestGuardReportReplacesABackendIsolation: the report step decides
// isolation itself; a value a backend left on its record is not passed on.
func TestGuardReportReplacesABackendIsolation(t *testing.T) {
	b := newRecBackend()
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0, NetworkProfile: config.NetworkProfileOpen, Isolation: IsolationTopology})
	sh, err := NewGuard(reportBackend{recBackend: b, typ: TypeVZ}).GetShed(as(alice), "box")
	if err != nil {
		t.Fatal(err)
	}
	if sh.Isolation != IsolationNone {
		t.Errorf("isolation %q, want %q", sh.Isolation, IsolationNone)
	}
}
