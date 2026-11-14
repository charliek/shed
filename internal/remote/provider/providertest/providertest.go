// Package providertest is the conformance suite every provider.Provider must
// pass. A provider's own tests call Run with a Harness that builds a fresh,
// empty provider and, where the provider can do them, the out-of-band changes
// the suite needs (an instance another server made, a published host key, a
// refusal).
//
// The suite checks what the contract in package provider promises and nothing
// a particular cloud happens to do: idempotent calls, the environment guard,
// not-found rules, refusals on the call and on the operation, label and
// metadata merging, and that Pending survives a JSON round trip.
package providertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// Harness describes the provider under test.
type Harness struct {
	// Env is the environment the provider serves. Required.
	Env string
	// New returns a fresh provider with no instances, serving Env. It is
	// called once per subtest.
	New func(t *testing.T) (provider.Provider, Controls)
	// Size is the machine shape to create with. Required.
	Size provider.Size
	// Image is an exact image the provider can boot. When the provider is
	// an ImageResolver it must resolve Image to itself.
	Image string
	// Family, when set and the provider is an ImageResolver, is a family
	// name ("family/<name>") that resolves to an exact image.
	Family string
	// Timeout bounds each Wait. Zero means 30 s.
	Timeout time.Duration
}

// Controls are the out-of-band changes a harness can make. A nil field skips
// the subtests that need it.
type Controls struct {
	// Seed puts a settled, running instance with the given name and labels
	// in the cloud, as another server would have made it, and returns its
	// Ref.
	Seed func(t *testing.T, name string, labels map[string]string) provider.Ref
	// PublishHostKey sets the guest's published host key.
	PublishHostKey func(t *testing.T, ref provider.Ref, key string)
	// RefuseNextCreate makes the next Create return kind (ErrQuota or
	// ErrCapacity) from the accepting call.
	RefuseNextCreate func(t *testing.T, kind error)
	// FailNextCreateOperation makes the next Create be accepted and its
	// operation fail with kind, so Wait returns it.
	FailNextCreateOperation func(t *testing.T, kind error)
	// SetFirewall replaces the cloud's whole firewall with rules. A rule
	// whose Network is empty goes on the provider's own network; any other
	// value names another network in the same cloud.
	SetFirewall func(t *testing.T, rules []provider.FirewallRule)
}

var idCounter atomic.Int64

// NewID returns a fresh, valid shed ID.
func NewID() string {
	return fmt.Sprintf("t%025d", idCounter.Add(1))
}

// Spec returns a valid create spec for a new shed called name in env.
func (h Harness) Spec(t *testing.T, env, name string) provider.Spec {
	t.Helper()
	id := provider.Identity{Env: env, ID: NewID(), Name: name, Owner: "nocturn"}
	shed := provider.ShedSpec{
		CreatedAt:  time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC),
		Repo:       "https://token@github.com/o/r.git",
		Network:    "isolated",
		LandingDir: "/home/shed/r",
	}
	spec, err := provider.NewSpec(id, shed, []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIConformanceKey shed-server"}, h.Size, 20, h.Image, 2*time.Hour)
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	return spec
}

