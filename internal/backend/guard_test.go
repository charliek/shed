package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// recBackend is a Backend that records which methods reached it and with
// what context. Like the real backends it checks the identity its context
// requires (owner, creation time, snapshot) against the record it holds when
// the call arrives, which is what the Guard relies on.
type recBackend struct {
	mu    sync.Mutex
	sheds map[string]config.Shed
	snaps map[string]config.Snapshot
	calls []string
	ctxs  map[string]context.Context
	reqs  []config.CreateShedRequest
	usage config.DiskUsage
	// lenientGet makes GetShed ignore a required creation time, so a test
	// sees the guard's own check of it rather than the backend's.
	lenientGet bool
}

func newRecBackend() *recBackend {
	return &recBackend{sheds: map[string]config.Shed{}, snaps: map[string]config.Snapshot{}, ctxs: map[string]context.Context{}}
}

func (b *recBackend) put(s config.Shed) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sheds[s.Name] = s
}

func (b *recBackend) putSnap(s config.Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snaps[s.Name] = s
}

// acted returns the methods that reached the backend, lookups excluded.
func (b *recBackend) acted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *recBackend) record(ctx context.Context, op string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, op)
	b.ctxs[op] = ctx
}

// checked returns the named shed after the identity checks a backend makes
// under its lock.
func (b *recBackend) checked(ctx context.Context, name string) (config.Shed, error) {
	b.mu.Lock()
	sh, ok := b.sheds[name]
	b.mu.Unlock()
	if !ok {
		return config.Shed{}, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	if err := CheckOwner(ctx, name, sh.Owner); err != nil {
		return config.Shed{}, err
	}
	if err := CheckCreatedAt(ctx, name, sh.CreatedAt); err != nil {
		return config.Shed{}, err
	}
	return sh, nil
}

func (b *recBackend) act(ctx context.Context, op, name string) error {
	if _, err := b.checked(ctx, name); err != nil {
		return err
	}
	b.record(ctx, op)
	return nil
}

func (b *recBackend) Type() Type   { return TypeFirecracker }
func (b *recBackend) Close() error { b.record(context.Background(), "Close"); return nil }

func (b *recBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	if req.FromSnapshot != "" {
		b.mu.Lock()
		snap, ok := b.snaps[req.FromSnapshot]
		b.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, req.FromSnapshot)
		}
		if err := CheckSnapshot(ctx, req.FromSnapshot, &snap); err != nil {
			return nil, err
		}
	}
	b.record(ctx, "CreateShed")
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	return &config.Shed{Name: req.Name, Owner: req.Owner, User: req.User}, nil
}

func (b *recBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	if b.lenientGet {
		ctx = context.Background()
	}
	sh, err := b.checked(ctx, name)
	if err != nil {
		return nil, err
	}
	return &sh, nil
}

func (b *recBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []config.Shed
	for _, sh := range b.sheds {
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *recBackend) DeleteShed(ctx context.Context, name string) error {
	return b.act(ctx, "DeleteShed", name)
}

func (b *recBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, b.act(ctx, "StartShed", name)
}

func (b *recBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, b.act(ctx, "StopShed", name)
}

// AssignUser applies the assignment rule to the shed it holds, as a real
// backend does under its lock, and records the user.
func (b *recBackend) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	sh, err := b.checked(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := CheckAssignable(&sh, req); err != nil {
		return nil, err
	}
	sh.User = req.User
	b.put(sh)
	b.record(ctx, "AssignUser")
	return &sh, nil
}

func (b *recBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return nil, b.act(ctx, "ResetShed", name)
}

func (b *recBackend) ListSessions(ctx context.Context, name string) ([]config.Session, error) {
	return nil, b.act(ctx, "ListSessions", name)
}

func (b *recBackend) KillSession(ctx context.Context, name, _ string) error {
	return b.act(ctx, "KillSession", name)
}

func (b *recBackend) Exec(ctx context.Context, name string, _ ExecOptions) error {
	return b.act(ctx, "Exec", name)
}

func (b *recBackend) DialService(ctx context.Context, name string, _ uint16) (net.Conn, error) {
	return nil, b.act(ctx, "DialService", name)
}

func (b *recBackend) ListImages(ctx context.Context) ([]config.ImageInfo, error) {
	b.record(ctx, "ListImages")
	return nil, nil
}

func (b *recBackend) InspectImage(ctx context.Context, _ string) (config.ImageInspectResponse, error) {
	b.record(ctx, "InspectImage")
	return config.ImageInspectResponse{}, nil
}

func (b *recBackend) TagImage(ctx context.Context, _, _ string) error {
	b.record(ctx, "TagImage")
	return nil
}

func (b *recBackend) PullImage(ctx context.Context, _, _, _ string, _ bool) (string, error) {
	b.record(ctx, "PullImage")
	return "", nil
}

func (b *recBackend) PushImage(ctx context.Context, _, _ string) error {
	b.record(ctx, "PushImage")
	return nil
}

func (b *recBackend) DeleteImage(ctx context.Context, _ string) error {
	b.record(ctx, "DeleteImage")
	return nil
}

func (b *recBackend) PruneImages(ctx context.Context, _ bool) ([]config.ImageInfo, error) {
	b.record(ctx, "PruneImages")
	return nil, nil
}

func (b *recBackend) DiskUsage(ctx context.Context) (config.DiskUsage, error) {
	b.record(ctx, "DiskUsage")
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usage
	u.Sheds = append([]config.ShedDiskEntry(nil), u.Sheds...)
	u.Snapshots = append([]config.SnapshotDiskEntry(nil), u.Snapshots...)
	u.Orphans = append([]config.FileEntry(nil), u.Orphans...)
	return u, nil
}

func (b *recBackend) Prune(ctx context.Context, _ PruneOptions) (config.PruneReport, error) {
	b.record(ctx, "Prune")
	return config.PruneReport{}, nil
}

func (b *recBackend) CreateSnapshot(ctx context.Context, req config.SnapshotCreateRequest) (*config.Snapshot, error) {
	return nil, b.act(ctx, "CreateSnapshot", req.SourceShed)
}

func (b *recBackend) ListSnapshots(context.Context) ([]config.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []config.Snapshot
	for _, s := range b.snaps {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *recBackend) GetSnapshot(_ context.Context, name string) (*config.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.snaps[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
	}
	return &s, nil
}

func (b *recBackend) DeleteSnapshot(ctx context.Context, name string) error {
	b.mu.Lock()
	s, ok := b.snaps[name]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", config.ErrSnapshotNotFoundSentinel, name)
	}
	if err := CheckSnapshot(ctx, name, &s); err != nil {
		return err
	}
	b.record(ctx, "DeleteSnapshot")
	return nil
}

// The egress capability.
func (b *recBackend) SetShedEgress(ctx context.Context, name string, _ []string) (*config.Shed, error) {
	return nil, b.act(ctx, "SetShedEgress", name)
}

func (b *recBackend) ClearShedEgress(ctx context.Context, name string) (*config.Shed, error) {
	return nil, b.act(ctx, "ClearShedEgress", name)
}

var (
	t0      = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	alice   = UserPrincipal("alice", false)
	bob     = UserPrincipal("bob", false)
	root    = UserPrincipal("root", true)
	nocturn = ClientPrincipal("nocturn")
)

func as(p Principal) context.Context { return WithPrincipal(context.Background(), p) }

// fixture: alice's shed "box" and her snapshot "snap".
func fixture() (*recBackend, *Guard) {
	b := newRecBackend()
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0, Status: config.StatusRunning})
	b.putSnap(config.Snapshot{Name: "snap", User: "alice", CreatedAt: t0})
	return b, NewGuard(b)
}

