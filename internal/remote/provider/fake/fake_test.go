package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

const testEnv = "dev-test"

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample shed-server"

func specFor(t *testing.T, env, id, name string) provider.Spec {
	t.Helper()
	spec, err := provider.NewSpec(
		provider.Identity{Env: env, ID: id, Name: name, Owner: "nocturn"},
		provider.ShedSpec{CreatedAt: time.Unix(1700000000, 0), Network: "isolated"},
		[]string{testKey},
		provider.DefaultSizes()[0],
		provider.DefaultDiskGB,
		"shed-extensions-test",
		24*time.Hour,
	)
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	return spec
}

// create makes a running instance on a provider whose operations finish at
// once or are completed by hand.
func create(t *testing.T, p *Provider, id, name string) provider.Ref {
	t.Helper()
	pend, err := p.Create(context.Background(), specFor(t, testEnv, id, name))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p.Complete(pend.Ref)
	if _, err := p.Wait(context.Background(), pend); err != nil {
		t.Fatalf("Wait after create: %v", err)
	}
	return pend.Ref
}

func mustState(t *testing.T, p *Provider, ref provider.Ref, want provider.State) provider.Instance {
	t.Helper()
	inst, err := p.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.State != want {
		t.Fatalf("state = %s, want %s", inst.State, want)
	}
	return inst
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	p := New(Options{Env: testEnv, Now: func() time.Time { return now }})

	pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pend.Ref == "" || pend.Want != provider.StateRunning {
		t.Fatalf("Create pending = %+v", pend)
	}
	inst, err := p.Wait(ctx, pend)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if inst.State != provider.StateRunning || inst.Address == "" {
		t.Fatalf("after create: state %s address %q", inst.State, inst.Address)
	}
	if inst.Name != "shed-dev-test-aaaaaaaa" || inst.ShedState() != provider.ShedStateCreating {
		t.Errorf("name %q shed_state %q", inst.Name, inst.ShedState())
	}
	if !inst.PlannedStopAt.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("PlannedStopAt = %v, want start plus the run limit", inst.PlannedStopAt)
	}
	if inst.CPUs != 2 || inst.MemoryMB != 8192 || inst.DiskGB != provider.DefaultDiskGB {
		t.Errorf("shape = %d CPUs, %d MB, %d GB", inst.CPUs, inst.MemoryMB, inst.DiskGB)
	}
	firstAddr := inst.Address

	stop, err := p.Stop(ctx, pend.Ref)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	inst, err = p.Wait(ctx, stop)
	if err != nil {
		t.Fatalf("Wait after stop: %v", err)
	}
	if inst.State != provider.StateStopped || inst.Address != "" || !inst.PlannedStopAt.IsZero() {
		t.Fatalf("after stop: %+v", inst)
	}

	start, err := p.Start(ctx, pend.Ref, provider.StartOptions{MaxRun: time.Hour})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	inst, err = p.Wait(ctx, start)
	if err != nil {
		t.Fatalf("Wait after start: %v", err)
	}
	if inst.Address == "" || inst.Address == firstAddr {
		t.Errorf("address after restart = %q, want a new one (was %q)", inst.Address, firstAddr)
	}
	if inst.MaxRun != time.Hour || !inst.PlannedStopAt.Equal(now.Add(time.Hour)) {
		t.Errorf("run limit after start = %v, planned stop %v", inst.MaxRun, inst.PlannedStopAt)
	}

	destroy, err := p.Destroy(ctx, pend.Ref)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if destroy.Want != provider.StateAbsent {
		t.Errorf("Destroy pending wants %q", destroy.Want)
	}
	if _, err := p.Wait(ctx, destroy); err != nil {
		t.Fatalf("Wait after destroy: %v", err)
	}
	if _, err := p.Get(ctx, pend.Ref); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Get after destroy = %v, want ErrNotFound", err)
	}
}

