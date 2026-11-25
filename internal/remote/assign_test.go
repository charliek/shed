package remote

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// usersEnv is an environment whose bridge routes by user, as on a server
// with a user list.
func usersEnv(t *testing.T) (*env, *plugin.Bridge) {
	t.Helper()
	bridge := plugin.NewBridge(plugin.NewRegistry(), true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })
	return e, bridge
}

func (e *env) assign(name, user string) (*config.Shed, error) {
	e.t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		e.t.Fatalf("GetShed(%s): %v", name, err)
	}
	return e.b.AssignUser(backend.WithRequiredCreatedAt(ctxT(e.t), shed.CreatedAt), name, config.AssignShedRequest{User: user})
}

// A shed with no user, running or stopped, is assigned: its instance is
// labelled with the user, its record stores it, and a running shed's bus
// route moves to the user without a reconnect.
func TestAssignUserLabelsAndRecords(t *testing.T) {
	e, bridge := usersEnv(t)
	e.createFor("running", "")
	e.createFor("stopped", "")
	if _, err := e.b.StopShed(ctxT(t), "stopped"); err != nil {
		t.Fatal(err)
	}
	e.settled("stopped")
	if u, ok := bridge.ShedUser("running"); ok {
		t.Fatalf("test set-up: a shed with no user routes to %q", u)
	}

	for _, name := range []string{"running", "stopped"} {
		shed, err := e.assign(name, "bob")
		if err != nil {
			t.Fatalf("assign %s: %v", name, err)
		}
		if shed.User != "bob" || e.userOf(name) != "bob" {
			t.Errorf("%s: assign returned %q, GetShed reports %q", name, shed.User, e.userOf(name))
		}
		if v, ok := e.labelOf(name, provider.LabelUser); !ok || v != "bob" {
			t.Errorf("%s: label %s = %q (present %v), want bob", name, provider.LabelUser, v, ok)
		}
		if r := e.storedRecord(name); r.Shed.User != "bob" || r.UserConflict != "" {
			t.Errorf("%s: stored user %q conflict %q", name, r.Shed.User, r.UserConflict)
		}
	}
	waitShedUser(t, bridge, "running", "bob", true)
	if got := e.status("stopped"); got != config.StatusStopped {
		t.Errorf("the stopped shed is %s after the assignment", got)
	}

	// Assigned is assigned: a second assignment writes nothing.
	labels := e.prov.LabelWrites()
	if _, err := e.assign("running", "alice"); !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Errorf("assigning it again: %v, want already assigned", err)
	}
	if n := e.prov.LabelWrites(); n != labels {
		t.Errorf("a refused assignment wrote labels (%d writes, was %d)", n, labels)
	}
}

// The label is written first. When the label write fails without taking
// effect nothing changes: the record still has no user, here and on disk.
func TestAssignUserLabelWriteFails(t *testing.T) {
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	e.prov.FailNext(fake.OpClaimLabel, errors.New("the cloud refused the label write"))
	if _, err := e.assign("legacy", "bob"); err == nil || !strings.Contains(err.Error(), "the cloud refused the label write") {
		t.Fatalf("assign with the label write failing: %v", err)
	}
	if got := e.userOf("legacy"); got != "" {
		t.Errorf("user after a failed label write = %q", got)
	}
	if r := e.storedRecord("legacy"); r.Shed.User != "" {
		t.Errorf("stored user after a failed label write = %q", r.Shed.User)
	}
	if v, _ := e.labelOf("legacy", provider.LabelUser); v != "" {
		t.Errorf("label after a failed write = %q", v)
	}
	// Tried again, it goes through.
	if _, err := e.assign("legacy", "bob"); err != nil {
		t.Fatalf("assign again: %v", err)
	}
}