func (h Harness) ctx(t *testing.T) context.Context {
	t.Helper()
	d := h.Timeout
	if d <= 0 {
		d = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// Run runs the conformance suite.
func Run(t *testing.T, h Harness) {
	if h.Env == "" || h.New == nil || h.Size.MachineType == "" || h.Image == "" {
		t.Fatal("providertest: Harness needs Env, New, Size and Image")
	}
	tests := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"create wait get list", testCreate},
		{"create is idempotent", testCreateIdempotent},
		{"create refuses a bad spec", testCreateInvalid},
		{"stop and start", testStopStart},
		{"labels", testLabels},
		{"metadata", testMetadata},
		{"destroy", testDestroy},
		{"missing instance", testMissing},
		{"other environment", testForeign},
		{"wait honours its context", testWaitCancelled},
		{"guest host key", testHostKey},
		{"resolve image", testResolveImage},
		{"refusal on the call", testRefusalOnCall},
		{"refusal on the operation", testRefusalOnOperation},
		{"firewall reads", testFirewallReads},
		{"firewall sources", testFirewallSources},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

func mustCreate(t *testing.T, h Harness, p provider.Provider, name string) (provider.Spec, provider.Instance) {
	t.Helper()
	ctx := h.ctx(t)
	spec := h.Spec(t, h.Env, name)
	pend, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inst, err := p.Wait(ctx, pend)
	if err != nil {
		t.Fatalf("Wait(create): %v", err)
	}
	return spec, inst
}

func testCreate(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	spec := h.Spec(t, h.Env, "alpha")
	pend, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pend.Ref == "" || pend.Want != provider.StateRunning {
		t.Fatalf("Create pending = %+v, want a ref and want=running", pend)
	}
	// Pending is plain data: a stored copy works the same.
	raw, err := json.Marshal(pend)
	if err != nil {
		t.Fatal(err)
	}
	var stored provider.Pending
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	inst, err := p.Wait(ctx, stored)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if inst.State != provider.StateRunning || inst.Address == "" {
		t.Errorf("after create: state %s address %q, want running with an address", inst.State, inst.Address)
	}
	if inst.Ref != pend.Ref || inst.Name != spec.Name {
		t.Errorf("instance ref/name = %s/%s, want %s/%s", inst.Ref, inst.Name, pend.Ref, spec.Name)
	}
	for k, v := range spec.Labels {
		if inst.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, inst.Labels[k], v)
		}
	}
	for k, v := range spec.Metadata {
		if inst.Metadata[k] != v {
			t.Errorf("metadata %s = %q, want %q", k, inst.Metadata[k], v)
		}
	}
	for k := range inst.Metadata {
		if len(k) < 5 || k[:5] != "shed-" {
			t.Errorf("Instance.Metadata holds non-shed key %q", k)
		}
	}
	shed, err := inst.ShedSpec()
	if err != nil {
		t.Fatalf("ShedSpec: %v", err)
	}
	if shed.Repo != "https://github.com/o/r.git" || shed.Image != h.Image || shed.Size != h.Size.Name {
		t.Errorf("shed-spec = %+v", shed)
	}
	if inst.DiskGB != spec.DiskGB || inst.MachineType != h.Size.MachineType || inst.MaxRun != spec.MaxRun {
		t.Errorf("disk %d type %s maxrun %v, want %d %s %v", inst.DiskGB, inst.MachineType, inst.MaxRun, spec.DiskGB, h.Size.MachineType, spec.MaxRun)
	}
	if id, err := inst.Identity(); err != nil || id.Name != "alpha" || id.Env != h.Env {
		t.Errorf("Identity = %+v, %v", id, err)
	}

	got, err := p.Get(ctx, pend.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != provider.StateRunning || got.Address != inst.Address {
		t.Errorf("Get = %s %q, want running %q", got.State, got.Address, inst.Address)
	}

	list, err := p.List(ctx, provider.Selector{Env: h.Env})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Ref != pend.Ref {
		t.Errorf("List(env) = %v, want just %s", refs(list), pend.Ref)
	}
	other, err := p.List(ctx, provider.Selector{Env: "zz-other"})
	if err != nil {
		t.Fatalf("List(other): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("List(other env) = %v, want none", refs(other))
	}
}

func testCreateIdempotent(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	spec := h.Spec(t, h.Env, "beta")
	first, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := p.Wait(ctx, first); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	again, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if again.Ref != first.Ref {
		t.Errorf("second Create ref %s, want %s", again.Ref, first.Ref)
	}
	inst, err := p.Wait(ctx, again)
	if err != nil || inst.State != provider.StateRunning {
		t.Fatalf("Wait(second) = %s, %v", inst.State, err)
	}
	list, err := p.List(ctx, provider.Selector{Env: h.Env})
	if err != nil || len(list) != 1 {
		t.Errorf("List = %v, %v; want one instance", refs(list), err)
	}
}

func testCreateInvalid(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	t.Run("empty authorized keys", func(t *testing.T) {
		spec := h.Spec(t, h.Env, "gamma")
		spec.Metadata[provider.MetadataAuthorizedKeys] = ""
		if _, err := p.Create(ctx, spec); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("Create = %v, want ErrInvalid", err)
		}
	})
	t.Run("another environment", func(t *testing.T) {
		spec := h.Spec(t, "zz-other", "gamma")
		if _, err := p.Create(ctx, spec); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("Create = %v, want ErrInvalid", err)
		}
	})
	list, err := p.List(ctx, provider.Selector{})
	if err != nil || len(list) != 0 {
		t.Errorf("List after refused creates = %v, %v; want none", refs(list), err)
	}
}

