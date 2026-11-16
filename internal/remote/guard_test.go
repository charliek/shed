package remote

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/vmutil"
)

func asUser(t *testing.T, p backend.Principal) context.Context {
	t.Helper()
	return backend.WithPrincipal(ctxT(t), p)
}

// guardedDelete deletes through the access guard and waits for the VM, the
// way a streamed delete does: one call, authorized once, at the delete.
func guardedDelete(t *testing.T, e *env, ctx context.Context, name string) error {
	t.Helper()
	al, ok := backend.IsAsync(backend.NewGuard(e.b))
	if !ok {
		t.Fatal("the guard hides the remote backend's async lifecycle")
	}
	d, ok := al.(backend.AsyncDeleter)
	if !ok {
		t.Fatal("the guard's async lifecycle has no delete-and-wait")
	}
	return d.DeleteAndWait(ctx, ctx, name)
}

// TestGuardedDeleteFollowsTheTombstone: a delete through the guard is
// authorized at the delete and then follows that incarnation through its
// delete marker after the record is gone. A destroy that failed is reported
// (ErrDeleteOwed), never as a finished delete; one that finished is success;
// another user can neither delete the shed nor learn anything.
func TestGuardedDeleteFollowsTheTombstone(t *testing.T) {
	alice := backend.UserPrincipal("alice", false)
	bob := backend.UserPrincipal("bob", false)

	t.Run("failed destroy is reported", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("box", "alice")
		e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
		if err := guardedDelete(t, e, asUser(t, alice), "box"); !errors.Is(err, ErrDeleteOwed) {
			t.Fatalf("delete with a failed destroy = %v, want ErrDeleteOwed", err)
		}
		e.idle()
		if e.instances() != 1 {
			t.Fatal("the instance should still exist")
		}
	})

	t.Run("finished destroy is success", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("box", "alice")
		if err := guardedDelete(t, e, asUser(t, alice), "box"); err != nil {
			t.Fatalf("delete = %v", err)
		}
		e.idle()
		if e.instances() != 0 {
			t.Fatal("the instance was not destroyed")
		}
	})

	t.Run("another user", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("box", "alice")
		if err := guardedDelete(t, e, asUser(t, bob), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Fatalf("bob's delete of alice's shed = %v, want not found", err)
		}
		if got := e.userOf("box"); got != "alice" {
			t.Fatalf("alice's shed after bob's delete: user %q", got)
		}
		if e.instances() != 1 {
			t.Fatal("bob's refused delete destroyed the instance")
		}
	})
}

// TestDeleteMarkerCarriesTheReportedOwner: the delete marker binds a wait to
// the owner the shed was reported with, not the raw stored one. A record that
// names both a service client and a user is reported with no owner (and an
// unresolved user, admin-only), so that client cannot follow its delete
// through the marker either: to it the name is simply not there.
func TestDeleteMarkerCarriesTheReportedOwner(t *testing.T) {
	e := newEnv(t)
	e.createFor("odd", "alice")
	e.b.mu.Lock()
	e.b.records["odd"].Shed.Owner = "nocturn" // a record that names both
	e.b.mu.Unlock()

	e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
	admin := asUser(t, backend.UserPrincipal("root", true))
	if err := guardedDelete(t, e, admin, "odd"); !errors.Is(err, ErrDeleteOwed) {
		t.Fatalf("admin's delete with a failed destroy = %v, want ErrDeleteOwed", err)
	}
	client := backend.WithRequiredOwner(ctxT(t), "nocturn")
	if _, err := e.b.WaitSettled(client, "odd"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("the raw owner's wait on the deleted shed = %v, want not found", err)
	}
	if _, err := e.b.WaitSettled(ctxT(t), "odd"); !errors.Is(err, ErrDeleteOwed) {
		t.Fatalf("an unbound wait = %v, want ErrDeleteOwed", err)
	}
	e.idle()
}