// The label is written and the record write then fails. The assignment
// stands: the record in memory has the user, and the next start takes it from
// the label and stores it.
func TestAssignUserLabelWrittenThenRecordWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not held back by directory permissions")
	}
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	sheds := filepath.Join(e.dir, "sheds")
	if err := os.Chmod(sheds, 0o500); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if err := os.Chmod(sheds, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)

	if _, err := e.assign("legacy", "bob"); err != nil {
		t.Fatalf("assign with the record write failing: %v", err)
	}
	if got := e.userOf("legacy"); got != "bob" {
		t.Errorf("user in memory = %q, want bob", got)
	}
	if !strings.Contains(strings.Join(e.logLines(), "\n"), "saving the record of shed legacy") {
		t.Fatalf("the failed record write was not logged:\n%s", strings.Join(e.logLines(), "\n"))
	}
	if r := e.storedRecord("legacy"); r.Shed.User != "" {
		t.Fatalf("test set-up: the record write did not fail (stored user %q)", r.Shed.User)
	}

	restore()
	e.restart()
	e.idle()
	if got := e.userOf("legacy"); got != "bob" {
		t.Errorf("user after the next start = %q, want bob from the label", got)
	}
	if r := e.storedRecord("legacy"); r.Shed.User != "bob" {
		t.Errorf("stored user after the next start = %q, want bob", r.Shed.User)
	}
}

// With its record store gone, an assigned shed is adopted from its labels
// with its user.
func TestAssignedShedIsAdoptedWithItsUser(t *testing.T) {
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	if _, err := e.assign("legacy", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	e.b = e.open()
	e.settled("legacy")
	if got := e.userOf("legacy"); got != "bob" {
		t.Errorf("adopted user = %q, want bob", got)
	}
}

// A shed deleted between the label write and the record write is not
// recorded as assigned, and nothing of another incarnation is touched.
func TestAssignUserDeletedAfterTheLabel(t *testing.T) {
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	assignLabelledHook = func(name string) {
		if err := e.b.DeleteShed(context.Background(), name); err != nil {
			t.Errorf("delete in the window: %v", err)
		}
	}
	t.Cleanup(func() { assignLabelledHook = nil })
	if _, err := e.assign("legacy", "bob"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("assign of a shed deleted after its label: %v, want not found", err)
	}
	if _, err := e.b.GetShed(context.Background(), "legacy"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("the deleted shed came back: %v", err)
	}
}

// A shed recreated under its name since the caller looked it up is not
// assigned: the record is checked by incarnation under the lock, before any
// label is written.
func TestAssignUserRefusesAnotherIncarnation(t *testing.T) {
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	shed, err := e.b.GetShed(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	stale := backend.WithRequiredCreatedAt(ctxT(t), shed.CreatedAt.Add(-1))
	labels := e.prov.LabelWrites()
	if _, err := e.b.AssignUser(stale, "legacy", config.AssignShedRequest{User: "bob"}); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("assign of another incarnation: %v, want not found", err)
	}
	if e.prov.LabelWrites() != labels || e.userOf("legacy") != "" {
		t.Errorf("the shed was changed")
	}
}

// Sheds that belong to someone are refused before any label is written: a
// user's, a service client's, and one whose record and label disagree.
func TestAssignUserRefusals(t *testing.T) {
	e, _ := usersEnv(t)
	e.createFor("alices", "alice")
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "svc", NoProvision: true, Owner: "nocturn"}); err != nil {
		t.Fatal(err)
	}
	e.settled("svc")
	e.createFor("odd", "alice")
	e.setUserLabel("odd", "carol")
	e.sweep()
	e.idle()
	if got := e.userOf("odd"); got != config.UserUnresolved {
		t.Fatalf("test set-up: odd is %q", got)
	}
	labels := e.prov.LabelWrites()
	for _, name := range []string{"alices", "svc", "odd"} {
		if _, err := e.assign(name, "bob"); !errors.Is(err, config.ErrShedAssignedSentinel) {
			t.Errorf("assign %s: %v, want already assigned", name, err)
		}
	}
	if e.prov.LabelWrites() != labels {
		t.Errorf("a refused assignment wrote a label")
	}
}