func testStopStart(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	_, inst := mustCreate(t, h, p, "delta")
	ref := inst.Ref

	// Start on a running instance starts nothing.
	pend, err := p.Start(ctx, ref, provider.StartOptions{})
	if err != nil {
		t.Fatalf("Start(running): %v", err)
	}
	if got, err := p.Wait(ctx, pend); err != nil || got.State != provider.StateRunning {
		t.Fatalf("Wait(start running) = %s, %v", got.State, err)
	}

	pend, err = p.Stop(ctx, ref)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pend.Want != provider.StateStopped {
		t.Errorf("Stop pending want %s", pend.Want)
	}
	stopped, err := p.Wait(ctx, pend)
	if err != nil {
		t.Fatalf("Wait(stop): %v", err)
	}
	if stopped.State != provider.StateStopped || stopped.Address != "" {
		t.Errorf("after stop: %s %q, want stopped with no address", stopped.State, stopped.Address)
	}

	// Stop again is a no-op that resolves at once.
	pend, err = p.Stop(ctx, ref)
	if err != nil {
		t.Fatalf("Stop(stopped): %v", err)
	}
	if got, err := p.Wait(ctx, pend); err != nil || got.State != provider.StateStopped {
		t.Fatalf("Wait(stop stopped) = %s, %v", got.State, err)
	}

	pend, err = p.Start(ctx, ref, provider.StartOptions{MaxRun: 3 * time.Hour})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started, err := p.Wait(ctx, pend)
	if err != nil {
		t.Fatalf("Wait(start): %v", err)
	}
	if started.State != provider.StateRunning || started.Address == "" {
		t.Errorf("after start: %s %q", started.State, started.Address)
	}
	if started.MaxRun != 3*time.Hour {
		t.Errorf("after start with MaxRun 3h: MaxRun %v", started.MaxRun)
	}

	// A Settle pending on a settled instance returns it.
	if got, err := p.Wait(ctx, provider.Settle(ref)); err != nil || got.State != provider.StateRunning {
		t.Errorf("Wait(Settle) = %s, %v", got.State, err)
	}
	// Waiting for a state it is not in, with nothing in flight, says so.
	_, err = p.Wait(ctx, provider.Pending{Ref: ref, Want: provider.StateStopped})
	if !errors.Is(err, provider.ErrUnexpectedState) {
		t.Errorf("Wait(want stopped on running) = %v, want ErrUnexpectedState", err)
	}
}

func testLabels(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	_, inst := mustCreate(t, h, p, "epsilon")
	ref := inst.Ref

	set := map[string]string{
		provider.LabelState:  string(provider.ShedStateReady),
		provider.LabelLayout: provider.LayoutLabel(1),
	}
	if err := p.SetLabels(ctx, ref, set); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	got, err := p.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.ShedState() != provider.ShedStateReady || got.Layout() != 1 {
		t.Errorf("after SetLabels: state %q layout %d", got.ShedState(), got.Layout())
	}
	if got.Labels[provider.LabelName] != "epsilon" {
		t.Errorf("SetLabels dropped %s: %v", provider.LabelName, got.Labels)
	}
	// Same values again: success.
	if err := p.SetLabels(ctx, ref, set); err != nil {
		t.Errorf("SetLabels(same): %v", err)
	}
	// An empty value sets the empty string.
	if err := p.SetLabels(ctx, ref, map[string]string{provider.LabelOwner: ""}); err != nil {
		t.Errorf("SetLabels(empty owner): %v", err)
	}
	if got, _ := p.Get(ctx, ref); got.Labels[provider.LabelOwner] != "" {
		t.Errorf("owner = %q, want empty", got.Labels[provider.LabelOwner])
	} else if _, ok := got.Labels[provider.LabelOwner]; !ok {
		t.Errorf("owner label removed, want it present and empty")
	}
	for _, k := range []string{provider.LabelSchema, provider.LabelEnv, provider.LabelID} {
		if err := p.SetLabels(ctx, ref, map[string]string{k: "changed1"}); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("SetLabels(%s) = %v, want ErrInvalid", k, err)
		}
	}
	if err := p.SetLabels(ctx, ref, map[string]string{"shed_state": "Not Valid"}); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("SetLabels(bad value) = %v, want ErrInvalid", err)
	}
}