// TestGuardedOperationsOnRemote: the guard decides on the remote backend as
// on the local ones, from the shed's reported user.
func TestGuardedOperationsOnRemote(t *testing.T) {
	e := newEnv(t)
	e.createFor("box", "alice")
	g := backend.NewGuard(e.b)
	bob := asUser(t, backend.UserPrincipal("bob", false))
	root := asUser(t, backend.UserPrincipal("root", true))
	alice := asUser(t, backend.UserPrincipal("alice", false))

	if _, err := g.GetShed(bob, "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("bob GetShed: %v, want not found", err)
	}
	if list, err := g.ListSheds(bob); err != nil || len(list) != 0 {
		t.Errorf("bob lists %v (%v)", list, err)
	}
	if err := g.Exec(root, "box", backend.ExecOptions{Cmd: []string{"true"}}); !errors.Is(err, config.ErrForbiddenSentinel) {
		t.Errorf("admin exec: %v, want forbidden", err)
	}
	if _, err := g.GetShed(alice, "box"); err != nil {
		t.Errorf("alice GetShed: %v", err)
	}
	if _, err := g.StopShed(root, "box"); err != nil {
		t.Errorf("admin stop: %v", err)
	}
	e.idle()
}

// TestGuardedDeleteFollowsTheTombstoneUnderAReplacement: the name is free as
// soon as the delete is acknowledged, so another shed can take it before the
// wait looks. The wait is bound to the deleted incarnation and follows its
// tombstone whatever now holds the name: a destroy that failed is still
// reported (ErrDeleteOwed), never as success; one that finished is success;
// and the newcomer is untouched either way.
func TestGuardedDeleteFollowsTheTombstoneUnderAReplacement(t *testing.T) {
	alice := backend.UserPrincipal("alice", false)
	for _, tc := range []struct {
		name    string
		fail    bool
		wantErr error
		wantVMs int
	}{
		{"failed destroy is reported", true, ErrDeleteOwed, 2},
		{"finished destroy is success", false, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.createFor("box", "alice")
			if tc.fail {
				e.prov.FailNext(fake.OpDestroy, errors.New("the cloud is having a moment"))
			}
			g := backend.NewGuard(e.b)
			replaced := false
			g.SetBeforeAct(func(op, name string) {
				if op == "DeleteAndWait.wait" && name == "box" && !replaced {
					replaced = true
					e.createFor("box", "bob")
				}
			})
			al, _ := backend.IsAsync(g)
			err := al.(backend.AsyncDeleter).DeleteAndWait(asUser(t, alice), ctxT(t), "box")
			if !replaced {
				t.Fatal("the guard never reached its hook between the delete and the wait")
			}
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("delete-and-wait with a replacement holding the name = %v, want %v", err, tc.wantErr)
			}
			e.idle()
			if got := e.userOf("box"); got != "bob" {
				t.Errorf("the replacement after the delete: user %q, want bob", got)
			}
			if n := e.instances(); n != tc.wantVMs {
				t.Errorf("%d instances, want %d", n, tc.wantVMs)
			}
		})
	}
}

// cloneRecorder records the owned (withhold the server's env_file variables)
// argument of every repo clone.
type cloneRecorder struct {
	mu    sync.Mutex
	owned []bool
}

func (r *cloneRecorder) clone(_ context.Context, _ *vmutil.AgentClient, _ *config.ServerConfig, _ string, owned bool, _ []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owned = append(r.owned, owned)
	return nil
}

func (r *cloneRecorder) calls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.owned...)
}