func TestIdempotence(t *testing.T) {
	ctx := context.Background()

	t.Run("create again with the same shed id changes nothing", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		ref := create(t, p, "aaaaaaaa", "one")
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil || pend.Ref != ref {
			t.Fatalf("second Create = %+v, %v", pend, err)
		}
		if got, _ := p.List(ctx, provider.Selector{}); len(got) != 1 {
			t.Errorf("instances = %d, want 1", len(got))
		}
	})

	t.Run("create while the first create is in flight returns the same operation", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		first, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil || second != first {
			t.Errorf("second Create = %+v, %v; want %+v", second, err, first)
		}
	})

	t.Run("start on running is a no-op", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		ref := create(t, p, "aaaaaaaa", "one")
		before := mustState(t, p, ref, provider.StateRunning)
		pend, err := p.Start(ctx, ref, provider.StartOptions{})
		if err != nil || pend.Operation != "" {
			t.Fatalf("Start = %+v, %v", pend, err)
		}
		after, err := p.Wait(ctx, pend)
		if err != nil || after.Address != before.Address {
			t.Errorf("Wait = %+v, %v; the address must not change", after, err)
		}
	})

	t.Run("stop on stopped is a no-op", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		ref := create(t, p, "aaaaaaaa", "one")
		if _, err := p.Stop(ctx, ref); err != nil {
			t.Fatal(err)
		}
		pend, err := p.Stop(ctx, ref)
		if err != nil || pend.Operation != "" || pend.Want != provider.StateStopped {
			t.Fatalf("second Stop = %+v, %v", pend, err)
		}
		if _, err := p.Wait(ctx, pend); err != nil {
			t.Errorf("Wait: %v", err)
		}
	})

	t.Run("destroy of a missing instance is success", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		pend, err := p.Destroy(ctx, provider.Ref("fake/never-existed"))
		if err != nil {
			t.Fatalf("Destroy = %v, want nil", err)
		}
		if _, err := p.Wait(ctx, pend); err != nil {
			t.Errorf("Wait = %v, want nil", err)
		}
	})

	t.Run("destroy twice returns the same operation", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		ref := create(t, p, "aaaaaaaa", "one")
		first, err := p.Destroy(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		mustState(t, p, ref, provider.StateStopping)
		second, err := p.Destroy(ctx, ref)
		if err != nil || second != first {
			t.Errorf("second Destroy = %+v, %v; want %+v", second, err, first)
		}
	})
}

func TestCreateNameTaken(t *testing.T) {
	ctx := context.Background()
	p := New(Options{})
	// Two environments whose names and IDs collapse to one instance name
	// cannot happen with valid IDs, so seed the collision.
	spec := specFor(t, testEnv, "aaaaaaaa", "one")
	labels := provider.Identity{Env: testEnv, ID: "bbbbbbbb", Name: "other"}.Labels(provider.ShedStateReady)
	p.Seed(provider.Instance{Name: spec.Name, State: provider.StateRunning, Labels: labels})
	if _, err := p.Create(ctx, spec); !errors.Is(err, provider.ErrExists) {
		t.Errorf("Create = %v, want ErrExists", err)
	}
}

