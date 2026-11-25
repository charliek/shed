package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
)

// `shed provision` needs more than the right to enter: only the shed's own
// user (or the implicit user, for a shed with no user) may cause a delivery
// to it, because it asks that person's source. Everyone else, including an
// admin on a service client's shed (which an admin may enter) and a script
// key acting as the shed's own user, is refused with 403 and reaches nothing.
func TestProvisionIsForTheShedsOwnUser(t *testing.T) {
	script := alice
	script.Script = true
	cases := []struct {
		name string
		shed config.Shed
		p    Principal
		ok   bool
		// want is the refusal's sentinel when !ok.
		want error
	}{
		{"the shed's user", config.Shed{Name: "box", User: "alice"}, alice, true, nil},
		{"an admin on their own shed", config.Shed{Name: "box", User: "root"}, root, true, nil},
		{"the implicit user on a shed with no user", config.Shed{Name: "box"}, ImplicitPrincipal(), true, nil},
		{"another user", config.Shed{Name: "box", User: "alice"}, bob, false, config.ErrShedNotFoundSentinel},
		{"an admin on another user's shed", config.Shed{Name: "box", User: "alice"}, root, false, config.ErrForbiddenSentinel},
		{"an admin on a service client's shed", config.Shed{Name: "box", Owner: "nocturn"}, root, false, config.ErrForbiddenSentinel},
		{"an admin on an unassigned shed", config.Shed{Name: "box"}, root, false, config.ErrForbiddenSentinel},
		{"the service client on its own shed", config.Shed{Name: "box", Owner: "nocturn"}, nocturn, false, config.ErrForbiddenSentinel},
		{"a script key of the shed's user", config.Shed{Name: "box", User: "alice"}, script, false, config.ErrForbiddenSentinel},
		{"the implicit user on a service client's shed", config.Shed{Name: "box", Owner: "nocturn"}, ImplicitPrincipal(), false, config.ErrForbiddenSentinel},
		{"the server itself", config.Shed{Name: "box", User: "alice"}, SystemPrincipal(), false, config.ErrForbiddenSentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newRecBackend()
			sh := tc.shed
			sh.CreatedAt, sh.Status = t0, config.StatusRunning
			b.put(sh)
			pv, ok := AsProvisioner(NewGuard(b))
			if !ok {
				t.Fatal("the guard hides the provision capability")
			}
			_, err := Provision(WithPrincipal(context.Background(), tc.p), pv, "box")
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := b.acted(); len(got) != 1 || got[0] != "ProvisionShed" {
					t.Fatalf("reached %v, want ProvisionShed", got)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := b.acted(); len(got) != 0 {
				t.Fatalf("a refused provision reached the backend: %v", got)
			}
		})
	}
}

