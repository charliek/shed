package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/config"
)

// guardLog attaches a fresh audit log to g and returns a reader of its
// lines.
func guardLog(t *testing.T, g *Guard) func() []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	g.SetAuditLog(l)
	return func() []map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}
}

// TestGuardAsyncDeleteEvent: a delete that waits for its VM writes one
// event, failed when the destroy failed; a refused one writes none.
func TestGuardAsyncDeleteEvent(t *testing.T) {
	for _, fails := range []bool{true, false} {
		rb, _ := fixture()
		b := &recAsync{recBackend: rb, destroyFails: fails, tombstones: map[string]time.Time{}}
		g := NewGuard(b)
		lines := guardLog(t, g)
		al, _ := IsAsync(g)
		_ = al.(AsyncDeleter).DeleteAndWait(as(bob), as(bob), "box")
		if got := lines(); len(got) != 0 {
			t.Fatalf("bob's refused delete was recorded: %v", got)
		}
		_ = al.(AsyncDeleter).DeleteAndWait(as(alice), as(alice), "box")
		got := lines()
		want := audit.ResultOK
		if fails {
			want = audit.ResultFailed
		}
		if len(got) != 1 || got[0]["event"] != audit.EventShedDelete || got[0]["result"] != want ||
			got[0]["shed"] != "box" || got[0]["shed_user"] != "alice" {
			t.Errorf("destroy fails=%v: lines %v", fails, got)
		}
	}
}

// TestGuardEventActor: the actor is the one the front door put on the
// context when it names the same principal (with its key and serial), and the
// principal itself otherwise; the backend sees that actor on its context, so
// a delivery's event names the same caller.
func TestGuardEventActor(t *testing.T) {
	b, g := fixture()
	lines := guardLog(t, g)
	fromDoor := audit.UserIdentity("alice", false, false, false, "SHA256:k", "1f")
	if _, err := g.StopShed(audit.WithActor(as(alice), fromDoor), "box"); err != nil {
		t.Fatal(err)
	}
	// The server acting for itself on a request's context names itself.
	if _, err := g.StartShed(audit.WithActor(as(SystemPrincipal()), fromDoor), "box"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateShed(as(nocturn), config.CreateShedRequest{Name: "nocturn-x", ResolvedMounts: config.SelectedMounts(nil)}); err != nil {
		t.Fatal(err)
	}
	got := lines()
	if len(got) != 3 {
		t.Fatalf("lines = %v", got)
	}
	id := func(i int) map[string]any { m, _ := got[i]["identity"].(map[string]any); return m }
	if id(0)["subject"] != "SHA256:k" || id(0)["serial"] != "1f" || id(0)["user"] != "alice" {
		t.Errorf("stop's actor = %v, want the door's", id(0))
	}
	if id(1)["kind"] != audit.KindSystem || len(id(1)) != 1 {
		t.Errorf("a system call's actor = %v, want {kind: system}", id(1))
	}
	if id(2)["kind"] != audit.KindClient || id(2)["name"] != "nocturn" || got[2]["shed_owner"] != "nocturn" || got[2]["event"] != audit.EventShedCreate {
		t.Errorf("the client's create = %v", got[2])
	}
	if a, ok := audit.ActorFrom(b.ctxs["CreateShed"]); !ok || a.Kind != audit.KindClient || a.Name != "nocturn" {
		t.Errorf("the backend's create saw actor %+v (%v)", a, ok)
	}
}

// mintingBackend gives a shed with no id its id at provision admission, as
// the Firecracker and VZ backends do for a shed made before ids.
type mintingBackend struct{ *recBackend }

func (b mintingBackend) BeginProvision(ctx context.Context, name string) (ProvisionRun, error) {
	b.mu.Lock()
	sh := b.sheds[name]
	if sh.ID == "" {
		sh.ID = "mintedmintedmintedmintedmi"
		b.sheds[name] = sh
	}
	b.mu.Unlock()
	return b.recBackend.BeginProvision(ctx, name)
}

// TestGuardProvisionEventsCarryTheMintedID: a shed from before ids gets its
// id at admission; the provision's start and end events carry it, as the
// delivery's event does, so a filter by the id finds all three.
func TestGuardProvisionEventsCarryTheMintedID(t *testing.T) {
	rb, _ := fixture()
	g := NewGuard(mintingBackend{rb})
	lines := guardLog(t, g)
	pv, ok := AsProvisioner(g)
	if !ok {
		t.Fatal("no provisioner")
	}
	if _, err := Provision(as(alice), pv, "box"); err != nil {
		t.Fatal(err)
	}
	got := lines()
	if len(got) != 2 {
		t.Fatalf("lines = %v", got)
	}
	for _, l := range got {
		if l["shed_id"] != "mintedmintedmintedmintedmi" {
			t.Errorf("%v carries shed_id %v, want the minted id", l["event"], l["shed_id"])
		}
	}
	if got[0]["event"] != audit.EventProvisionStart || got[1]["event"] != audit.EventProvisionEnd {
		t.Errorf("events = %v, %v", got[0]["event"], got[1]["event"])
	}
}