func TestBusyDuringTransitions(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		// arrange leaves an operation in flight and returns its Pending.
		arrange func(t *testing.T, p *Provider, ref provider.Ref) provider.Pending
		// call is the call that must be refused.
		call func(p *Provider, ref provider.Ref) error
	}{
		{
			name: "stop while starting",
			arrange: func(t *testing.T, p *Provider, ref provider.Ref) provider.Pending {
				p.Stop(ctx, ref)
				p.Complete(ref)
				pend, _ := p.Start(ctx, ref, provider.StartOptions{})
				return pend
			},
			call: func(p *Provider, ref provider.Ref) error { _, err := p.Stop(ctx, ref); return err },
		},
		{
			name: "start while stopping",
			arrange: func(t *testing.T, p *Provider, ref provider.Ref) provider.Pending {
				pend, _ := p.Stop(ctx, ref)
				return pend
			},
			call: func(p *Provider, ref provider.Ref) error {
				_, err := p.Start(ctx, ref, provider.StartOptions{})
				return err
			},
		},
		{
			name: "destroy while stopping",
			arrange: func(t *testing.T, p *Provider, ref provider.Ref) provider.Pending {
				pend, _ := p.Stop(ctx, ref)
				return pend
			},
			call: func(p *Provider, ref provider.Ref) error { _, err := p.Destroy(ctx, ref); return err },
		},
		{
			name: "start while being destroyed",
			arrange: func(t *testing.T, p *Provider, ref provider.Ref) provider.Pending {
				pend, _ := p.Destroy(ctx, ref)
				return pend
			},
			call: func(p *Provider, ref provider.Ref) error {
				_, err := p.Start(ctx, ref, provider.StartOptions{})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(Options{Env: testEnv, Manual: true})
			ref := create(t, p, "aaaaaaaa", "one")
			inFlight := tt.arrange(t, p, ref)
			if err := tt.call(p, ref); !errors.Is(err, provider.ErrBusy) {
				t.Fatalf("call = %v, want ErrBusy", err)
			}
			// The documented recovery: settle, then the first operation
			// has finished.
			settled := make(chan error, 1)
			go func() {
				_, err := p.Wait(ctx, provider.Settle(ref))
				settled <- err
			}()
			select {
			case err := <-settled:
				t.Fatalf("Settle returned %v before the operation finished", err)
			case <-time.After(20 * time.Millisecond):
			}
			p.Complete(ref)
			err := <-settled
			if inFlight.Want == provider.StateAbsent {
				if !errors.Is(err, provider.ErrNotFound) {
					t.Errorf("Settle on a destroyed instance = %v, want ErrNotFound", err)
				}
			} else if err != nil {
				t.Errorf("Settle = %v", err)
			}
		})
	}

	t.Run("destroy while creating", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Destroy(ctx, pend.Ref); !errors.Is(err, provider.ErrBusy) {
			t.Fatalf("Destroy = %v, want ErrBusy", err)
		}
		p.Complete(pend.Ref)
		if _, err := p.Destroy(ctx, pend.Ref); err != nil {
			t.Errorf("Destroy after the create settled = %v", err)
		}
	})
}

func TestInjectedFailures(t *testing.T) {
	ctx := context.Background()
	quota := provider.Errorf(provider.ErrQuota, "create", "CPUS quota")
	capacity := provider.Errorf(provider.ErrCapacity, "wait", "zone is full")

	t.Run("create refused at once leaves nothing", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		p.FailNext(OpCreate, quota)
		if _, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one")); !errors.Is(err, provider.ErrQuota) {
			t.Fatalf("Create = %v, want ErrQuota", err)
		}
		if got, _ := p.List(ctx, provider.Selector{}); len(got) != 0 {
			t.Errorf("instances = %d, want 0", len(got))
		}
		if _, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one")); err != nil {
			t.Errorf("the next Create should succeed: %v", err)
		}
	})

	t.Run("create refused late: Wait returns it and the instance is gone", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		p.FailOperation(OpCreate, capacity)
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatalf("Create = %v, want accepted", err)
		}
		mustState(t, p, pend.Ref, provider.StateStarting)
		p.Complete(pend.Ref)
		if _, err := p.Wait(ctx, pend); !errors.Is(err, provider.ErrCapacity) {
			t.Fatalf("Wait = %v, want ErrCapacity", err)
		}
		if _, err := p.Get(ctx, pend.Ref); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Get = %v, want ErrNotFound", err)
		}
		// Waiting again reports the same failure.
		if _, err := p.Wait(ctx, pend); !errors.Is(err, provider.ErrCapacity) {
			t.Errorf("second Wait = %v, want ErrCapacity", err)
		}
	})

	t.Run("start refused late leaves the instance stopped", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		ref := create(t, p, "aaaaaaaa", "one")
		if _, err := p.Stop(ctx, ref); err != nil {
			t.Fatal(err)
		}
		p.FailOperation(OpStart, capacity)
		pend, err := p.Start(ctx, ref, provider.StartOptions{})
		if err != nil {
			t.Fatalf("Start = %v, want accepted", err)
		}
		_, err = p.Wait(ctx, pend)
		if !errors.Is(err, provider.ErrCapacity) || provider.Reason(err) != provider.ReasonCapacity {
			t.Fatalf("Wait = %v, want ErrCapacity", err)
		}
		mustState(t, p, ref, provider.StateStopped)
	})

	t.Run("failures queue and are consumed in order", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		boom := errors.New("boom")
		p.FailNext(OpList, boom)
		p.FailNext(OpList, quota)
		if _, err := p.List(ctx, provider.Selector{}); !errors.Is(err, boom) {
			t.Errorf("first List = %v", err)
		}
		if _, err := p.List(ctx, provider.Selector{}); !errors.Is(err, provider.ErrQuota) {
			t.Errorf("second List = %v", err)
		}
		if _, err := p.List(ctx, provider.Selector{}); err != nil {
			t.Errorf("third List = %v", err)
		}
	})
}