// The start-up pass on the remote backend: with one user every shed with no
// user is labelled and recorded as theirs; with two nothing is written.
func TestStampUnassignedOnRemote(t *testing.T) {
	for _, tc := range []struct {
		name  string
		users []backend.UpgradeUser
		want  string
	}{
		{"one user", []backend.UpgradeUser{{Name: "root", Admin: true}}, "root"},
		{"two users", []backend.UpgradeUser{{Name: "root", Admin: true}, {Name: "bob"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, bridge := usersEnv(t)
			e.createFor("legacy", "")
			e.createFor("alices", "alice")
			labels := e.prov.LabelWrites()
			rep := backend.StampUnassigned(ctxT(t), e.b, tc.users, true, backend.UpgradeLog{})
			if got := e.userOf("legacy"); got != tc.want {
				t.Errorf("legacy's user = %q, want %q", got, tc.want)
			}
			if v, _ := e.labelOf("legacy", provider.LabelUser); v != tc.want {
				t.Errorf("legacy's label = %q, want %q", v, tc.want)
			}
			if e.userOf("alices") != "alice" {
				t.Errorf("alice's shed changed")
			}
			if tc.want == "" {
				if e.prov.LabelWrites() != labels || len(rep.Unassigned) != 1 {
					t.Errorf("two users: labels written or report %+v", rep)
				}
				return
			}
			waitShedUser(t, bridge, "legacy", tc.want, true)
			if r := e.storedRecord("legacy"); r.Shed.User != tc.want {
				t.Errorf("stored user %q", r.Shed.User)
			}
		})
	}
}

// publishNow publishes a request from the running shed name on the bridge,
// as its message channel would, with no wait: the connection generation is
// found by trying each (a wrong one is refused without any effect).
func publishNow(t *testing.T, bridge *plugin.Bridge, name, namespace string) {
	t.Helper()
	for gen := uint64(1); gen < 1024; gen++ {
		env := plugin.NewEnvelope(namespace, plugin.MessageTypeRequest, json.RawMessage(`{"op":"list"}`))
		err := bridge.PublishToHost(context.Background(), name, gen, env)
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "is not registered") {
			t.Fatalf("publish from %s: %v", name, err)
		}
	}
	t.Fatalf("shed %s is not registered on the bus", name)
}

