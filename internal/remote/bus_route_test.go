package remote

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// TestRemoteRouteIsHeldAtOnceWhenTheReportedUserChanges: the read of an
// instance that changes a shed's reported user takes the shed's bus route
// away before it returns, while the new user is still to be applied (the
// shed's bus worker is held here by a hook), so the previous user's machine
// is never sent another request of the shed nor able to answer one. Each
// shed's change is applied by its own worker: another shed's change goes
// through, hold lifted and all, while this shed's worker is still held.
func TestRemoteRouteIsHeldAtOnceWhenTheReportedUserChanges(t *testing.T) {
	// Registered first so it runs last, once Close has joined every worker.
	t.Cleanup(func() { busSyncHook, busSyncDoneHook = nil, nil })

	bridge := plugin.NewBridge(plugin.NewRegistry(), true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })
	e.createFor("box", "alice")
	e.createFor("other", "alice")
	waitShedUser(t, bridge, "box", "alice", true)
	waitShedUser(t, bridge, "other", "alice", true)

	boxHeld := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before Close, so Close never waits on the hook
	otherDone := make(chan struct{}, 8)
	// No worker runs yet (no reported user has changed), so these writes
	// race nothing.
	busSyncHook = func(name string) {
		if name == "box" {
			boxHeld <- struct{}{}
			<-release
		}
	}
	busSyncDoneHook = func(name string) {
		if name == "other" {
			otherDone <- struct{}{}
		}
	}
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never happened", what)
		}
	}

	e.setUserLabel("box", "bob")
	e.sweep() // records the conflict: box is unresolved
	if got := e.userOf("box"); got != config.UserUnresolved {
		t.Fatalf("test set-up: box reported as %q, want %q", got, config.UserUnresolved)
	}
	if u, ok := bridge.ShedUser("box"); ok {
		t.Fatalf("box still routes to %q once its reported user changed", u)
	}
	wait(boxHeld, "box's bus worker starting")
	if u, ok := bridge.ShedUser("box"); ok {
		t.Fatalf("box routes to %q while its new user is still to be applied", u)
	}

	// other's changes are applied to the end while box's worker is held.
	e.setUserLabel("other", "bob")
	e.sweep()
	wait(otherDone, "other's change being applied while box's worker is held")
	e.setUserLabel("other", "alice")
	e.sweep()
	wait(otherDone, "other's heal being applied while box's worker is held")
	if u, ok := bridge.ShedUser("other"); !ok || u != "alice" {
		t.Fatalf("other routes to (%q, %v) while box's worker is held, want alice", u, ok)
	}
	if _, ok := bridge.ShedUser("box"); ok {
		t.Fatal("box's route came back while its worker was held")
	}

	// box's label put back in line, then its worker let go: it ends up
	// routed to alice again.
	e.setUserLabel("box", "alice")
	e.sweep()
	unblock()
	waitShedUser(t, bridge, "box", "alice", true)
}