// guardCase says how one Backend method is guarded and how to call it.
type guardCase struct {
	// kind is "none" (needs no principal), "any" (needs one, any will do),
	// "create", "shed" (an operation on the named shed "box", with action),
	// "snapshot" (on the snapshot "snap", with action) or "admin".
	kind   string
	action Action
	call   func(g *Guard, ctx context.Context) error
}

func guardCases() map[string]guardCase {
	shed := func(a Action, call func(g *Guard, ctx context.Context) error) guardCase {
		return guardCase{kind: "shed", action: a, call: call}
	}
	return map[string]guardCase{
		"Type":  {kind: "none", call: func(g *Guard, _ context.Context) error { _ = g.Type(); return nil }},
		"Close": {kind: "none", call: func(g *Guard, _ context.Context) error { return g.Close() }},
		"CreateShed": {kind: "create", call: func(g *Guard, ctx context.Context) error {
			_, err := g.CreateShed(ctx, config.CreateShedRequest{Name: "new", ResolvedMounts: config.SelectedMounts(nil)})
			return err
		}},
		"GetShed":    shed(ActionView, func(g *Guard, ctx context.Context) error { _, err := g.GetShed(ctx, "box"); return err }),
		"ListSheds":  {kind: "any", call: func(g *Guard, ctx context.Context) error { _, err := g.ListSheds(ctx); return err }},
		"DeleteShed": shed(ActionManage, func(g *Guard, ctx context.Context) error { return g.DeleteShed(ctx, "box") }),
		"StartShed":  shed(ActionEnter, func(g *Guard, ctx context.Context) error { _, err := g.StartShed(ctx, "box"); return err }),
		"StopShed":   shed(ActionManage, func(g *Guard, ctx context.Context) error { _, err := g.StopShed(ctx, "box"); return err }),
		"ResetShed":  shed(ActionEnter, func(g *Guard, ctx context.Context) error { _, err := g.ResetShed(ctx, "box"); return err }),
		// On "legacy", a shed with no user, put in place by the call itself
		// (alice's "box" already has a user and cannot be assigned).
		"AssignUser": {kind: "admin", call: func(g *Guard, ctx context.Context) error {
			g.inner.(*recBackend).put(config.Shed{Name: "legacy", CreatedAt: t0, Mounts: config.SelectedMounts(nil)})
			_, err := g.AssignUser(ctx, "legacy", config.AssignShedRequest{User: "bob"})
			return err
		}},
		"ListSessions": shed(ActionEnter, func(g *Guard, ctx context.Context) error {
			_, err := g.ListSessions(ctx, "box")
			return err
		}),
		"KillSession": shed(ActionEnter, func(g *Guard, ctx context.Context) error { return g.KillSession(ctx, "box", "s") }),
		"Exec":        shed(ActionEnter, func(g *Guard, ctx context.Context) error { return g.Exec(ctx, "box", ExecOptions{}) }),
		"DialService": shed(ActionEnter, func(g *Guard, ctx context.Context) error {
			_, err := g.DialService(ctx, "box", 22)
			return err
		}),
		"ListImages":   {kind: "any", call: func(g *Guard, ctx context.Context) error { _, err := g.ListImages(ctx); return err }},
		"InspectImage": {kind: "any", call: func(g *Guard, ctx context.Context) error { _, err := g.InspectImage(ctx, "x"); return err }},
		"TagImage":     {kind: "admin", call: func(g *Guard, ctx context.Context) error { return g.TagImage(ctx, "a", "b") }},
		"PullImage": {kind: "admin", call: func(g *Guard, ctx context.Context) error {
			_, err := g.PullImage(ctx, "r", "t", "", false)
			return err
		}},
		"PushImage":   {kind: "admin", call: func(g *Guard, ctx context.Context) error { return g.PushImage(ctx, "a", "b") }},
		"DeleteImage": {kind: "admin", call: func(g *Guard, ctx context.Context) error { return g.DeleteImage(ctx, "a") }},
		"PruneImages": {kind: "admin", call: func(g *Guard, ctx context.Context) error { _, err := g.PruneImages(ctx, true); return err }},
		"DiskUsage":   {kind: "any", call: func(g *Guard, ctx context.Context) error { _, err := g.DiskUsage(ctx); return err }},
		"Prune": {kind: "admin", call: func(g *Guard, ctx context.Context) error {
			_, err := g.Prune(ctx, PruneOptions{DryRun: true})
			return err
		}},
		"CreateSnapshot": shed(ActionEnter, func(g *Guard, ctx context.Context) error {
			_, err := g.CreateSnapshot(ctx, config.SnapshotCreateRequest{Name: "s2", SourceShed: "box"})
			return err
		}),
		"ListSnapshots": {kind: "any", call: func(g *Guard, ctx context.Context) error { _, err := g.ListSnapshots(ctx); return err }},
		"GetSnapshot": {kind: "snapshot", action: ActionView, call: func(g *Guard, ctx context.Context) error {
			_, err := g.GetSnapshot(ctx, "snap")
			return err
		}},
		"DeleteSnapshot": {kind: "snapshot", action: ActionManage, call: func(g *Guard, ctx context.Context) error {
			return g.DeleteSnapshot(ctx, "snap")
		}},
	}
}