func TestWait(t *testing.T) {
	ctx := context.Background()

	t.Run("a deadline ends the wait, not the operation", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		if _, err := p.Wait(short, pend); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v, want DeadlineExceeded", err)
		}
		mustState(t, p, pend.Ref, provider.StateStarting)
		p.Complete(pend.Ref)
		if inst, err := p.Wait(ctx, pend); err != nil || inst.State != provider.StateRunning {
			t.Errorf("second Wait = %s, %v", inst.State, err)
		}
	})

	t.Run("a delay is honoured", func(t *testing.T) {
		const delay = 30 * time.Millisecond
		p := New(Options{Env: testEnv, CreateDelay: delay})
		began := time.Now()
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatal(err)
		}
		if pend.Operation == "" {
			t.Errorf("Create returned no operation token")
		}
		inst, err := p.Wait(ctx, pend)
		if err != nil || inst.State != provider.StateRunning {
			t.Fatalf("Wait = %s, %v", inst.State, err)
		}
		if waited := time.Since(began); waited < delay {
			t.Errorf("Wait returned after %v, before the %v delay", waited, delay)
		}
	})

	t.Run("a lost operation token falls back to the instance state", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatal(err)
		}
		resumed := provider.Pending{Ref: pend.Ref, Want: provider.StateRunning}
		done := make(chan error, 1)
		go func() {
			_, err := p.Wait(ctx, resumed)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("Wait returned %v while the create was in flight", err)
		case <-time.After(20 * time.Millisecond):
		}
		p.Complete(pend.Ref)
		if err := <-done; err != nil {
			t.Errorf("Wait = %v", err)
		}
	})

	t.Run("settling in another state is ErrUnexpectedState with the instance", func(t *testing.T) {
		p := New(Options{Env: testEnv})
		ref := create(t, p, "aaaaaaaa", "one")
		if err := p.ExternalStop(ref); err != nil {
			t.Fatal(err)
		}
		inst, err := p.Wait(ctx, provider.Pending{Ref: ref, Want: provider.StateRunning})
		if !errors.Is(err, provider.ErrUnexpectedState) || inst.State != provider.StateStopped {
			t.Errorf("Wait = %s, %v", inst.State, err)
		}
	})

	t.Run("an instance deleted from outside is ErrNotFound", func(t *testing.T) {
		p := New(Options{Env: testEnv, Manual: true})
		pend, err := p.Create(ctx, specFor(t, testEnv, "aaaaaaaa", "one"))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := p.Wait(ctx, pend)
			done <- err
		}()
		time.Sleep(10 * time.Millisecond)
		p.ExternalDelete(pend.Ref)
		if err := <-done; !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Wait = %v, want ErrNotFound", err)
		}
	})
}