// A running shed's route is usable the moment AssignUser returns, even while
// the shed's bus-route worker is held (by the test hook): a request it
// publishes straight after the return reaches the new user's listener, with
// no wait. Before the assignment it reaches nobody.
func TestAssignUserRouteUsableAtReturn(t *testing.T) {
	// Registered first so it runs last, once Close has joined every worker.
	t.Cleanup(func() { busSyncHook = nil })
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })
	e.createFor("box", "")
	e.idle()
	bobs, err := reg.Register(plugin.ListenerKey{User: "bob", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := bridge.ShedUser("box"); ok {
		t.Fatalf("test set-up: a shed with no user routes to %q", u)
	}

	held := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before Close, so Close never waits on the hook
	busSyncHook = func(name string) {
		if name == "box" {
			held <- struct{}{}
			<-release
		}
	}

	if _, err := e.assign("box", "bob"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	publishNow(t, bridge, "box", "ssh-agent")
	select {
	case m := <-bobs.Messages:
		if env := bobs.Take(m); env == nil || env.Shed == nil || env.Shed.Name != "box" {
			t.Fatalf("bob's listener was handed %+v, want box's request", env)
		}
	default:
		t.Fatal("a request published as AssignUser returned did not reach the new user's listener")
	}
	if u, ok := bridge.ShedUser("box"); !ok || u != "bob" {
		t.Errorf("box routes to (%q, %v) at the return, want bob", u, ok)
	}

	// The worker stops at the hook before it applies anything, so it had
	// applied nothing above; checked here, after the publish, that it does
	// get there. Let go, it leaves the route as it is.
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the shed's bus-route worker never ran, so the test proved nothing about it")
	}
	unblock()
	e.idle()
	if u, ok := bridge.ShedUser("box"); !ok || u != "bob" {
		t.Errorf("box routes to (%q, %v) after the worker, want bob", u, ok)
	}
}

// Reconciliation records the new user between the label claim and
// AssignUser's re-check under the lock (forced: a sweep in the hook reads the
// label). That read holds the route and leaves it to the shed's bus-route
// worker, which is held here; AssignUser finds the user already recorded and
// must still make the route usable before it returns: a request published
// straight after the return reaches the new user's listener, with no wait.
func TestAssignUserRouteUsableWhenReconcileRecordedFirst(t *testing.T) {
	// Registered first so they run last, once Close has joined every worker.
	t.Cleanup(func() { busSyncHook, assignLabelledHook = nil, nil })
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })
	e.createFor("box", "")
	e.idle()
	bobs, err := reg.Register(plugin.ListenerKey{User: "bob", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before Close, so Close never waits on the hook
	busSyncHook = func(name string) {
		if name == "box" {
			held <- struct{}{}
			<-release
		}
	}
	reconciled := false
	assignLabelledHook = func(name string) {
		e.sweep()
		reconciled = e.userOf(name) == "bob"
	}

	if _, err := e.assign("box", "bob"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if !reconciled {
		t.Fatal("test set-up: the sweep in the window did not record bob from the label, so the branch under test was not reached")
	}
	publishNow(t, bridge, "box", "ssh-agent")
	select {
	case m := <-bobs.Messages:
		if env := bobs.Take(m); env == nil || env.Shed == nil || env.Shed.Name != "box" {
			t.Fatalf("bob's listener was handed %+v, want box's request", env)
		}
	default:
		t.Fatal("a request published as AssignUser returned did not reach the new user's listener (reconciliation recorded the user first)")
	}
	if u, ok := bridge.ShedUser("box"); !ok || u != "bob" {
		t.Errorf("box routes to (%q, %v) at the return, want bob", u, ok)
	}

	// The worker the sweep scheduled stopped at the hook before applying
	// anything; checked after the publish that it did get there.
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the shed's bus-route worker never ran, so the test proved nothing about it")
	}
	unblock()
	e.idle()
	if u, ok := bridge.ShedUser("box"); !ok || u != "bob" {
		t.Errorf("box routes to (%q, %v) after the worker, want bob", u, ok)
	}
}

// openSecond opens another Backend over the same cloud (the fake provider)
// with its own state directory: a second server on the same environment. It
// adopts every shed from its labels.
func (e *env) openSecond() *Backend {
	e.t.Helper()
	dir := e.dir
	e.dir = e.t.TempDir()
	b := e.open()
	e.dir = dir
	e.t.Cleanup(func() { _ = b.Close() })
	return b
}

// Two servers on one environment hold the same unassigned shed. The second
// verifies the instance (its label empty) and, before it writes, the first
// assigns the shed to alice (forced with the hook). The second's label
// claim finds alice and refuses as already assigned; the label stays alice's
// and the second server's record is not given bob.
func TestAssignUserTwoServersCannotBothAssign(t *testing.T) {
	t.Cleanup(func() { assignVerifiedHook = nil })
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	second := e.openSecond()
	shed, err := second.GetShed(context.Background(), "legacy")
	if err != nil || shed.User != "" {
		t.Fatalf("test set-up: the second server holds %+v, %v", shed, err)
	}

	fired := false
	assignVerifiedHook = func(name string) {
		if fired {
			return
		}
		fired = true
		if _, err := e.assign(name, "alice"); err != nil {
			t.Errorf("the first server's assignment in the window: %v", err)
		}
	}
	_, err = second.AssignUser(backend.WithRequiredCreatedAt(ctxT(t), shed.CreatedAt), "legacy", config.AssignShedRequest{User: "bob"})
	if !fired {
		t.Fatal("the hook never ran")
	}
	if !errors.Is(err, config.ErrShedAssignedSentinel) {
		t.Fatalf("the second server's assignment: %v, want already assigned", err)
	}
	if v, _ := e.labelOf("legacy", provider.LabelUser); v != "alice" {
		t.Errorf("label = %q, want alice", v)
	}
	// The refused assignment wrote nothing into the second server's record
	// (a read of the instance since may have taken alice from the label,
	// which is how the record follows the label).
	if got, err := second.GetShed(context.Background(), "legacy"); err != nil || (got.User != "" && got.User != "alice") {
		t.Errorf("the second server's record says user %q (%v), want none or alice", got.User, err)
	}
	if got := e.userOf("legacy"); got != "alice" {
		t.Errorf("the first server's record says %q, want alice", got)
	}
}

// A label write that takes effect but returns an error: the assignment fails
// and the record is untouched. Tried again for the same user it goes through
// without writing the label a second time; for another user it is refused,
// the label naming the first.
func TestAssignUserLabelWrittenButErrorReturned(t *testing.T) {
	for _, tc := range []struct {
		name, again string
		want        error
	}{
		{"the same user again", "bob", nil},
		{"another user", "alice", config.ErrShedAssignedSentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := usersEnv(t)
			e.createFor("legacy", "")
			e.prov.FailNextApplied(fake.OpClaimLabel, errors.New("the answer to the label write was lost"))
			if _, err := e.assign("legacy", "bob"); err == nil || !strings.Contains(err.Error(), "the answer to the label write was lost") {
				t.Fatalf("assign with the answer lost: %v", err)
			}
			if v, _ := e.labelOf("legacy", provider.LabelUser); v != "bob" {
				t.Fatalf("test set-up: the label write did not take effect (%q)", v)
			}
			if got := e.userOf("legacy"); got != "" {
				t.Errorf("record after the failed assignment = %q, want none", got)
			}
			writes := e.prov.LabelWrites()

			_, err := e.assign("legacy", tc.again)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("assign %s again: %v", tc.again, err)
				}
				if got := e.userOf("legacy"); got != "bob" {
					t.Errorf("record = %q, want bob", got)
				}
			} else {
				if !errors.Is(err, tc.want) {
					t.Fatalf("assign %s: %v, want %v", tc.again, err, tc.want)
				}
				if got := e.userOf("legacy"); got != "" {
					t.Errorf("record after the refusal = %q, want none", got)
				}
			}
			if n := e.prov.LabelWrites(); n != writes {
				t.Errorf("the label was written again (%d writes, was %d)", n, writes)
			}
			if v, _ := e.labelOf("legacy", provider.LabelUser); v != "bob" {
				t.Errorf("label = %q, want bob", v)
			}
		})
	}
}