func testMetadata(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	spec, inst := mustCreate(t, h, p, "zeta")
	ref := inst.Ref
	pin := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPinnedHostKey"
	if err := p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: pin}); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err := p.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostKeyPin() != pin {
		t.Errorf("pin = %q, want %q", got.HostKeyPin(), pin)
	}
	if got.Metadata[provider.MetadataShedSpec] != spec.Metadata[provider.MetadataShedSpec] {
		t.Errorf("SetMetadata changed shed-spec")
	}
	if err := p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: pin}); err != nil {
		t.Errorf("SetMetadata(same): %v", err)
	}
	if err := p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: ""}); err != nil {
		t.Fatalf("SetMetadata(remove): %v", err)
	}
	if got, _ := p.Get(ctx, ref); got.HostKeyPin() != "" {
		t.Errorf("pin after removal = %q", got.HostKeyPin())
	}
	if err := p.SetMetadata(ctx, ref, map[string]string{"startup-script": "x"}); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("SetMetadata(non-shed key) = %v, want ErrInvalid", err)
	}
}

func testDestroy(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	_, inst := mustCreate(t, h, p, "eta")
	ref := inst.Ref
	pend, err := p.Destroy(ctx, ref)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if pend.Want != provider.StateAbsent {
		t.Errorf("Destroy pending want %s", pend.Want)
	}
	if got, err := p.Wait(ctx, pend); err != nil || got.Ref != "" {
		t.Fatalf("Wait(destroy) = %+v, %v; want the zero Instance", got, err)
	}
	if _, err := p.Get(ctx, ref); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Get after destroy = %v, want ErrNotFound", err)
	}
	again, err := p.Destroy(ctx, ref)
	if err != nil {
		t.Fatalf("Destroy(absent) = %v, want nil", err)
	}
	if _, err := p.Wait(ctx, again); err != nil {
		t.Errorf("Wait(destroy absent) = %v", err)
	}
	if _, err := p.Wait(ctx, provider.Settle(ref)); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Wait(Settle) on a deleted instance = %v, want ErrNotFound", err)
	}
	list, err := p.List(ctx, provider.Selector{Env: h.Env})
	if err != nil || len(list) != 0 {
		t.Errorf("List after destroy = %v, %v", refs(list), err)
	}
}

func testMissing(t *testing.T, h Harness) {
	p, _ := h.New(t)
	ctx := h.ctx(t)
	// A ref for an instance that never existed: create one, destroy it.
	_, inst := mustCreate(t, h, p, "theta")
	ref := inst.Ref
	pend, err := p.Destroy(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(ctx, pend); err != nil {
		t.Fatal(err)
	}
	checks := map[string]error{}
	_, checks["Get"] = p.Get(ctx, ref)
	_, checks["Start"] = p.Start(ctx, ref, provider.StartOptions{})
	_, checks["Stop"] = p.Stop(ctx, ref)
	checks["SetLabels"] = p.SetLabels(ctx, ref, map[string]string{provider.LabelState: "ready"})
	checks["SetMetadata"] = p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: "x y"})
	_, checks["Wait(running)"] = p.Wait(ctx, provider.Pending{Ref: ref, Want: provider.StateRunning})
	for name, err := range checks {
		if !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("%s on a missing instance = %v, want ErrNotFound", name, err)
		}
	}
}

func testForeign(t *testing.T, h Harness) {
	p, ctl := h.New(t)
	if ctl.Seed == nil {
		t.Skip("harness cannot seed instances")
	}
	ctx := h.ctx(t)
	id := provider.Identity{Env: "zz-other", ID: NewID(), Name: "iota"}
	ref := ctl.Seed(t, id.InstanceName(), id.Labels(provider.ShedStateReady))

	if got, err := p.Get(ctx, ref); err != nil || got.Labels[provider.LabelEnv] != "zz-other" {
		t.Errorf("Get(foreign) = %v, %v; reads are allowed", got.Labels, err)
	}
	checks := map[string]error{}
	_, checks["Start"] = p.Start(ctx, ref, provider.StartOptions{})
	_, checks["Stop"] = p.Stop(ctx, ref)
	_, checks["Destroy"] = p.Destroy(ctx, ref)
	checks["SetLabels"] = p.SetLabels(ctx, ref, map[string]string{provider.LabelState: "deleting"})
	checks["SetMetadata"] = p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: "x y"})
	for name, err := range checks {
		if !errors.Is(err, provider.ErrForeign) {
			t.Errorf("%s on another environment's instance = %v, want ErrForeign", name, err)
		}
	}
	if got, err := p.Get(ctx, ref); err != nil || got.State != provider.StateRunning || got.ShedState() != provider.ShedStateReady {
		t.Errorf("foreign instance changed: %s %q, %v", got.State, got.ShedState(), err)
	}

	mine, err := p.List(ctx, provider.Selector{Env: h.Env})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(refs(mine), ref) {
		t.Errorf("List(env) includes another environment's instance")
	}
	all, err := p.List(ctx, provider.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(refs(all), ref) {
		t.Errorf("List(all) = %v, want it to include %s", refs(all), ref)
	}
}