func TestEnvironmentGuard(t *testing.T) {
	ctx := context.Background()
	p := New(Options{Env: testEnv})
	foreign := p.Seed(provider.Instance{
		Name:   "shed-prod-zzzzzzzz",
		State:  provider.StateRunning,
		Labels: provider.Identity{Env: "prod", ID: "zzzzzzzz", Name: "theirs"}.Labels(provider.ShedStateReady),
	})

	calls := []struct {
		name string
		call func() error
	}{
		{"start", func() error { _, err := p.Start(ctx, foreign, provider.StartOptions{}); return err }},
		{"stop", func() error { _, err := p.Stop(ctx, foreign); return err }},
		{"destroy", func() error { _, err := p.Destroy(ctx, foreign); return err }},
		{"set labels", func() error {
			return p.SetLabels(ctx, foreign, map[string]string{provider.LabelState: string(provider.ShedStateDeleting)})
		}},
		{"set metadata", func() error {
			return p.SetMetadata(ctx, foreign, map[string]string{provider.MetadataHostKeyPin: "x"})
		}},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, provider.ErrForeign) {
				t.Errorf("err = %v, want ErrForeign", err)
			}
		})
	}
	mustState(t, p, foreign, provider.StateRunning)

	t.Run("create for another environment", func(t *testing.T) {
		if _, err := p.Create(ctx, specFor(t, "prod", "yyyyyyyy", "mine")); !errors.Is(err, provider.ErrInvalid) {
			t.Errorf("Create = %v, want ErrInvalid", err)
		}
	})

	t.Run("list selects by environment and sorts by name", func(t *testing.T) {
		create(t, p, "bbbbbbbb", "two")
		create(t, p, "aaaaaaaa", "one")
		mine, err := p.List(ctx, provider.Selector{Env: testEnv})
		if err != nil {
			t.Fatal(err)
		}
		if len(mine) != 2 || mine[0].Name != "shed-dev-test-aaaaaaaa" || mine[1].Name != "shed-dev-test-bbbbbbbb" {
			t.Errorf("mine = %v", names(mine))
		}
		all, err := p.List(ctx, provider.Selector{})
		if err != nil || len(all) != 3 {
			t.Errorf("all = %v, %v", names(all), err)
		}
	})
}

func TestLabelsAndMetadata(t *testing.T) {
	ctx := context.Background()
	p := New(Options{Env: testEnv})
	ref := create(t, p, "aaaaaaaa", "one")

	t.Run("set labels merges", func(t *testing.T) {
		err := p.SetLabels(ctx, ref, map[string]string{
			provider.LabelState:  string(provider.ShedStateReady),
			provider.LabelLayout: provider.LayoutLabel(1),
		})
		if err != nil {
			t.Fatalf("SetLabels: %v", err)
		}
		inst := mustState(t, p, ref, provider.StateRunning)
		if inst.ShedState() != provider.ShedStateReady || inst.Layout() != 1 {
			t.Errorf("labels = %v", inst.Labels)
		}
		if inst.Labels[provider.LabelName] != "one" || inst.Labels[provider.LabelOwner] != "nocturn" {
			t.Errorf("other labels were lost: %v", inst.Labels)
		}
	})

	immutable := []string{provider.LabelSchema, provider.LabelEnv, provider.LabelID}
	for _, key := range immutable {
		t.Run("cannot change "+key, func(t *testing.T) {
			if err := p.SetLabels(ctx, ref, map[string]string{key: "other"}); !errors.Is(err, provider.ErrInvalid) {
				t.Errorf("SetLabels = %v, want ErrInvalid", err)
			}
		})
	}

	t.Run("set metadata merges and an empty value removes", func(t *testing.T) {
		const pin = "ssh-ed25519 AAAAhostkey"
		if err := p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: pin}); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		inst := mustState(t, p, ref, provider.StateRunning)
		if inst.HostKeyPin() != pin || inst.Metadata[provider.MetadataShedName] != "one" {
			t.Errorf("metadata = %v", inst.Metadata)
		}
		if err := p.SetMetadata(ctx, ref, map[string]string{provider.MetadataHostKeyPin: ""}); err != nil {
			t.Fatal(err)
		}
		if got := mustState(t, p, ref, provider.StateRunning); got.HostKeyPin() != "" {
			t.Errorf("pin still %q", got.HostKeyPin())
		}
	})

	t.Run("returned instances are copies", func(t *testing.T) {
		inst := mustState(t, p, ref, provider.StateRunning)
		inst.Labels[provider.LabelName] = "changed"
		if got := mustState(t, p, ref, provider.StateRunning); got.Labels[provider.LabelName] != "one" {
			t.Errorf("a caller changed the provider's labels")
		}
	})

	t.Run("missing instance", func(t *testing.T) {
		missing := provider.Ref("fake/none")
		if err := p.SetLabels(ctx, missing, map[string]string{"a": "b"}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("SetLabels = %v", err)
		}
		if _, err := p.Start(ctx, missing, provider.StartOptions{}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Start = %v", err)
		}
		if _, err := p.Stop(ctx, missing); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("Stop = %v", err)
		}
	})
}