// TestAssignWritesUserAndOriginTogether: an assignment writes the user and
// the origin they have now in one label update, so a read of the instance
// (here a periodic pass, made right after the label write and before the
// record's) sees the new user only with its new origin, and the record ends
// with both. A label write that fails leaves both labels and the record as
// they were. And a record whose known origin disagrees with its instance's
// label for the same user fails closed: unknown.
func TestAssignWritesUserAndOriginTogether(t *testing.T) {
	e, _ := usersEnv(t)
	svc := delivery.NewService(deliverytest.NewSource(), time.Second)
	svc.SetOrigin(func(u string) string {
		if u == "pat" {
			return delivery.OriginDirectory
		}
		return delivery.OriginLocal
	})
	e.b.SetDelivery(svc)
	e.createFor("box", "")
	e.createFor("other", "")
	if v, _ := e.labelOf("box", provider.LabelUserOrigin); v != delivery.OriginLocal {
		t.Fatalf("test set-up: an unassigned shed's origin label = %q", v)
	}

	assignLabelledHook = func(name string) {
		if u, _ := e.labelOf(name, provider.LabelUser); u != "pat" {
			t.Errorf("after the label write: user %q", u)
		}
		if o, _ := e.labelOf(name, provider.LabelUserOrigin); o != delivery.OriginDirectory {
			t.Errorf("after the label write: the user is pat and the origin %q: written apart", o)
		}
		e.sweep() // a read of the instance in the window
	}
	t.Cleanup(func() { assignLabelledHook = nil })
	if _, err := e.assign("box", "pat"); err != nil {
		t.Fatal(err)
	}
	assignLabelledHook = nil
	if r := e.storedRecord("box"); r.Shed.User != "pat" || r.UserOrigin != delivery.OriginDirectory {
		t.Fatalf("the record: user %q origin %q, want pat and directory", r.Shed.User, r.UserOrigin)
	}

	e.prov.FailNext(fake.OpClaimLabel, errors.New("the cloud refused the label write"))
	if _, err := e.assign("other", "pat"); err == nil {
		t.Fatal("an assignment whose label write failed succeeded")
	}
	if u, _ := e.labelOf("other", provider.LabelUser); u != "" {
		t.Errorf("a failed write left user label %q", u)
	}
	if o, _ := e.labelOf("other", provider.LabelUserOrigin); o != delivery.OriginLocal {
		t.Errorf("a failed write left origin label %q, want local as before", o)
	}
	if r := e.storedRecord("other"); r.Shed.User != "" || r.UserOrigin != delivery.OriginLocal {
		t.Errorf("a failed write changed the record: user %q origin %q", r.Shed.User, r.UserOrigin)
	}

	// The same user, the origins disagree: unknown, and it stays so.
	ref := e.ref("box")
	e.prov.Seed(func() provider.Instance {
		inst, err := e.prov.Get(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		inst.Labels[provider.LabelUserOrigin] = delivery.OriginLocal
		return inst
	}())
	e.sweep()
	if r := e.storedRecord("box"); r.UserOrigin != delivery.OriginUnknown {
		t.Fatalf("a record whose origin disagrees with its label = %q, want unknown", r.UserOrigin)
	}
	e.sweep()
	if r := e.storedRecord("box"); r.UserOrigin != delivery.OriginUnknown {
		t.Fatalf("an unknown origin was filled by a later read: %q", r.UserOrigin)
	}
}