func testWaitCancelled(t *testing.T, h Harness) {
	p, _ := h.New(t)
	_, inst := mustCreate(t, h, p, "kappa")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Wait(ctx, provider.Pending{Ref: inst.Ref, Want: provider.StateStopped})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Wait on a cancelled context = %v, want context.Canceled", err)
	}
}

func testHostKey(t *testing.T, h Harness) {
	p, ctl := h.New(t)
	hk, ok := p.(provider.HostKeyReader)
	if !ok || ctl.PublishHostKey == nil {
		t.Skip("provider reads no host key, or harness cannot publish one")
	}
	ctx := h.ctx(t)
	_, inst := mustCreate(t, h, p, "lambda")
	if _, err := hk.GuestHostKey(ctx, inst.Ref); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("GuestHostKey before publish = %v, want ErrNotFound", err)
	}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuestKey"
	ctl.PublishHostKey(t, inst.Ref, key)
	got, err := hk.GuestHostKey(ctx, inst.Ref)
	if err != nil || got != key {
		t.Errorf("GuestHostKey = %q, %v; want %q", got, err, key)
	}
}

func testResolveImage(t *testing.T, h Harness) {
	p, _ := h.New(t)
	r, ok := p.(provider.ImageResolver)
	if !ok {
		t.Skip("provider resolves no images")
	}
	ctx := h.ctx(t)
	got, err := r.ResolveImage(ctx, h.Image)
	if err != nil || got != h.Image {
		t.Errorf("ResolveImage(%q) = %q, %v; want itself", h.Image, got, err)
	}
	if h.Family != "" {
		got, err := r.ResolveImage(ctx, h.Family)
		if err != nil || got == "" || got == h.Family {
			t.Errorf("ResolveImage(%q) = %q, %v; want an exact name", h.Family, got, err)
		}
	}
	if _, err := r.ResolveImage(ctx, ""); err == nil {
		t.Errorf("ResolveImage(\"\") succeeded")
	}
}

func testRefusalOnCall(t *testing.T, h Harness) {
	for _, kind := range []error{provider.ErrQuota, provider.ErrCapacity} {
		t.Run(kind.Error(), func(t *testing.T) {
			p, ctl := h.New(t)
			if ctl.RefuseNextCreate == nil {
				t.Skip("harness cannot inject a refusal")
			}
			ctx := h.ctx(t)
			ctl.RefuseNextCreate(t, kind)
			_, err := p.Create(ctx, h.Spec(t, h.Env, "mu"))
			if !errors.Is(err, kind) || !provider.IsRefusal(err) {
				t.Errorf("Create = %v, want %v", err, kind)
			}
			list, err := p.List(ctx, provider.Selector{Env: h.Env})
			if err != nil || len(list) != 0 {
				t.Errorf("List after refusal = %v, %v; want none", refs(list), err)
			}
		})
	}
}

func testRefusalOnOperation(t *testing.T, h Harness) {
	for _, kind := range []error{provider.ErrQuota, provider.ErrCapacity} {
		t.Run(kind.Error(), func(t *testing.T) {
			p, ctl := h.New(t)
			if ctl.FailNextCreateOperation == nil {
				t.Skip("harness cannot fail an operation")
			}
			ctx := h.ctx(t)
			ctl.FailNextCreateOperation(t, kind)
			pend, err := p.Create(ctx, h.Spec(t, h.Env, "nu"))
			if err != nil {
				// A provider may notice the failure on the call already.
				if !errors.Is(err, kind) {
					t.Fatalf("Create = %v, want nil or %v", err, kind)
				}
				return
			}
			if _, err := p.Wait(ctx, pend); !errors.Is(err, kind) {
				t.Errorf("Wait = %v, want %v", err, kind)
			}
			list, err := p.List(ctx, provider.Selector{Env: h.Env})
			if err != nil || len(list) != 0 {
				t.Errorf("List after failed create = %v, %v; want none", refs(list), err)
			}
		})
	}
}