// TestGuardCoversEveryBackendMethod walks the Backend interface by reflection:
// every method must have a rule here, so a method added to Backend fails this
// test until it is classified. Then, for each method: a call with no principal
// is refused and reaches nothing; a stranger (another user, not an admin)
// reaches nothing that is alice's; alice reaches her own; and a guarded shed
// call reaches the backend bound to the incarnation the guard loaded.
func TestGuardCoversEveryBackendMethod(t *testing.T) {
	cases := guardCases()
	iface := reflect.TypeOf((*Backend)(nil)).Elem()
	seen := map[string]bool{}
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		seen[name] = true
		if _, ok := cases[name]; !ok {
			t.Errorf("Backend.%s has no guard rule in this test: classify it", name)
		}
	}
	for name := range cases {
		if !seen[name] {
			t.Errorf("guard case %s is not a Backend method", name)
		}
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.kind == "none" {
				b, g := fixture()
				if err := c.call(g, context.Background()); err != nil {
					t.Fatalf("no principal needed, got %v", err)
				}
				_ = b
				return
			}

			// No principal: refused, and the backend is not reached at all.
			b, g := fixture()
			if err := c.call(g, context.Background()); !errors.Is(err, ErrNoPrincipal) {
				t.Errorf("no principal: %v, want ErrNoPrincipal", err)
			}
			if got := b.acted(); len(got) != 0 {
				t.Errorf("no principal reached the backend: %v", got)
			}

			// Bob, a user who is not an admin, on alice's things.
			b, g = fixture()
			err := c.call(g, as(bob))
			switch c.kind {
			case "shed":
				if !errors.Is(err, config.ErrShedNotFoundSentinel) {
					t.Errorf("bob: %v, want shed not found", err)
				}
			case "snapshot":
				if !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
					t.Errorf("bob: %v, want snapshot not found", err)
				}
			case "admin":
				if !errors.Is(err, config.ErrForbiddenSentinel) {
					t.Errorf("bob: %v, want forbidden", err)
				}
			case "any", "create":
				if err != nil {
					t.Errorf("bob: %v, want allowed", err)
				}
			}
			if c.kind == "shed" || c.kind == "snapshot" || c.kind == "admin" {
				if got := b.acted(); len(got) != 0 {
					t.Errorf("bob reached the backend: %v", got)
				}
			}

			// Alice, on her own.
			b, g = fixture()
			err = c.call(g, as(alice))
			if c.kind == "admin" {
				if !errors.Is(err, config.ErrForbiddenSentinel) {
					t.Errorf("alice (not an admin): %v, want forbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("alice: %v", err)
			}
			if c.kind == "shed" && name != "GetShed" {
				ctx := b.ctxs[name]
				if ctx == nil {
					t.Fatalf("alice's %s did not reach the backend (calls %v)", name, b.acted())
				}
				if got, ok := RequiredCreatedAt(ctx); !ok || !got.Equal(t0) {
					t.Errorf("%s reached the backend without the shed's incarnation: %v %v", name, got, ok)
				}
			}
		})
	}
}

// TestGuardAdminRules: an admin may list, stop and delete alice's shed and
// may not enter it; the image store and prune are the admin's.
func TestGuardAdminRules(t *testing.T) {
	for name, c := range guardCases() {
		if c.kind == "none" {
			continue
		}
		b, g := fixture()
		err := c.call(g, as(root))
		switch {
		case c.kind == "shed" && c.action == ActionEnter:
			if !errors.Is(err, config.ErrForbiddenSentinel) {
				t.Errorf("admin %s on alice's shed: %v, want forbidden", name, err)
			}
			if len(b.acted()) != 0 {
				t.Errorf("admin %s reached the backend: %v", name, b.acted())
			}
		default:
			if err != nil {
				t.Errorf("admin %s: %v, want allowed", name, err)
			}
		}
	}
}

