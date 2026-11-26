package backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// The access rule for a cloud shed's lease (plan 041 §3.4): extend is the
// shed's own user's or an admin's (a local admin included), never a service
// client's or a script key's; usage is counted for whom AuthorizeUsage says.

func (b *recBackend) ExtendShed(ctx context.Context, name string, hours int, requestID string) (*config.Shed, error) {
	sh, err := b.checked(ctx, name)
	if err != nil {
		return nil, err
	}
	b.record(ctx, "ExtendShed")
	b.mu.Lock()
	b.extendIDs = append(b.extendIDs, requestID)
	b.mu.Unlock()
	return &sh, nil
}

// ServicePerson: alice and root are the account service's; anyone else is
// a local admin.
func (b *recBackend) ServicePerson(name string) bool {
	return name == "alice" || name == "root" || name == "bob"
}

func (b *recBackend) CloudUsage(ctx context.Context, user string) (config.CloudUsage, error) {
	b.record(ctx, "CloudUsage:"+user)
	return config.CloudUsage{Org: config.CloudCounts{Machines: 1, Max: 10}}, nil
}

func TestGuardExtendRule(t *testing.T) {
	script := UserPrincipal("alice", false)
	script.Script = true
	adminScript := UserPrincipal("root", true)
	adminScript.Script = true
	for _, tc := range []struct {
		name  string
		p     Principal
		shed  config.Shed
		allow bool
		// notFound: the caller may not even see the shed.
		notFound bool
	}{
		{"own user", alice, config.Shed{Name: "box", User: "alice", CreatedAt: t0}, true, false},
		{"admin on another's", root, config.Shed{Name: "box", User: "alice", CreatedAt: t0}, true, false},
		{"admin on a client's", root, config.Shed{Name: "box", Owner: "nocturn", CreatedAt: t0}, true, false},
		{"implicit user", ImplicitPrincipal(), config.Shed{Name: "box", User: "alice", CreatedAt: t0}, true, false},
		{"another user", bob, config.Shed{Name: "box", User: "alice", CreatedAt: t0}, false, true},
		{"a client on its own", nocturn, config.Shed{Name: "box", Owner: "nocturn", CreatedAt: t0}, false, false},
		{"a client on another's", nocturn, config.Shed{Name: "box", User: "alice", CreatedAt: t0}, false, false},
		{"a script key on its user's", script, config.Shed{Name: "box", User: "alice", CreatedAt: t0}, false, false},
		{"an admin's script key", adminScript, config.Shed{Name: "box", User: "alice", CreatedAt: t0}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newRecBackend()
			b.put(tc.shed)
			ex, ok := AsExtender(NewGuard(b))
			if !ok {
				t.Fatal("the guard hides the extend capability")
			}
			_, err := ex.ExtendShed(as(tc.p), "box", 6, "")
			switch {
			case tc.allow && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.allow:
				if got := b.acted(); len(got) != 1 || got[0] != "ExtendShed" {
					t.Fatalf("reached %v", got)
				}
			case tc.notFound && !errors.Is(err, config.ErrShedNotFoundSentinel):
				t.Fatalf("err %v, want not found", err)
			case !tc.notFound && !errors.Is(err, config.ErrForbiddenSentinel):
				t.Fatalf("err %v, want forbidden", err)
			}
			if !tc.allow && len(b.acted()) != 0 {
				t.Fatalf("a refused extend reached the backend: %v", b.acted())
			}
		})
	}
}

// A backend without leases (Firecracker, VZ) offers no extend through the
// guard: the API answers 501.
func TestGuardExtendOnlyWhereOffered(t *testing.T) {
	type plain struct{ Backend }
	if _, ok := AsExtender(NewGuard(plain{})); ok {
		t.Fatal("a backend without ExtendShed offers it through the guard")
	}
	if _, ok := AsCloudUsager(NewGuard(plain{})); ok {
		t.Fatal("a backend without CloudUsage offers it through the guard")
	}
}

func TestGuardUsageRule(t *testing.T) {
	script := UserPrincipal("alice", false)
	script.Script = true
	ops := UserPrincipal("ops", true) // a local admin
	for _, tc := range []struct {
		name   string
		p      Principal
		asked  string
		want   string
		denied bool
	}{
		{"a person counts their own", alice, "", "alice", false},
		{"a person names themselves", alice, "alice", "alice", false},
		{"a person names another", alice, "bob", "", true},
		{"an admin of the service names anyone", root, "alice", "alice", false},
		{"an admin alone", root, "", "root", false},
		{"a local admin alone", ops, "", "ops", false},
		{"a local admin names another", ops, "alice", "", true},
		{"a local admin names themselves", ops, "ops", "", true},
		{"the implicit user alone", ImplicitPrincipal(), "", "", false},
		{"the implicit user names one", ImplicitPrincipal(), "alice", "", true},
		{"a client: the organization", nocturn, "", "", false},
		{"a client names one", nocturn, "alice", "", true},
		{"a script key reads its user's", script, "", "alice", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newRecBackend()
			cu, ok := AsCloudUsager(NewGuard(b))
			if !ok {
				t.Fatal("the guard hides the usage capability")
			}
			_, err := cu.CloudUsage(as(tc.p), tc.asked)
			if tc.denied {
				if !errors.Is(err, config.ErrForbiddenSentinel) || len(b.acted()) != 0 {
					t.Fatalf("err %v, reached %v; want forbidden and nothing reached", err, b.acted())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := b.acted(); len(got) != 1 || got[0] != "CloudUsage:"+tc.want {
				t.Fatalf("reached %v, want CloudUsage:%s", got, tc.want)
			}
		})
	}
	cu, _ := AsCloudUsager(NewGuard(newRecBackend()))
	if _, err := cu.CloudUsage(context.Background(), ""); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("no principal: %v", err)
	}
}

// The request id an extend sends is derived from the caller's own id, the
// shed and the caller: the same three give the same id, any other gives
// another, and no caller id gives none (the backend makes a new one).
func TestGuardExtendRequestID(t *testing.T) {
	b := newRecBackend()
	b.put(config.Shed{Name: "box", ID: "shed-one", User: "alice", CreatedAt: t0})
	ex, _ := AsExtender(NewGuard(b))
	const id = "6F9619FF-8B86-4D01-B42D-00CF4FC964FF"
	for _, call := range []struct {
		p  Principal
		id string
	}{{alice, id}, {alice, strings.ToLower(id)}, {root, id}, {alice, ""}} {
		if _, err := ex.ExtendShed(as(call.p), "box", 6, call.id); err != nil {
			t.Fatal(err)
		}
	}
	b.put(config.Shed{Name: "box", ID: "shed-two", User: "alice", CreatedAt: t0})
	if _, err := ex.ExtendShed(as(alice), "box", 6, id); err != nil {
		t.Fatal(err)
	}
	ids := b.extendIDs
	if !config.ValidUUID(ids[0]) || ids[0] != ids[1] || strings.EqualFold(ids[0], id) {
		t.Fatalf("alice's retry: %v", ids)
	}
	if ids[2] == ids[0] || ids[3] != "" || ids[4] == ids[0] {
		t.Fatalf("another caller, no id, another shed: %v", ids)
	}
}