// envRule is the environment's rule as the setup script makes it.
func envRule(env string, sources ...string) provider.FirewallRule {
	return provider.FirewallRule{
		Name:         provider.FirewallRuleName(env),
		Direction:    provider.DirectionIngress,
		Priority:     1000,
		SourceRanges: sources,
		TargetTags:   []string{provider.NetworkTag(env)},
		Allowed:      []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}},
	}
}

func testFirewallReads(t *testing.T, h Harness) {
	p, c := h.New(t)
	if c.SetFirewall == nil {
		t.Skip("the harness cannot set the cloud's firewall")
	}
	ctx := h.ctx(t)

	t.Run("no rule", func(t *testing.T) {
		c.SetFirewall(t, nil)
		if _, err := p.EnvFirewallRule(ctx); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("EnvFirewallRule with no rule: %v, want ErrNotFound", err)
		}
		view, err := p.NetworkIngress(ctx)
		if err != nil || view.Network == "" || len(view.Rules) != 0 {
			t.Errorf("NetworkIngress with no rule = %+v, %v", view, err)
		}
	})

	t.Run("rules", func(t *testing.T) {
		c.SetFirewall(t, []provider.FirewallRule{
			{Name: "zz-stray", Direction: provider.DirectionIngress, Priority: 900, SourceRanges: []string{"198.51.100.0/24"},
				Allowed: []provider.FirewallPorts{{Protocol: "all"}}},
			envRule(h.Env, "192.0.2.10/32", "203.0.113.0/24"),
			{Name: "aa-deny", Direction: provider.DirectionIngress, Priority: 500, Disabled: true, SourceTags: []string{"other"},
				TargetServiceAccounts: []string{"sa@example.iam.gserviceaccount.com"},
				Denied:                []provider.FirewallPorts{{Protocol: "udp", Ports: []string{"53", "1000-2000"}}}},
			{Name: "egress-rule", Direction: provider.DirectionEgress, DestinationRanges: []string{"0.0.0.0/0"},
				Denied: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"25"}}}},
			{Name: "elsewhere", Network: "another-network", Direction: provider.DirectionIngress, SourceRanges: []string{"0.0.0.0/0"},
				Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}}},
		})
		rule, err := p.EnvFirewallRule(ctx)
		if err != nil {
			t.Fatalf("EnvFirewallRule: %v", err)
		}
		view, err := p.NetworkIngress(ctx)
		if err != nil {
			t.Fatalf("NetworkIngress: %v", err)
		}
		if rule.Name != provider.FirewallRuleName(h.Env) || rule.Network != view.Network || rule.Direction != provider.DirectionIngress ||
			rule.Disabled || rule.Priority != 1000 ||
			!slices.Equal(rule.SourceRanges, []string{"192.0.2.10/32", "203.0.113.0/24"}) ||
			!slices.Equal(rule.TargetTags, []string{provider.NetworkTag(h.Env)}) ||
			len(rule.Allowed) != 1 || rule.Allowed[0].Protocol != "tcp" || !slices.Equal(rule.Allowed[0].Ports, []string{"22"}) || len(rule.Denied) != 0 {
			t.Errorf("EnvFirewallRule = %+v", rule)
		}
		if view.Default {
			t.Error("the test network is reported as the default network")
		}
		var names []string
		for _, r := range view.Rules {
			names = append(names, r.Name)
			if r.Network != view.Network || r.Direction != provider.DirectionIngress {
				t.Errorf("NetworkIngress returned %+v", r)
			}
		}
		// Ordered by name; the egress rule and the other network's rule
		// are left out; a disabled rule is still listed.
		if want := []string{"aa-deny", provider.FirewallRuleName(h.Env), "zz-stray"}; !slices.Equal(names, want) {
			t.Fatalf("NetworkIngress rules = %v, want %v", names, want)
		}
		deny := view.Rules[0]
		if !deny.Disabled || deny.Priority != 500 || !slices.Equal(deny.SourceTags, []string{"other"}) || len(deny.TargetServiceAccounts) != 1 ||
			len(deny.Allowed) != 0 || len(deny.Denied) != 1 || deny.Denied[0].Protocol != "udp" || !slices.Equal(deny.Denied[0].Ports, []string{"53", "1000-2000"}) {
			t.Errorf("deny rule = %+v", deny)
		}
		stray := view.Rules[2]
		if len(stray.TargetTags) != 0 || len(stray.Allowed) != 1 || stray.Allowed[0].Protocol != "all" || len(stray.Allowed[0].Ports) != 0 {
			t.Errorf("stray rule = %+v", stray)
		}
	})

	t.Run("the rule on another network is still found by name", func(t *testing.T) {
		moved := envRule(h.Env, "192.0.2.10/32")
		moved.Network = "another-network"
		c.SetFirewall(t, []provider.FirewallRule{moved})
		rule, err := p.EnvFirewallRule(ctx)
		if err != nil || rule.Network != "another-network" {
			t.Errorf("EnvFirewallRule = %+v, %v", rule, err)
		}
		view, err := p.NetworkIngress(ctx)
		if err != nil || len(view.Rules) != 0 {
			t.Errorf("NetworkIngress = %+v, %v", view, err)
		}
	})
}