// TestGuardRequestWithNoPrincipalIsRefused: a call with no principal, or a
// malformed one, is refused and never treated as an admin.
func TestGuardRequestWithNoPrincipalIsRefused(t *testing.T) {
	b, g := fixture()
	for _, ctx := range []context.Context{
		context.Background(),
		WithPrincipal(context.Background(), Principal{}),
		WithPrincipal(context.Background(), Principal{Kind: PrincipalUser, Admin: true}),
	} {
		if _, err := g.ListSheds(ctx); !errors.Is(err, ErrNoPrincipal) {
			t.Errorf("ListSheds: %v, want ErrNoPrincipal", err)
		}
		if err := g.DeleteShed(ctx, "box"); !errors.Is(err, ErrNoPrincipal) {
			t.Errorf("DeleteShed: %v, want ErrNoPrincipal", err)
		}
	}
	if got := b.acted(); len(got) != 0 {
		t.Errorf("reached the backend: %v", got)
	}
}

// TestGuardDeleteAndRecreateBetweenLookupAndAction forces the interleaving
// the guard is built against: after the guard has loaded and authorized
// alice's shed, it is deleted and created again under the same name by bob.
// Every operation then reaches a backend that refuses it (the incarnation
// the guard passed down is not the shed it holds), and bob's shed is not
// acted on.
func TestGuardDeleteAndRecreateBetweenLookupAndAction(t *testing.T) {
	for name, c := range guardCases() {
		if c.kind != "shed" || name == "GetShed" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			b, g := fixture()
			g.SetBeforeAct(func(op, shed string) {
				if shed == "box" {
					b.put(config.Shed{Name: "box", User: "bob", CreatedAt: t0.Add(time.Minute), Status: config.StatusRunning})
				}
			})
			err := c.call(g, as(alice))
			if !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Errorf("%s across a recreate: %v, want not found", name, err)
			}
			if got := b.acted(); len(got) != 0 {
				t.Errorf("%s acted on bob's shed: %v", name, got)
			}
		})
	}
	// The egress capability, which also acts.
	for _, op := range []string{"SetShedEgress", "ClearShedEgress"} {
		b, g := fixture()
		g.SetBeforeAct(func(_, shed string) {
			b.put(config.Shed{Name: "box", User: "bob", CreatedAt: t0.Add(time.Minute)})
		})
		ec, ok := AsEgressController(g)
		if !ok {
			t.Fatal("the guard hides the egress capability")
		}
		var err error
		if op == "SetShedEgress" {
			_, err = ec.SetShedEgress(as(alice), "box", []string{"base"})
		} else {
			_, err = ec.ClearShedEgress(as(alice), "box")
		}
		if !errors.Is(err, config.ErrShedNotFoundSentinel) || len(b.acted()) != 0 {
			t.Errorf("%s across a recreate: %v, calls %v", op, err, b.acted())
		}
	}
}

// TestGuardSnapshotDeleteAndRetakeBetweenLookupAndAction: a snapshot deleted
// and taken again under its name between the guard's lookup and the action
// is not deleted, and a shed is not created from it.
func TestGuardSnapshotDeleteAndRetakeBetweenLookupAndAction(t *testing.T) {
	retake := func(b *recBackend, g *Guard) {
		g.SetBeforeAct(func(_, _ string) {
			b.putSnap(config.Snapshot{Name: "snap", User: "bob", CreatedAt: t0.Add(time.Minute)})
		})
	}
	b, g := fixture()
	retake(b, g)
	if err := g.DeleteSnapshot(as(alice), "snap"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Errorf("DeleteSnapshot across a retake: %v, want not found", err)
	}
	if got := b.acted(); len(got) != 0 {
		t.Errorf("deleted bob's snapshot: %v", got)
	}

	b, g = fixture()
	retake(b, g)
	_, err := g.CreateShed(as(alice), config.CreateShedRequest{Name: "new", FromSnapshot: "snap", ResolvedMounts: config.SelectedMounts(nil)})
	if !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Errorf("CreateShed from a retaken snapshot: %v, want not found", err)
	}
	if got := b.acted(); len(got) != 0 {
		t.Errorf("created from bob's snapshot: %v", got)
	}
}

// TestGuardNeverReplacesARequiredCreatedAt: a creation time the caller
// already required (the reaper's, from its listing) is checked against the
// shed the guard loads, and passed down unchanged; it is never replaced by
// the loaded shed's.
func TestGuardNeverReplacesARequiredCreatedAt(t *testing.T) {
	b, g := fixture()
	b.lenientGet = true
	stale := WithRequiredCreatedAt(as(SystemPrincipal()), t0.Add(-time.Hour))
	if err := g.DeleteShed(stale, "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("delete with an older required incarnation: %v, want not found", err)
	}
	if got := b.acted(); len(got) != 0 {
		t.Errorf("acted on a shed recreated since it was listed: %v", got)
	}

	same := WithRequiredCreatedAt(as(SystemPrincipal()), t0)
	if err := g.DeleteShed(same, "box"); err != nil {
		t.Fatalf("delete with the shed's own incarnation: %v", err)
	}
	if got, ok := RequiredCreatedAt(b.ctxs["DeleteShed"]); !ok || !got.Equal(t0) {
		t.Errorf("required incarnation passed down = %v %v", got, ok)
	}
}