func TestHostKey(t *testing.T) {
	ctx := context.Background()
	p := New(Options{Env: testEnv})
	ref := create(t, p, "aaaaaaaa", "one")
	if _, err := p.GuestHostKey(ctx, ref); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("before publishing: %v, want ErrNotFound", err)
	}
	p.PublishHostKey(ref, "ssh-ed25519 AAAAhostkey")
	if got, err := p.GuestHostKey(ctx, ref); err != nil || got != "ssh-ed25519 AAAAhostkey" {
		t.Errorf("GuestHostKey = %q, %v", got, err)
	}
}

func TestResolveImage(t *testing.T) {
	ctx := context.Background()
	p := New(Options{Env: testEnv})
	tests := []struct {
		in, want string
		wantErr  error
	}{
		{"shed-extensions-v1-amd64", "shed-extensions-v1-amd64", nil},
		{"family/shed-extensions-amd64", "shed-extensions-amd64-fake0001", nil},
		{"", "", provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run("image "+tt.in, func(t *testing.T) {
			got, err := p.ResolveImage(ctx, tt.in)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Errorf("ResolveImage(%q) = %q, %v; want %q, %v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
	t.Run("a missing image can be injected", func(t *testing.T) {
		p.FailNext(OpResolveImage, provider.Errorf(provider.ErrNotFound, "resolve_image", "no such image"))
		if _, err := p.ResolveImage(ctx, "gone"); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("ResolveImage = %v, want ErrNotFound", err)
		}
	})
}

func TestExternalStopAndCalls(t *testing.T) {
	ctx := context.Background()
	p := New(Options{Env: testEnv})
	ref := create(t, p, "aaaaaaaa", "one")
	if err := p.ExternalStop(ref); err != nil {
		t.Fatal(err)
	}
	inst := mustState(t, p, ref, provider.StateStopped)
	if inst.StoppedAt.IsZero() {
		t.Errorf("StoppedAt not set")
	}
	if p.Count(OpStop) != 0 {
		t.Errorf("an external stop must not be recorded as a call")
	}
	if p.Count(OpCreate) != 1 || p.Count(OpWait) != 1 || p.Count(OpGet) != 1 {
		t.Errorf("calls = %v", p.Calls())
	}
	if first := p.Calls()[0]; first.Op != OpCreate || first.Ref != ref {
		t.Errorf("first call = %+v", first)
	}

	t.Run("a cancelled context is refused and recorded", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.Start(cancelled, ref, provider.StartOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("Start = %v, want Canceled", err)
		}
		mustState(t, p, ref, provider.StateStopped)
	})
}

// TestConcurrentUse is for the race detector: many goroutines drive separate
// instances through a full life while others list.
func TestConcurrentUse(t *testing.T) {
	ctx := context.Background()
	p := New(Options{Env: testEnv, CreateDelay: time.Millisecond, StopDelay: time.Millisecond, DestroyDelay: time.Millisecond})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := specFor(t, testEnv, fmt.Sprintf("id%06d", i), fmt.Sprintf("shed-%d", i))
			pend, err := p.Create(ctx, spec)
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			if _, err := p.Wait(ctx, pend); err != nil {
				t.Errorf("Wait: %v", err)
				return
			}
			if _, err := p.List(ctx, provider.Selector{Env: testEnv}); err != nil {
				t.Errorf("List: %v", err)
			}
			stop, err := p.Stop(ctx, pend.Ref)
			if err != nil {
				t.Errorf("Stop: %v", err)
				return
			}
			if _, err := p.Wait(ctx, stop); err != nil {
				t.Errorf("Wait stop: %v", err)
			}
			destroy, err := p.Destroy(ctx, pend.Ref)
			if err != nil {
				t.Errorf("Destroy: %v", err)
				return
			}
			if _, err := p.Wait(ctx, destroy); err != nil {
				t.Errorf("Wait destroy: %v", err)
			}
		}()
	}
	wg.Wait()
	if got, _ := p.List(ctx, provider.Selector{}); len(got) != 0 {
		t.Errorf("instances left = %v", names(got))
	}
}

func names(insts []provider.Instance) []string {
	out := make([]string, len(insts))
	for i, in := range insts {
		out[i] = in.Name
	}
	return out
}

// A rule handed to the fake, and a rule read from it, share nothing with the
// fake's own copy: not the lists, and not the port lists inside each allow
// and deny entry. A test that edits its fixture after storing it, or a rule
// it read, must not change what the next read returns.
func TestFirewallRulesAreDeepCopies(t *testing.T) {
	ctx := context.Background()
	name := provider.FirewallRuleName("test")
	rule := func() provider.FirewallRule {
		return provider.FirewallRule{
			Name:         name,
			SourceRanges: []string{DefaultFirewallSource},
			TargetTags:   []string{provider.NetworkTag("test")},
			Allowed:      []provider.FirewallPorts{{Protocol: "tcp", Ports: []string{"22"}}},
			Denied:       []provider.FirewallPorts{{Protocol: "udp", Ports: []string{"53"}}},
		}
	}
	ports := func(r provider.FirewallRule) string {
		return r.Allowed[0].Ports[0] + " " + r.Denied[0].Ports[0] + " " + r.SourceRanges[0] + " " + r.TargetTags[0]
	}
	want := ports(rule())
	spoil := func(r provider.FirewallRule) {
		r.Allowed[0].Ports[0] = "0-65535"
		r.Denied[0].Ports[0] = "0"
		r.SourceRanges[0] = "0.0.0.0/0"
		r.TargetTags[0] = "other"
	}
	tests := []struct {
		name  string
		store func(p *Provider, r provider.FirewallRule)
		read  func(p *Provider) provider.FirewallRule
	}{
		{"SetFirewallRules, then the input is edited",
			func(p *Provider, r provider.FirewallRule) { p.SetFirewallRules(r); spoil(r) },
			func(p *Provider) provider.FirewallRule { r, _ := p.EnvFirewallRule(ctx); return r }},
		{"AddFirewallRule, then the input is edited",
			func(p *Provider, r provider.FirewallRule) { p.SetFirewallRules(); p.AddFirewallRule(r); spoil(r) },
			func(p *Provider) provider.FirewallRule { r, _ := p.EnvFirewallRule(ctx); return r }},
		{"a rule read with EnvFirewallRule is edited",
			func(p *Provider, r provider.FirewallRule) {
				p.SetFirewallRules(r)
				got, _ := p.EnvFirewallRule(ctx)
				spoil(got)
			},
			func(p *Provider) provider.FirewallRule { r, _ := p.EnvFirewallRule(ctx); return r }},
		{"a rule read with NetworkIngress is edited",
			func(p *Provider, r provider.FirewallRule) {
				p.SetFirewallRules(r)
				view, _ := p.NetworkIngress(ctx)
				spoil(view.Rules[0])
			},
			func(p *Provider) provider.FirewallRule { view, _ := p.NetworkIngress(ctx); return view.Rules[0] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(Options{Env: "test"})
			tt.store(p, rule())
			if got := ports(tt.read(p)); got != want {
				t.Errorf("the fake's rule reads %q after the edit, want %q", got, want)
			}
		})
	}
}