// The shed's delivery record is the shed's user's: an admin looking at
// another user's shed gets its state alone, as the repository and landing
// directory are withheld; the user sees all of it.
func TestShownWithWithholdsTheDeliveryRecord(t *testing.T) {
	rec := &delivery.Record{
		Steps: &delivery.Steps{
			Stage1: delivery.Step{State: delivery.StepDone}, Clone: delivery.Step{State: delivery.StepDone},
			Stage2: delivery.Step{State: delivery.StepOwed}, Install: delivery.Step{State: delivery.StepOwed},
		},
		Last: &delivery.Last{At: "2026-11-23T10:00:00Z", Reason: delivery.ReasonCreate, Outcome: delivery.OutcomeAccepted,
			Source:   &delivery.Source{Kind: delivery.SourceKindMachine, MachineID: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b", MachineName: "ada-laptop"},
			Problems: []delivery.Problem{{Provider: "github", Code: "signed_out", Message: "sign in"}}},
		Items: []delivery.RecordItem{{Name: "ssh-key", Provider: "ssh-key", Stage: 1, Kind: delivery.KindFile, Base: delivery.BaseHome,
			Target: ".ssh/id_ed25519", Fingerprint: "fp", WrittenAt: "2026-11-23T10:00:01Z", Result: delivery.ResultOK}},
	}
	rec.State = delivery.DeriveState(rec)
	b := newRecBackend()
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0, Status: config.StatusRunning,
		Repo: "https://git.example.test/example/box.git", Delivery: config.NewShedDelivery(rec, t0)})
	g := NewGuard(b)

	own, err := g.GetShed(as(alice), "box")
	if err != nil {
		t.Fatal(err)
	}
	if own.Delivery == nil || len(own.Delivery.Items) != 1 || own.Delivery.Last == nil || len(own.Delivery.Owed) != 2 {
		t.Fatalf("the user's own view = %+v, want the whole record", own.Delivery)
	}
	if own.Delivery.Withheld {
		t.Fatal("the user's own view is marked withheld")
	}

	other, err := g.GetShed(as(root), "box")
	if err != nil {
		t.Fatal(err)
	}
	d := other.Delivery
	if d == nil || d.State != rec.State {
		t.Fatalf("an admin's view = %+v, want the state %s", d, rec.State)
	}
	if d.Steps != nil || d.Last != nil || d.Items != nil || d.Owed != nil || d.Expired != nil {
		t.Fatalf("an admin's view of another user's shed carries more than its state: %+v", d)
	}
	// It says so, whatever the state: a state-only record cannot tell an
	// admin's view of a "none" or "not_applicable" shed from the user's own.
	if !d.Withheld {
		t.Fatalf("an admin's view is not marked withheld: %+v", d)
	}
	raw, err := json.Marshal(other.Delivery)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"state":"`+string(rec.State)+`","withheld":true}` {
		t.Fatalf("an admin's view on the wire = %s", raw)
	}
	ownRaw, _ := json.Marshal(own.Delivery)
	if strings.Contains(string(ownRaw), "withheld") {
		t.Fatalf("the user's own view on the wire carries withheld: %s", ownRaw)
	}
	if other.Repo != "" {
		t.Fatalf("an admin's view carries the repo %q", other.Repo)
	}
	// The backend's own record is not changed by the report step.
	again, _ := g.GetShed(as(alice), "box")
	if len(again.Delivery.Items) != 1 {
		t.Fatal("the report step changed the backend's record")
	}
}

// replacingRun is a provision run that, between its last step and its final
// lookup, has the shed deleted and created again under its name (another
// user's, created later), then looks it up as a backend does: with the
// identity checks its context requires.
type replacingRun struct{ b *recBackend }

func (r replacingRun) Run(ctx context.Context) (*config.Shed, error) {
	r.b.put(config.Shed{Name: "box", User: "bob", CreatedAt: t0.Add(time.Hour), Status: config.StatusRunning})
	s, err := r.b.checked(ctx, "box")
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (replacingRun) Release() {}

type replacingBackend struct{ *recBackend }

func (b replacingBackend) BeginProvision(ctx context.Context, name string) (ProvisionRun, error) {
	if _, err := b.checked(ctx, name); err != nil {
		return nil, err
	}
	return replacingRun{b: b.recBackend}, nil
}

// The run of an admitted provision is bound to the shed admission
// authorized: a replacement shed under the name is never returned.
func TestProvisionRunKeepsTheAdmissionsPins(t *testing.T) {
	b := newRecBackend()
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0, Status: config.StatusRunning})
	pv, _ := AsProvisioner(NewGuard(replacingBackend{b}))
	run, err := pv.BeginProvision(as(alice), "box")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Release()
	// Run gets the caller's own context (as the API's detached one), with
	// no pins of its own.
	shed, err := run.Run(as(alice))
	if !errors.Is(err, config.ErrShedNotFoundSentinel) || shed != nil {
		t.Fatalf("Run = %+v, %v: want the replacement refused as not found", shed, err)
	}
}