// TestGuardUnresolvedShedIsAdminOnly: a shed whose records disagree about
// whose it is (reported config.UserUnresolved) reaches an admin only, and the
// admin may not enter it.
func TestGuardUnresolvedShedIsAdminOnly(t *testing.T) {
	b := newRecBackend()
	b.put(config.Shed{Name: "odd", User: config.UserUnresolved, CreatedAt: t0})
	g := NewGuard(b)
	if _, err := g.GetShed(as(alice), "odd"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("alice: %v, want not found", err)
	}
	if list, _ := g.ListSheds(as(alice)); len(list) != 0 {
		t.Errorf("alice lists %v", list)
	}
	if _, err := g.StopShed(as(root), "odd"); err != nil {
		t.Errorf("admin stop: %v", err)
	}
	if err := g.Exec(as(root), "odd", ExecOptions{}); !errors.Is(err, config.ErrForbiddenSentinel) {
		t.Errorf("admin exec: %v, want forbidden", err)
	}
}

// TestGuardListsAndDiskUsage: lists are filtered to what the caller may view;
// a caller who is not an admin sees only their own disk entries and no
// orphans, with the server's totals.
func TestGuardListsAndDiskUsage(t *testing.T) {
	b := newRecBackend()
	b.put(config.Shed{Name: "a1", User: "alice"})
	b.put(config.Shed{Name: "b1", User: "bob"})
	b.put(config.Shed{Name: "c1", Owner: "nocturn"})
	b.put(config.Shed{Name: "legacy"})
	b.putSnap(config.Snapshot{Name: "as", User: "alice"})
	b.putSnap(config.Snapshot{Name: "bs", User: "bob"})
	b.usage = config.DiskUsage{
		Sheds:     []config.ShedDiskEntry{{Name: "a1", User: "alice"}, {Name: "b1", User: "bob"}, {Name: "c1", Owner: "nocturn"}, {Name: "legacy"}},
		Snapshots: []config.SnapshotDiskEntry{{Name: "as", User: "alice"}, {Name: "bs", User: "bob"}},
		Orphans:   []config.FileEntry{{Path: "/x"}},
		Totals:    config.DiskUsageTotals{Sheds: config.DiskSize{LogicalBytes: 4}},
	}
	g := NewGuard(b)
	names := func(sheds []config.Shed) []string {
		var out []string
		for _, s := range sheds {
			out = append(out, s.Name)
		}
		return out
	}
	for _, tc := range []struct {
		p    Principal
		want []string
	}{
		{alice, []string{"a1"}},
		{nocturn, []string{"c1"}},
		{root, []string{"a1", "b1", "c1", "legacy"}},
		{ImplicitPrincipal(), []string{"a1", "b1", "c1", "legacy"}},
	} {
		got, err := g.ListSheds(as(tc.p))
		if err != nil || !reflect.DeepEqual(names(got), tc.want) {
			t.Errorf("%s lists %v (%v), want %v", tc.p, names(got), err, tc.want)
		}
	}
	snaps, _ := g.ListSnapshots(as(alice))
	if len(snaps) != 1 || snaps[0].Name != "as" {
		t.Errorf("alice's snapshots %v", snaps)
	}
	u, err := g.DiskUsage(as(alice))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Sheds) != 1 || u.Sheds[0].Name != "a1" || len(u.Snapshots) != 1 || u.Snapshots[0].Name != "as" || len(u.Orphans) != 0 || u.Totals.Sheds.LogicalBytes != 4 {
		t.Errorf("alice's df = %+v", u)
	}
	u, _ = g.DiskUsage(as(root))
	if len(u.Sheds) != 4 || len(u.Orphans) != 1 {
		t.Errorf("admin's df = %+v", u)
	}
}

// TestGuardCreateStampsAndAdminOnlyFields: the owner and user come from the
// principal, never the request; a user who is not an admin gets no host
// directories, no server mounts and no env_file; snapshots are used only by
// their owner (an admin may use one with no user).
func TestGuardCreateStampsAndAdminOnlyFields(t *testing.T) {
	none := config.SelectedMounts(nil)
	b, g := fixture()
	if _, err := g.CreateShed(as(alice), config.CreateShedRequest{Name: "n", Owner: "nocturn", User: "bob", ResolvedMounts: none}); err != nil {
		t.Fatal(err)
	}
	if r := b.reqs[0]; r.Owner != "" || r.User != "alice" || !r.NoServerEnv {
		t.Errorf("alice's create reached the backend as owner %q user %q noServerEnv %v", r.Owner, r.User, r.NoServerEnv)
	}
	if _, err := g.CreateShed(as(nocturn), config.CreateShedRequest{Name: "n2", User: "alice"}); err != nil {
		t.Fatal(err)
	}
	if r := b.reqs[1]; r.Owner != "nocturn" || r.User != "" {
		t.Errorf("client's create reached the backend as owner %q user %q", r.Owner, r.User)
	}
	if _, err := g.CreateShed(as(root), config.CreateShedRequest{Name: "n3", LocalDir: "/srv", ResolvedMounts: config.AllMounts()}); err != nil {
		t.Fatal(err)
	}
	if r := b.reqs[2]; r.User != "root" || r.NoServerEnv {
		t.Errorf("admin's create reached the backend as user %q noServerEnv %v", r.User, r.NoServerEnv)
	}
	if _, err := g.CreateShed(as(ImplicitPrincipal()), config.CreateShedRequest{Name: "n4", User: "alice", LocalDir: "/srv"}); err != nil {
		t.Fatal(err)
	}
	if r := b.reqs[3]; r.User != "" || r.Owner != "" || r.NoServerEnv {
		t.Errorf("implicit create reached the backend as %+v", r)
	}

	for _, req := range []config.CreateShedRequest{
		{Name: "x", LocalDir: "/srv", ResolvedMounts: none},
		{Name: "x", AddDirs: []string{"/srv"}, ResolvedMounts: none},
		{Name: "x"}, // nil mounts would be every server mount
		{Name: "x", ResolvedMounts: config.AllMounts()},
		{Name: "x", ResolvedMounts: config.SelectedMounts([]string{"gh"})},
	} {
		b, g := fixture()
		if _, err := g.CreateShed(as(alice), req); !errors.Is(err, config.ErrForbiddenSentinel) {
			t.Errorf("alice create %+v: %v, want forbidden", req, err)
		}
		if len(b.acted()) != 0 {
			t.Errorf("refused create reached the backend: %v", b.acted())
		}
	}

	// Snapshots.
	b, g = fixture()
	b.putSnap(config.Snapshot{Name: "legacy", CreatedAt: t0})
	use := func(p Principal, snap string) error {
		_, err := g.CreateShed(as(p), config.CreateShedRequest{Name: "c", FromSnapshot: snap, ResolvedMounts: none})
		return err
	}
	if err := use(alice, "snap"); err != nil {
		t.Errorf("alice from her snapshot: %v", err)
	}
	if err := use(bob, "snap"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Errorf("bob from alice's snapshot: %v, want not found", err)
	}
	if err := use(root, "snap"); !errors.Is(err, config.ErrForbiddenSentinel) {
		t.Errorf("admin from alice's snapshot: %v, want forbidden", err)
	}
	if err := use(root, "legacy"); err != nil {
		t.Errorf("admin from a snapshot with no user: %v", err)
	}
	if err := use(alice, "legacy"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Errorf("alice from a snapshot with no user: %v, want not found", err)
	}
	if err := use(nocturn, "legacy"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		t.Errorf("client from a snapshot: %v, want not found", err)
	}
}