func testFirewallSources(t *testing.T, h Harness) {
	p, c := h.New(t)
	setter, ok := p.(provider.FirewallSourceSetter)
	if !ok || c.SetFirewall == nil {
		t.Skip("the provider cannot set firewall sources, or the harness cannot set the firewall")
	}
	ctx := h.ctx(t)
	other := provider.FirewallRule{Name: "shed-otherenv-ssh", Direction: provider.DirectionIngress, Priority: 1000,
		SourceRanges: []string{"198.51.100.7/32"}, TargetTags: []string{"shed-otherenv"},
		Allowed: []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}}}
	c.SetFirewall(t, []provider.FirewallRule{envRule(h.Env, "192.0.2.10/32"), other})

	t.Run("invalid ranges send nothing", func(t *testing.T) {
		for _, bad := range [][]string{
			nil, {}, {"203.0.113.9"}, {"203.0.113.9/24"}, {"2001:db8::1/128"}, {"0.0.0.0/0"}, {"203.0.0.0/16"}, {"203.0.113.9/32", "junk"},
		} {
			if err := setter.SetEnvFirewallSources(ctx, bad); !errors.Is(err, provider.ErrInvalid) {
				t.Errorf("SetEnvFirewallSources(%v): %v, want ErrInvalid", bad, err)
			}
		}
		rule, err := p.EnvFirewallRule(ctx)
		if err != nil || !slices.Equal(rule.SourceRanges, []string{"192.0.2.10/32"}) {
			t.Errorf("the rule changed: %+v, %v", rule, err)
		}
	})

	t.Run("sets the one rule's sources and nothing else", func(t *testing.T) {
		before, err := p.EnvFirewallRule(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := setter.SetEnvFirewallSources(ctx, []string{"203.0.113.9/32"}); err != nil {
			t.Fatalf("SetEnvFirewallSources: %v", err)
		}
		after, err := p.EnvFirewallRule(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(after.SourceRanges, []string{"203.0.113.9/32"}) {
			t.Errorf("sources = %v", after.SourceRanges)
		}
		before.SourceRanges, after.SourceRanges = nil, nil
		if fmt.Sprintf("%+v", before) != fmt.Sprintf("%+v", after) {
			t.Errorf("more than the sources changed:\n before %+v\n after  %+v", before, after)
		}
		view, err := p.NetworkIngress(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range view.Rules {
			if r.Name == other.Name && !slices.Equal(r.SourceRanges, other.SourceRanges) {
				t.Errorf("another environment's rule changed: %+v", r)
			}
		}
	})

	t.Run("a missing rule is not created", func(t *testing.T) {
		c.SetFirewall(t, []provider.FirewallRule{other})
		if err := setter.SetEnvFirewallSources(ctx, []string{"203.0.113.9/32"}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("SetEnvFirewallSources with no rule: %v, want ErrNotFound", err)
		}
		if _, err := p.EnvFirewallRule(ctx); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("the rule exists now: %v", err)
		}
	})
}

func refs(insts []provider.Instance) []provider.Ref {
	out := make([]provider.Ref, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.Ref)
	}
	return out
}