// TestCloneWithholdsServerEnvForAUserWhoIsNotAnAdmin: a shed created for a
// user who is not an admin is cloned without the server's env_file variables
// (owned=true to the clone), as on Firecracker and VZ. The flag is kept on the
// record, so a create resumed after a restart withholds them too; an admin's
// shed is cloned with them, as before.
func TestCloneWithholdsServerEnvForAUserWhoIsNotAnAdmin(t *testing.T) {
	const repo = "https://git.example.com/acme/widgets.git"
	alice := backend.UserPrincipal("alice", false)
	root := backend.UserPrincipal("root", true)
	create := func(t *testing.T, e *env, p backend.Principal, name string) {
		t.Helper()
		req := config.CreateShedRequest{Name: name, Repo: repo, NoProvision: true, ResolvedMounts: config.SelectedMounts(nil)}
		if _, err := backend.NewGuard(e.b).CreateShed(asUser(t, p), req); err != nil {
			t.Fatalf("create %s as %s: %v", name, p, err)
		}
	}

	t.Run("create", func(t *testing.T) {
		for _, tc := range []struct {
			p    backend.Principal
			want bool
		}{{alice, true}, {root, false}} {
			e := newEnv(t)
			rec := &cloneRecorder{}
			e.b.clone = rec.clone
			create(t, e, tc.p, "box")
			if got := e.settled("box").Status; got != config.StatusRunning {
				t.Fatalf("settled %q", got)
			}
			if got := rec.calls(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("clone for %s: owned %v, want [%v]", tc.p, got, tc.want)
			}
			if got := e.storedRecord("box").NoServerEnv; got != tc.want {
				t.Errorf("stored record for %s: NoServerEnv %v, want %v", tc.p, got, tc.want)
			}
		}
	})

	t.Run("resumed after a restart", func(t *testing.T) {
		e := newEnv(t, manual())
		create(t, e, alice, "box")
		id := e.settledID("box")
		deadline := time.Now().Add(testTimeout)
		for e.b.snapshot("box", id).Ref == "" {
			if time.Now().After(deadline) {
				t.Fatal("the create never recorded its instance")
			}
			time.Sleep(2 * time.Millisecond)
		}
		rec := &cloneRecorder{}
		e.clone = rec.clone
		e.restart()
		e.auto()
		if got := e.settled("box"); got.Status != config.StatusRunning || got.ID != id {
			t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
		}
		if got := rec.calls(); len(got) != 1 || !got[0] {
			t.Errorf("resumed clone: owned %v, want [true]", got)
		}
	})
}

// TestGuardedOperationRefusedWhenTheUserChangesBeforeTheAction: the guard
// authorizes alice on her shed; before the backend acts, a reconcile finds the
// shed's user label in conflict and reports it unresolved (admin-only). The
// backend's check under its lock compares the user the guard authorized
// against, so alice's stop, start, exec and delete on it
// are answered as a shed that is not there, and nothing is done to it. The
// control (no change in between) acts.
func TestGuardedOperationRefusedWhenTheUserChangesBeforeTheAction(t *testing.T) {
	alice := backend.UserPrincipal("alice", false)
	ops := map[string]func(g *backend.Guard, ctx context.Context) error{
		"StopShed": func(g *backend.Guard, ctx context.Context) error {
			_, err := g.StopShed(ctx, "box")
			return err
		},
		"StartShed": func(g *backend.Guard, ctx context.Context) error {
			_, err := g.StartShed(ctx, "box")
			return err
		},
		"DeleteShed": func(g *backend.Guard, ctx context.Context) error { return g.DeleteShed(ctx, "box") },
		"Exec": func(g *backend.Guard, ctx context.Context) error {
			return g.Exec(ctx, "box", backend.ExecOptions{Cmd: []string{"true"}})
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.createFor("box", "alice")
			g := backend.NewGuard(e.b)
			g.SetBeforeAct(func(_, shed string) {
				if shed != "box" {
					return
				}
				e.b.mu.Lock()
				e.b.records["box"].UserConflict = "mallory"
				e.b.mu.Unlock()
			})
			err := op(g, asUser(t, alice))
			if !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Fatalf("alice's %s after her shed became unresolved: %v, want not found", name, err)
			}
			e.idle()
			if e.instances() != 1 || e.status("box") != config.StatusRunning {
				t.Errorf("alice's refused %s acted on the shed: %d instances, status %q", name, e.instances(), e.status("box"))
			}
		})
	}

	t.Run("control: no change in between", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("box", "alice")
		if _, err := backend.NewGuard(e.b).StopShed(asUser(t, alice), "box"); err != nil {
			t.Fatalf("alice's stop: %v", err)
		}
	})
}