// recAsync is a recBackend with an asynchronous lifecycle: a delete hides
// the record at once and leaves a tombstone for its incarnation until the
// VM is destroyed (or the destroy fails).
type recAsync struct {
	*recBackend
	destroyFails bool
	tombstones   map[string]time.Time // name -> created_at of the deleted incarnation
}

func (b *recAsync) DeleteShed(ctx context.Context, name string) error {
	sh, err := b.checked(ctx, name)
	if err != nil {
		return err
	}
	b.record(ctx, "DeleteShed")
	b.mu.Lock()
	delete(b.sheds, name)
	b.tombstones[name] = sh.CreatedAt
	b.mu.Unlock()
	return nil
}

var errDeleteOwed = errors.New("the VM could not be destroyed yet")

func (b *recAsync) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	b.record(ctx, "WaitSettled")
	if sh, err := b.checked(ctx, name); err == nil {
		return &sh, nil
	}
	b.mu.Lock()
	at, ok := b.tombstones[name]
	b.mu.Unlock()
	if ok && CheckCreatedAt(ctx, name, at) == nil && b.destroyFails {
		return nil, errDeleteOwed
	}
	return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
}

// TestGuardAsyncDelete: through the guard, a delete that waits for its VM is
// authorized once, at the delete; it follows that incarnation through its
// tombstone; and a destroy that failed is reported, never as success. The
// plain WaitSettled finds nothing it may authorize once the record is gone,
// which is why the delete has its own path.
func TestGuardAsyncDelete(t *testing.T) {
	newAsync := func(fails bool) (*recAsync, *Guard) {
		rb, _ := fixture()
		b := &recAsync{recBackend: rb, destroyFails: fails, tombstones: map[string]time.Time{}}
		return b, NewGuard(b)
	}
	if _, ok := IsAsync(NewGuard(newRecBackend())); ok {
		t.Fatal("a guard around a synchronous backend reports async")
	}

	b, g := newAsync(true)
	al, ok := IsAsync(g)
	if !ok {
		t.Fatal("the guard hides the async lifecycle")
	}
	d, ok := al.(AsyncDeleter)
	if !ok {
		t.Fatal("the guard's async lifecycle has no delete-and-wait")
	}
	if err := d.DeleteAndWait(as(alice), as(alice), "box"); !errors.Is(err, errDeleteOwed) {
		t.Errorf("failed destroy: %v, want it reported", err)
	}
	if got, ok := RequiredCreatedAt(b.ctxs["WaitSettled"]); !ok || !got.Equal(t0) {
		t.Errorf("the wait was not bound to the deleted incarnation: %v %v", got, ok)
	}

	_, g = newAsync(false)
	al, _ = IsAsync(g)
	if err := al.(AsyncDeleter).DeleteAndWait(as(alice), as(alice), "box"); err != nil {
		t.Errorf("finished destroy: %v", err)
	}
	// The record is gone: a plain wait has nothing to authorize.
	if _, err := al.WaitSettled(as(alice), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("wait after the delete: %v, want not found", err)
	}

	// Bob can neither delete alice's shed nor follow it.
	b, g = newAsync(true)
	al, _ = IsAsync(g)
	if err := al.(AsyncDeleter).DeleteAndWait(as(bob), as(bob), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("bob's delete: %v, want not found", err)
	}
	if _, err := al.WaitSettled(as(bob), "box"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("bob's wait: %v, want not found", err)
	}
	if got := b.acted(); len(got) != 0 {
		t.Errorf("bob reached the backend: %v", got)
	}
}

// capsBackend reports its own capabilities and network profiles.
type capsBackend struct{ *recBackend }

func (capsBackend) Capabilities() Capabilities                                { return RemoteCapabilities() }
func (capsBackend) SupportsNetworkProfile(p string) bool                      { return p == config.NetworkProfileIsolated }
func (capsBackend) WaitSettled(context.Context, string) (*config.Shed, error) { return nil, nil }

// TestGuardKeepsOptionalInterfaces: the capability questions read the backend
// the guard wraps, so the API sees the same capabilities through the guard.
func TestGuardKeepsOptionalInterfaces(t *testing.T) {
	plain := newRecBackend()
	if got, want := CapabilitiesOf(NewGuard(plain)), CapabilitiesOf(plain); got != want {
		t.Errorf("capabilities through the guard %+v, want %+v", got, want)
	}
	if _, ok := ReportedCapabilities(NewGuard(plain)); ok {
		t.Error("a backend that does not report reports through the guard")
	}
	rep := capsBackend{newRecBackend()}
	if got, ok := ReportedCapabilities(NewGuard(rep)); !ok || got != RemoteCapabilities() {
		t.Errorf("reported through the guard = %+v %v", got, ok)
	}
	if got, want := CapabilitiesOf(NewGuard(rep)), CapabilitiesOf(rep); got != want {
		t.Errorf("capabilities through the guard %+v, want %+v", got, want)
	}
	if !SupportsNetworkProfile(NewGuard(rep), config.NetworkProfileIsolated) || SupportsNetworkProfile(NewGuard(rep), config.NetworkProfileOpen) {
		t.Error("network profiles not read through the guard")
	}
	if _, ok := AsEgressController(NewGuard(rep)); !ok {
		t.Error("the egress capability is hidden by the guard")
	}
	if Guarded(NewGuard(plain)) == nil || Guarded(nil) != nil {
		t.Error("Guarded")
	}
	g := NewGuard(plain)
	if Guarded(g) != g {
		t.Error("Guarded wrapped a guard again")
	}
}

// TestGuardWithholdsRepoFromViewers: a principal who may view a shed but not
// enter it (an admin and another user's shed) gets its record with no
// repository URL and no landing directory at all, from every call that
// returns a record to them, wherever in the URL a token sits; the shed's own
// user, the implicit user and the server see both unchanged.
func TestGuardWithholdsRepoFromViewers(t *testing.T) {
	for form, repo := range map[string]string{
		"userinfo":    "https://alice:s3cr3t-T0KEN@git.example/private.git",
		"token user":  "https://s3cr3t-T0KEN@git.example/private.git",
		"query token": "https://git.example/private.git?access_token=s3cr3t-T0KEN",
		"path token":  "https://git.example/s3cr3t-T0KEN/private.git",
		"scp-like":    "git@git.example:s3cr3t-T0KEN/private.git",
	} {
		t.Run(form, func(t *testing.T) {
			_, landing, err := config.ResolveCreateLayout(config.CreateShedRequest{Repo: repo})
			if err != nil {
				t.Fatal(err)
			}
			checkWithheldFromViewers(t, repo, landing)
		})
	}
}

func checkWithheldFromViewers(t *testing.T, repo, landing string) {
	t.Helper()
	newG := func() (*recBackend, *Guard) {
		b := newRecBackend()
		b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0, Status: config.StatusRunning, Repo: repo, LandingDir: landing})
		return b, NewGuard(stopReturnsRecord{b})
	}
	viewed := map[string]func(g *Guard, ctx context.Context) (*config.Shed, error){
		"GetShed": func(g *Guard, ctx context.Context) (*config.Shed, error) { return g.GetShed(ctx, "box") },
		"ListSheds": func(g *Guard, ctx context.Context) (*config.Shed, error) {
			list, err := g.ListSheds(ctx)
			if err != nil || len(list) != 1 {
				return nil, fmt.Errorf("list %v: %w", list, err)
			}
			return &list[0], nil
		},
		"Check":    func(g *Guard, ctx context.Context) (*config.Shed, error) { return g.Check(ctx, "box", ActionView) },
		"StopShed": func(g *Guard, ctx context.Context) (*config.Shed, error) { return g.StopShed(ctx, "box") },
		"WaitSettled": func(g *Guard, ctx context.Context) (*config.Shed, error) {
			return guardedAsync{g: g, inner: recordWaiter{g.inner}}.WaitSettled(ctx, "box")
		},
	}
	for name, call := range viewed {
		for _, tc := range []struct {
			p             Principal
			repo, landing string
		}{
			{root, "", ""},
			{alice, repo, landing},
			{ImplicitPrincipal(), repo, landing},
			{SystemPrincipal(), repo, landing},
		} {
			_, g := newG()
			got, err := call(g, as(tc.p))
			if err != nil {
				t.Errorf("%s as %s: %v", name, tc.p, err)
				continue
			}
			if got.Repo != tc.repo || got.LandingDir != tc.landing {
				t.Errorf("%s as %s: repo %q landing %q, want %q %q", name, tc.p, got.Repo, got.LandingDir, tc.repo, tc.landing)
			}
		}
	}
	// The record the backend holds is never changed by withholding: the
	// backend here hands the guard its own record, not a copy, so a guard
	// that cleared the fields in place would clear them here too.
	b := newRecBackend()
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0})
	held := &config.Shed{Name: "box", User: "alice", CreatedAt: t0, Status: config.StatusRunning, Repo: repo, LandingDir: landing}
	g := NewGuard(heldRecord{b, held})
	got, err := g.GetShed(as(root), "box")
	if err != nil {
		t.Fatal(err)
	}
	if got == held {
		t.Error("the guard returned the backend's own record to a viewer")
	}
	if held.Repo != repo || held.LandingDir != landing {
		t.Errorf("the backend's record was changed: repo %q landing %q", held.Repo, held.LandingDir)
	}
}

// heldRecord is a recBackend whose GetShed hands out the one record it holds
// by pointer, as a backend with a cache could, so an in-place change by a
// caller shows in the record itself.
type heldRecord struct {
	*recBackend
	held *config.Shed
}

func (b heldRecord) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	if _, err := b.checked(ctx, name); err != nil {
		return nil, err
	}
	return b.held, nil
}

// stopReturnsRecord is a recBackend whose StopShed returns the record it
// holds, as the real backends do.
type stopReturnsRecord struct{ *recBackend }

func (b stopReturnsRecord) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	if err := b.act(ctx, "StopShed", name); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

// recordWaiter is an AsyncLifecycle whose wait returns the shed as held.
type recordWaiter struct{ b Backend }

func (w recordWaiter) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	return w.b.GetShed(ctx, name)
}

// TestGuardDiskUsageDecidesByTheEntryItself: whose a disk entry is comes from
// the entry, read in the same pass as its sizes. A snapshot and a shed that
// bob had when the usage was read, deleted and made again under the same
// names by alice before the guard looked, are not shown to alice as hers.
func TestGuardDiskUsageDecidesByTheEntryItself(t *testing.T) {
	b := newRecBackend()
	// The listings now: alice's shed "box" and snapshot "snap".
	b.put(config.Shed{Name: "box", User: "alice", CreatedAt: t0.Add(time.Minute)})
	b.putSnap(config.Snapshot{Name: "snap", User: "alice", CreatedAt: t0.Add(time.Minute)})
	// The usage, read a moment before: bob's shed and snapshot of those names.
	b.usage = config.DiskUsage{
		Sheds:     []config.ShedDiskEntry{{Name: "box", User: "bob", CreatedAt: t0}, {Name: "mine", User: "alice", CreatedAt: t0}},
		Snapshots: []config.SnapshotDiskEntry{{Name: "snap", SourceShed: "bobs-box", User: "bob", CreatedAt: t0}},
	}
	g := NewGuard(b)
	u, err := g.DiskUsage(as(alice))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Sheds) != 1 || u.Sheds[0].Name != "mine" {
		t.Errorf("alice's df sheds = %+v, want only her own entry", u.Sheds)
	}
	if len(u.Snapshots) != 0 {
		t.Errorf("alice's df snapshots = %+v, want none of bob's", u.Snapshots)
	}
	u, err = g.DiskUsage(as(bob))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Sheds) != 1 || u.Sheds[0].Name != "box" || len(u.Snapshots) != 1 {
		t.Errorf("bob's df = %+v %+v, want his shed and snapshot", u.Sheds, u.Snapshots)
	}
}

// TestGuardHostResourcesNeedAnAdminToEnter: a shed that carries host
// directories or server mounts (made while its user was an admin) can be
// entered and started only by an admin. Its user, once demoted, may still
// view, stop and delete it; an admin (the same user promoted, the implicit
// user) may enter it; a shed with an explicitly empty selection is not
// affected; a service client's shed follows its own policy.
func TestGuardHostResourcesNeedAnAdminToEnter(t *testing.T) {
	shapes := map[string]config.Shed{
		"host directories":       {ProjectMounts: []config.MountConfig{{Source: "/srv/code", Target: "/home/shed/code"}}, Mounts: config.SelectedMounts(nil)},
		"every server mount":     {Mounts: config.AllMounts()},
		"a named server mount":   {Mounts: config.SelectedMounts([]string{"gh"})},
		"an unknown mount mode":  {Mounts: &config.MountSelection{Mode: "future"}},
		"no mounts (stays open)": {Mounts: config.SelectedMounts(nil)},
		"no selection reported":  {},
	}
	for shape, sh := range shapes {
		carries := len(sh.HostResources()) > 0
		if want := !strings.HasSuffix(shape, "(stays open)") && shape != "no selection reported"; carries != want {
			t.Fatalf("%s: HostResources = %v", shape, sh.HostResources())
		}
		for name, c := range guardCases() {
			if c.kind != "shed" {
				continue
			}
			for _, tc := range []struct {
				who Principal
				// enter is whether an Enter action is allowed.
				enter bool
			}{
				{alice, !carries},
				{UserPrincipal("alice", true), true},
				{ImplicitPrincipal(), true},
			} {
				b := newRecBackend()
				s := sh
				s.Name, s.User, s.CreatedAt, s.Status = "box", "alice", t0, config.StatusRunning
				b.put(s)
				g := NewGuard(b)
				err := c.call(g, as(tc.who))
				allowed := c.action != ActionEnter || tc.enter
				switch {
				case allowed && err != nil:
					t.Errorf("%s, %s as %s: %v, want allowed", shape, name, tc.who, err)
				case !allowed && !errors.Is(err, config.ErrForbiddenSentinel):
					t.Errorf("%s, %s as %s: %v, want forbidden", shape, name, tc.who, err)
				case !allowed && len(b.acted()) != 0:
					t.Errorf("%s, %s as %s reached the backend: %v", shape, name, tc.who, b.acted())
				}
			}
		}
	}
	// A service client's own shed with its policy's mounts.
	b := newRecBackend()
	b.put(config.Shed{Name: "box", Owner: "nocturn", CreatedAt: t0, Mounts: config.SelectedMounts([]string{"gh"})})
	if err := NewGuard(b).Exec(as(nocturn), "box", ExecOptions{}); err != nil {
		t.Errorf("client exec into its own shed with policy mounts: %v", err)
	}
}
