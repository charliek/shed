package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/state"
	"github.com/charliek/shed/internal/vmutil"
)

// createFor creates a shed for a user and waits for it to settle running.
func (e *env) createFor(name, user string) {
	e.t.Helper()
	req := config.CreateShedRequest{Name: name, NoProvision: true, User: user}
	if _, err := e.b.CreateShed(ctxT(e.t), req); err != nil {
		e.t.Fatalf("create %s for %q: %v", name, user, err)
	}
	if got := e.settled(name).Status; got != config.StatusRunning {
		e.t.Fatalf("shed %s settled %q, want running", name, got)
	}
}

// userOf is the user GetShed reports for a shed.
func (e *env) userOf(name string) string {
	e.t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		e.t.Fatalf("GetShed(%s): %v", name, err)
	}
	return shed.User
}

// labelOf is a label on a shed's instance.
func (e *env) labelOf(name, key string) (string, bool) {
	e.t.Helper()
	inst, err := e.prov.Get(context.Background(), e.ref(name))
	if err != nil {
		e.t.Fatalf("Get(%s): %v", name, err)
	}
	v, ok := inst.Labels[key]
	return v, ok
}

// storedRecord reads a shed's record from disk, as the next server would.
func (e *env) storedRecord(name string) *state.Record {
	e.t.Helper()
	st, err := state.Open(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	recs, _, err := st.Records()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range recs {
		if r.Shed.Name == name {
			return r
		}
	}
	e.t.Fatalf("no record of shed %s on disk", name)
	return nil
}

// rewriteRecord edits a shed's record on disk with the backend closed.
func (e *env) rewriteRecord(name string, fn func(*state.Record)) {
	e.t.Helper()
	r := e.storedRecord(name)
	fn(r)
	st, err := state.Open(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := st.SaveRecord(r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setUserLabel(name, user string) {
	e.t.Helper()
	if err := e.prov.SetLabels(context.Background(), e.ref(name), map[string]string{provider.LabelUser: user}); err != nil {
		e.t.Fatalf("SetLabels(%s): %v", name, err)
	}
}

// A shed's user is stored in its record and on its instance, and stays the
// same through a stop, a start and a server restart.
func TestUserSurvivesStopStartAndRestart(t *testing.T) {
	e := newEnv(t)
	e.createFor("mine", "alice")
	e.createFor("implicit", "")

	if got := e.userOf("mine"); got != "alice" {
		t.Fatalf("user after create = %q, want alice", got)
	}
	if v, ok := e.labelOf("mine", provider.LabelUser); !ok || v != "alice" {
		t.Fatalf("instance label %s = %q (present %v), want alice", provider.LabelUser, v, ok)
	}
	if v, ok := e.labelOf("implicit", provider.LabelUser); !ok || v != "" {
		t.Fatalf("implicit user's instance label %s = %q (present %v), want present and empty", provider.LabelUser, v, ok)
	}

	if _, err := e.b.StopShed(ctxT(t), "mine"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("mine"); got.Status != config.StatusStopped || got.User != "alice" {
		t.Fatalf("after stop: status %q user %q, want stopped and alice", got.Status, got.User)
	}
	if _, err := e.b.StartShed(ctxT(t), "mine"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("mine"); got.Status != config.StatusRunning || got.User != "alice" {
		t.Fatalf("after start: status %q user %q, want running and alice", got.Status, got.User)
	}

	e.restart()
	e.idle()
	if got := e.userOf("mine"); got != "alice" {
		t.Fatalf("user after restart = %q, want alice", got)
	}
	if got := e.userOf("implicit"); got != "" {
		t.Fatalf("implicit user after restart = %q, want empty", got)
	}
	sheds, err := e.b.ListSheds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sheds {
		want := map[string]string{"mine": "alice", "implicit": ""}[s.Name]
		if s.User != want {
			t.Errorf("ListSheds: %s has user %q, want %q", s.Name, s.User, want)
		}
	}
	if r := e.storedRecord("mine"); r.Shed.User != "alice" || r.UserConflict != "" {
		t.Errorf("stored record: user %q conflict %q, want alice and none", r.Shed.User, r.UserConflict)
	}
}

// With the record store gone, a shed is adopted from its labels with the user
// its shed_user label names; an instance labelled before the label existed
// is adopted with no user.
func TestAdoptionCarriesTheUserLabel(t *testing.T) {
	e := newEnv(t)
	e.createFor("alices", "alice")
	e.createFor("legacy", "")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	// "legacy" was labelled by a build that predates shed_user.
	legacy := e.ref("legacy")
	e.prov.Seed(func() provider.Instance {
		inst, err := e.prov.Get(context.Background(), legacy)
		if err != nil {
			t.Fatal(err)
		}
		delete(inst.Labels, provider.LabelUser)
		return inst
	}())
	if _, ok := e.labelOf("legacy", provider.LabelUser); ok {
		t.Fatal("test set-up: the legacy instance still carries the user label")
	}
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}

	e.b = e.open()
	e.settled("alices")
	e.settled("legacy")
	if got := e.userOf("alices"); got != "alice" {
		t.Errorf("adopted user = %q, want alice", got)
	}
	if got := e.userOf("legacy"); got != "" {
		t.Errorf("adopted user of an instance with no %s label = %q, want none", provider.LabelUser, got)
	}
	if r := e.storedRecord("alices"); r.Shed.User != "alice" {
		t.Errorf("adopted record stores user %q, want alice", r.Shed.User)
	}
}

// The label is written and the local write of the user then fails. The user
// is not lost and not wrong: the server keeps the label's user in memory, and
// the next start takes it from the label again and stores it.
//
// Two forms: a record that lost its user (rewritten by a build that predates
// users), healed from the label with its save failing; and a record that is
// gone, whose re-creation from the labels fails.
func TestLabelWrittenThenLocalWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not held back by directory permissions")
	}
	sheds := func(e *env) string { return filepath.Join(e.dir, "sheds") }
	readOnly := func(t *testing.T, dir string) (restore func()) {
		t.Helper()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		done := false
		restore = func() {
			if !done {
				done = true
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Cleanup(restore)
		return restore
	}

	t.Run("the heal of a record that lost its user", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("mine", "alice")
		if err := e.b.Close(); err != nil {
			t.Fatal(err)
		}
		e.rewriteRecord("mine", func(r *state.Record) { r.Shed.User = "" })
		restore := readOnly(t, sheds(e))

		e.b = e.open()
		e.settled("mine")
		if got := e.userOf("mine"); got != "alice" {
			t.Fatalf("user with the record write failing = %q, want alice from the label", got)
		}
		if !strings.Contains(strings.Join(e.logLines(), "\n"), "saving the record of shed mine") {
			t.Fatalf("the failed record write was not logged:\n%s", strings.Join(e.logLines(), "\n"))
		}
		if r := e.storedRecord("mine"); r.Shed.User != "" {
			t.Fatalf("test set-up: the record write did not fail (stored user %q)", r.Shed.User)
		}

		restore()
		e.restart()
		e.idle()
		if got := e.userOf("mine"); got != "alice" {
			t.Fatalf("user after the next start = %q, want alice", got)
		}
		if r := e.storedRecord("mine"); r.Shed.User != "alice" {
			t.Errorf("stored user after the next start = %q, want alice", r.Shed.User)
		}
	})

	t.Run("the adoption of a shed whose record is gone", func(t *testing.T) {
		e := newEnv(t)
		e.createFor("mine", "alice")
		if err := e.b.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(sheds(e), "mine.json")); err != nil {
			t.Fatal(err)
		}
		restore := readOnly(t, sheds(e))

		e.b = e.open()
		e.idle()
		if _, err := e.b.GetShed(context.Background(), "mine"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Fatalf("GetShed with the adoption unrecorded = %v, want not found", err)
		}

		restore()
		e.restart()
		e.settled("mine")
		if got := e.userOf("mine"); got != "alice" {
			t.Fatalf("user after the next start = %q, want alice", got)
		}
	})
}

// A local record and its instance's label that name two different users fail
// closed: the shed is reported with config.UserUnresolved (no user can equal
// it), the conflict is reported, and it holds across restarts until the two
// agree again.
func TestUserLabelConflictFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.createFor("mine", "alice")
	e.setUserLabel("mine", "bob")

	e.restart()
	e.idle()
	if got := e.userOf("mine"); got != config.UserUnresolved {
		t.Fatalf("user with the label naming bob = %q, want %q", got, config.UserUnresolved)
	}
	if rep := strings.Join(e.b.Reported(), "\n"); !strings.Contains(rep, `shed mine is recorded with user "alice"`) || !strings.Contains(rep, `shed_user="bob"`) {
		t.Fatalf("the conflict was not reported: %q", rep)
	}
	r := e.storedRecord("mine")
	if r.Shed.User != "alice" || r.UserConflict != "bob" {
		t.Fatalf("stored record: user %q conflict %q, want alice and bob kept as found", r.Shed.User, r.UserConflict)
	}

	// Still unresolved after another restart, and reported again.
	e.restart()
	e.idle()
	if got := e.userOf("mine"); got != config.UserUnresolved {
		t.Fatalf("user after a second restart = %q, want %q", got, config.UserUnresolved)
	}
	if rep := strings.Join(e.b.Reported(), "\n"); !strings.Contains(rep, "only an admin can reach it") {
		t.Fatalf("the conflict was not reported at the second start: %q", rep)
	}

	// The label put back in line: the record's user is reported again.
	e.setUserLabel("mine", "alice")
	e.restart()
	e.idle()
	if got := e.userOf("mine"); got != "alice" {
		t.Fatalf("user with the label back in line = %q, want alice", got)
	}
	if r := e.storedRecord("mine"); r.UserConflict != "" {
		t.Errorf("stored conflict after the label agreed = %q, want none", r.UserConflict)
	}

	// An empty label (absent means no user) does not override the record.
	e.setUserLabel("mine", "")
	e.restart()
	e.idle()
	if got := e.userOf("mine"); got != "alice" {
		t.Errorf("user with an empty label = %q, want alice from the record", got)
	}
}

// A service client's shed whose instance is labelled with a user is a record
// naming both: it is reported with no owner and config.UserUnresolved, and
// the service client no longer reaches it.
func TestOwnedShedWithAUserLabelFailsClosed(t *testing.T) {
	e := newEnv(t)
	req := config.CreateShedRequest{Name: "theirs", NoProvision: true, Owner: "nocturn"}
	if _, err := e.b.CreateShed(ctxT(t), req); err != nil {
		t.Fatal(err)
	}
	e.settled("theirs")
	clientCtx := backend.WithRequiredOwner(context.Background(), "nocturn")
	if _, err := e.b.GetShed(clientCtx, "theirs"); err != nil {
		t.Fatalf("the client's own shed: %v", err)
	}

	e.setUserLabel("theirs", "alice")
	e.restart()
	e.idle()
	shed, err := e.b.GetShed(context.Background(), "theirs")
	if err != nil {
		t.Fatal(err)
	}
	if shed.Owner != "" || shed.User != config.UserUnresolved {
		t.Fatalf("reported owner %q user %q, want none and %q", shed.Owner, shed.User, config.UserUnresolved)
	}
	if _, err := e.b.GetShed(clientCtx, "theirs"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Fatalf("the service client reaches the shed: %v, want not found", err)
	}
}

// An instance labelled with both an owner and a user (no server writes that)
// is adopted as found, reported with no owner and config.UserUnresolved, and
// reported at start-up.
func TestAdoptedOwnerAndUserFailsClosed(t *testing.T) {
	e := newEnv(t)
	req := config.CreateShedRequest{Name: "odd", NoProvision: true, Owner: "nocturn"}
	if _, err := e.b.CreateShed(ctxT(t), req); err != nil {
		t.Fatal(err)
	}
	e.settled("odd")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.setUserLabel("odd", "alice")
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}

	e.b = e.open()
	shed := e.settled("odd")
	if shed.Owner != "" || shed.User != config.UserUnresolved {
		t.Fatalf("adopted shed reported with owner %q user %q, want none and %q", shed.Owner, shed.User, config.UserUnresolved)
	}
	if _, err := e.b.GetShed(backend.WithRequiredOwner(context.Background(), "nocturn"), "odd"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("the service client reaches the shed: %v, want not found", err)
	}
	if rep := strings.Join(e.b.Reported(), "\n"); !strings.Contains(rep, `labelled with owner "nocturn" and user "alice"`) {
		t.Errorf("the adoption was not reported: %q", rep)
	}
}

// A create for a user and a service client at once, or for a user name that
// is not valid, is refused before anything is recorded or sent to the cloud.
func TestCreateRefusesBadOwnership(t *testing.T) {
	e := newEnv(t)
	for _, req := range []config.CreateShedRequest{
		{Name: "both", NoProvision: true, Owner: "nocturn", User: "alice"},
		{Name: "bad-user", NoProvision: true, User: "Alice"},
		{Name: "marker", NoProvision: true, User: config.UserUnresolved},
	} {
		if _, err := e.b.CreateShed(ctxT(t), req); !errors.Is(err, config.ErrInvalidShedRequestSentinel) {
			t.Errorf("create %s (owner %q user %q) = %v, want an invalid request", req.Name, req.Owner, req.User, err)
		}
	}
	e.idle()
	// Refused up front, with the request's own error, not by the provider
	// later when the labels are built.
	if n := e.prov.Count(fake.OpResolveImage); n != 0 {
		t.Errorf("%d image resolutions for refused creates, want none", n)
	}
	if sheds, _ := e.b.ListSheds(context.Background()); len(sheds) != 0 {
		t.Errorf("%d sheds recorded after refused creates", len(sheds))
	}
	if n := e.instances(); n != 0 {
		t.Errorf("%d instances after refused creates", n)
	}
}

// A shed_user label can be a valid label value and not a valid user name
// (no server writes one). It is never stored or reported as the shed's user:
// it is a conflict, reported unresolved and noted, whether the shed is
// adopted from its labels or its record (with no user to heal) is read
// beside it.
func TestInvalidUserNameLabelIsAConflict(t *testing.T) {
	for _, label := range []string{"alice_smith", "123"} {
		t.Run(label, func(t *testing.T) {
			if !provider.ValidLabelValue(label) || config.ValidUserName(label) {
				t.Fatalf("test set-up: %q must be a valid label value and an invalid user name", label)
			}

			t.Run("adopted", func(t *testing.T) {
				e := newEnv(t)
				e.createFor("mine", "")
				if err := e.b.Close(); err != nil {
					t.Fatal(err)
				}
				e.setUserLabel("mine", label)
				if err := os.RemoveAll(e.dir); err != nil {
					t.Fatal(err)
				}
				e.b = e.open()
				e.settled("mine")
				if got := e.userOf("mine"); got != config.UserUnresolved {
					t.Errorf("adopted user = %q, want %q", got, config.UserUnresolved)
				}
				if r := e.storedRecord("mine"); r.Shed.User != "" || r.UserConflict != label {
					t.Errorf("adopted record: user %q conflict %q, want none and %q", r.Shed.User, r.UserConflict, label)
				}
				if rep := strings.Join(e.b.Reported(), "\n"); !strings.Contains(rep, "not a valid user name") {
					t.Errorf("the label was not reported: %q", rep)
				}
			})

			t.Run("beside a record with no user", func(t *testing.T) {
				e := newEnv(t)
				e.createFor("mine", "")
				e.setUserLabel("mine", label)
				e.restart()
				e.idle()
				if got := e.userOf("mine"); got != config.UserUnresolved {
					t.Errorf("user = %q, want %q", got, config.UserUnresolved)
				}
				if r := e.storedRecord("mine"); r.Shed.User != "" || r.UserConflict != label {
					t.Errorf("stored record: user %q conflict %q, want none and %q", r.Shed.User, r.UserConflict, label)
				}
				if rep := strings.Join(e.b.Reported(), "\n"); !strings.Contains(rep, "not a valid user name") {
					t.Errorf("the label was not reported: %q", rep)
				}
			})
		})
	}
}

// A shed whose instance is not in the start-up list (a server restarted with
// another configured zone lists none of its sheds) is read through the
// record's stored reference. That read applies the same user-label rules as
// a listed instance: a service client's record with a user label conflicts,
// a record with no user is healed, and a conflict clears once the label
// agrees.
func TestUserLabelReconciledThroughTheStoredReference(t *testing.T) {
	h := &hookProvider{}
	e := newEnv(t, hooked(h))
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "theirs", NoProvision: true, Owner: "nocturn"}); err != nil {
		t.Fatal(err)
	}
	e.settled("theirs")
	e.createFor("lost", "alice")
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	e.setUserLabel("theirs", "alice")
	e.rewriteRecord("lost", func(r *state.Record) { r.Shed.User = "" })
	h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }

	e.b = e.open()
	e.idle()
	shed, err := e.b.GetShed(context.Background(), "theirs")
	if err != nil {
		t.Fatal(err)
	}
	if shed.Owner != "" || shed.User != config.UserUnresolved {
		t.Errorf("unlisted owned shed with a user label: owner %q user %q, want none and %q", shed.Owner, shed.User, config.UserUnresolved)
	}
	if _, err := e.b.GetShed(backend.WithRequiredOwner(context.Background(), "nocturn"), "theirs"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("the service client reaches the conflicted shed: %v, want not found", err)
	}
	if r := e.storedRecord("theirs"); r.UserConflict != "alice" {
		t.Errorf("stored conflict = %q, want alice", r.UserConflict)
	}
	if got := e.userOf("lost"); got != "alice" {
		t.Errorf("unlisted record with no user: user %q, want alice healed from the label", got)
	}
	if r := e.storedRecord("lost"); r.Shed.User != "alice" {
		t.Errorf("healed record stores user %q, want alice", r.Shed.User)
	}

	// The label emptied: the conflict clears on the same path.
	e.setUserLabel("theirs", "")
	e.restart()
	e.idle()
	shed, err = e.b.GetShed(context.Background(), "theirs")
	if err != nil {
		t.Fatal(err)
	}
	if shed.Owner != "nocturn" || shed.User != "" {
		t.Errorf("after the label agreed: owner %q user %q, want nocturn and none", shed.Owner, shed.User)
	}
	if r := e.storedRecord("theirs"); r.UserConflict != "" {
		t.Errorf("stored conflict after the label agreed = %q, want none", r.UserConflict)
	}
}

// A start job reads the shed's instance, and applies the user-label rules
// as every read does: a label naming another user found at a start makes
// the shed unresolved, and the next start that finds it in line clears it.
func TestStartJobReconcilesTheUserLabel(t *testing.T) {
	e := newEnv(t)
	e.createFor("mine", "alice")
	stopStart := func() {
		t.Helper()
		if _, err := e.b.StopShed(ctxT(t), "mine"); err != nil {
			t.Fatal(err)
		}
		e.settled("mine")
		if _, err := e.b.StartShed(ctxT(t), "mine"); err != nil {
			t.Fatal(err)
		}
		if got := e.settled("mine").Status; got != config.StatusRunning {
			t.Fatalf("settled %q, want running", got)
		}
	}

	e.setUserLabel("mine", "bob")
	stopStart()
	if got := e.userOf("mine"); got != config.UserUnresolved {
		t.Errorf("user after a start found the label naming bob = %q, want %q", got, config.UserUnresolved)
	}
	if r := e.storedRecord("mine"); r.Shed.User != "alice" || r.UserConflict != "bob" {
		t.Errorf("stored record: user %q conflict %q, want alice and bob", r.Shed.User, r.UserConflict)
	}

	e.setUserLabel("mine", "alice")
	stopStart()
	if got := e.userOf("mine"); got != "alice" {
		t.Errorf("user after a start found the label in line = %q, want alice", got)
	}
	if r := e.storedRecord("mine"); r.UserConflict != "" {
		t.Errorf("stored conflict = %q, want none", r.UserConflict)
	}
}

// A create resumed after a restart, whose instance the start-up list does not
// show, reads its instance in the job. A record with no user whose instance
// is labelled with one is healed there.
func TestResumedCreateReconcilesTheUserLabel(t *testing.T) {
	h := &hookProvider{}
	e := newEnv(t, manual(), hooked(h))
	const name = "resume"
	e.put(name, config.StatusStarting)
	id := e.settledID(name)
	deadline := time.Now().Add(testTimeout)
	for e.b.snapshot(name, id).Ref == "" {
		if time.Now().After(deadline) {
			t.Fatal("the create never recorded its instance")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	if r := e.storedRecord(name); r.Shed.User != "" || r.Ready {
		t.Fatalf("test set-up: record user %q ready %v, want an unfinished create with no user", r.Shed.User, r.Ready)
	}
	e.setUserLabel(name, "alice")
	h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }
	e.b = e.open()
	e.auto()
	if got := e.settled(name); got.Status != config.StatusRunning || got.ID != id {
		t.Fatalf("came to rest %q (%q), want running", got.Status, got.StatusReason)
	}
	if got := e.userOf(name); got != "alice" {
		t.Errorf("user after the resumed create = %q, want alice from the label", got)
	}
}

// A create resumed after a restart whose instance had in fact finished
// (labelled ready) and has stopped since is recorded as a finished shed
// without a probe. That read of the instance heals the record's user too.
func TestResumedFinishedCreateReconcilesTheUserLabel(t *testing.T) {
	h := &hookProvider{}
	e := newEnv(t, manual(), hooked(h))
	const name = "finished"
	e.put(name, config.StatusStarting)
	id := e.settledID(name)
	deadline := time.Now().Add(testTimeout)
	for e.b.snapshot(name, id).Ref == "" {
		if time.Now().After(deadline) {
			t.Fatal("the create never recorded its instance")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.b.Close(); err != nil {
		t.Fatal(err)
	}
	ref := e.ref(name)
	e.prov.Complete(ref)
	if err := e.prov.SetLabels(context.Background(), ref, map[string]string{
		provider.LabelState: string(provider.ShedStateReady),
		provider.LabelUser:  "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.prov.ExternalStop(ref); err != nil {
		t.Fatal(err)
	}
	h.list = func(context.Context, provider.Selector) ([]provider.Instance, error) { return nil, nil }
	e.b = e.open()
	if got := e.settled(name); got.Status != config.StatusStopped || got.ID != id {
		t.Fatalf("came to rest %q (%q), want stopped", got.Status, got.StatusReason)
	}
	if got := e.userOf(name); got != "alice" {
		t.Errorf("user after the resumed finished create = %q, want alice from the label", got)
	}
	if r := e.storedRecord(name); r.Shed.User != "alice" {
		t.Errorf("stored user = %q, want alice", r.Shed.User)
	}
}

// A periodic pass reads every instance it lists, and a listed instance whose
// state matches its record needs no correction; its shed_user label is still
// a read of the shed's user. A label that changes on a shed that keeps
// running (or stays stopped) is reconciled at the next pass: a service
// client's shed labelled with a user stops reaching that client, and the
// conflict clears at the pass after the label is corrected.
func TestSweepReconcilesTheUserLabelOfAnUnchangedShed(t *testing.T) {
	e := newEnv(t)
	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "theirs", NoProvision: true, Owner: "nocturn"}); err != nil {
		t.Fatal(err)
	}
	e.settled("theirs")
	e.createFor("mine", "alice")
	if _, err := e.b.StopShed(ctxT(t), "mine"); err != nil {
		t.Fatal(err)
	}
	if got := e.settled("mine").Status; got != config.StatusStopped {
		t.Fatalf("test set-up: mine settled %q, want stopped", got)
	}
	clientCtx := backend.WithRequiredOwner(context.Background(), "nocturn")

	e.setUserLabel("theirs", "alice")
	e.setUserLabel("mine", "bob")
	e.sweep()
	e.idle()
	if got := e.status("theirs"); got != config.StatusRunning {
		t.Fatalf("test set-up: the pass changed theirs to %q; it must find nothing to correct", got)
	}
	if got := e.status("mine"); got != config.StatusStopped {
		t.Fatalf("test set-up: the pass changed mine to %q; it must find nothing to correct", got)
	}
	if _, err := e.b.GetShed(clientCtx, "theirs"); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("the service client reaches its shed labelled with a user after a pass: %v, want not found", err)
	}
	if got := e.userOf("theirs"); got != config.UserUnresolved {
		t.Errorf("theirs after a pass: user %q, want %q", got, config.UserUnresolved)
	}
	if got := e.userOf("mine"); got != config.UserUnresolved {
		t.Errorf("mine after a pass: user %q, want %q", got, config.UserUnresolved)
	}
	if r := e.storedRecord("theirs"); r.UserConflict != "alice" {
		t.Errorf("theirs: stored conflict %q, want alice", r.UserConflict)
	}
	if r := e.storedRecord("mine"); r.Shed.User != "alice" || r.UserConflict != "bob" {
		t.Errorf("mine: stored user %q conflict %q, want alice and bob", r.Shed.User, r.UserConflict)
	}

	e.setUserLabel("theirs", "")
	e.setUserLabel("mine", "alice")
	e.sweep()
	e.idle()
	if _, err := e.b.GetShed(clientCtx, "theirs"); err != nil {
		t.Errorf("the service client after its label was corrected: %v", err)
	}
	if got := e.userOf("mine"); got != "alice" {
		t.Errorf("mine after its label was corrected: user %q, want alice", got)
	}
	for _, name := range []string{"theirs", "mine"} {
		if r := e.storedRecord(name); r.UserConflict != "" {
			t.Errorf("%s: stored conflict after the label was corrected = %q, want none", name, r.UserConflict)
		}
	}
}

// A probe that finds the shed's instance stopped records it stopped, and that
// read of the instance reconciles the user label too.
func TestStoppedProbeReconcilesTheUserLabel(t *testing.T) {
	e := newEnv(t)
	const name = "mine"
	e.createFor(name, "alice")
	id := e.settledID(name)
	if err := e.prov.ExternalStop(e.ref(name)); err != nil {
		t.Fatal(err)
	}
	e.setUserLabel(name, "bob")

	// The probe of a shed recorded as running on its way (as the start-up
	// pass or a periodic correction leaves it), run to its end.
	e.b.mu.Lock()
	r := e.b.currentLocked(name, id)
	r.Shed.Status = config.StatusStarting
	r.Job = state.JobProbe
	e.b.saveLocked(r)
	j := e.b.newJobLocked(state.JobProbe, name, id)
	e.b.mu.Unlock()
	e.b.runJob(j)

	if got := e.status(name); got != config.StatusStopped {
		t.Fatalf("after the probe: %q, want stopped", got)
	}
	if got := e.userOf(name); got != config.UserUnresolved {
		t.Errorf("user after a probe found the label naming bob = %q, want %q", got, config.UserUnresolved)
	}
	if r := e.storedRecord(name); r.Shed.User != "alice" || r.UserConflict != "bob" {
		t.Errorf("stored record: user %q conflict %q, want alice and bob", r.Shed.User, r.UserConflict)
	}
}

// readHooks lets a test step into a stop job's reads of its instance.
type readHooks struct {
	*fake.Provider
	get  func(ctx context.Context, ref provider.Ref) (provider.Instance, error)
	stop func(ctx context.Context, ref provider.Ref) (provider.Pending, error)
}

func (h *readHooks) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	if h.get != nil {
		return h.get(ctx, ref)
	}
	return h.Provider.Get(ctx, ref)
}

func (h *readHooks) Stop(ctx context.Context, ref provider.Ref) (provider.Pending, error) {
	if h.stop != nil {
		return h.stop(ctx, ref)
	}
	return h.Provider.Stop(ctx, ref)
}

func withReadHooks(h *readHooks) envOpt {
	return func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			h.Provider = p.(*fake.Provider)
			return h
		}
	}
}

// A stop job reads the shed's instance three ways: the verification read
// before the stop call, the instance the wait returns, and, when the stop
// fails, a read of what the instance is doing now. Each reconciles the user
// label. The label is changed to name bob at the point under test, after any
// earlier read, and every later read is made to fail or never happens.
func TestStopJobReconcilesTheUserLabel(t *testing.T) {
	stopFailed := errors.New("the stop call was turned down")
	for _, tt := range []struct {
		name string
		arm  func(e *env, h *readHooks)
	}{
		{"the verification read", func(e *env, h *readHooks) {
			e.setUserLabel("mine", "bob")
			gets := 0
			h.get = func(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
				gets++
				if gets > 1 { // the failed stop's read of the instance
					return provider.Instance{}, errors.New("the cloud could not be reached")
				}
				return h.Provider.Get(ctx, ref)
			}
			h.stop = func(context.Context, provider.Ref) (provider.Pending, error) { return provider.Pending{}, stopFailed }
		}},
		{"the instance the wait returns", func(e *env, h *readHooks) {
			h.stop = func(ctx context.Context, ref provider.Ref) (provider.Pending, error) {
				e.setUserLabel("mine", "bob")
				return h.Provider.Stop(ctx, ref)
			}
		}},
		{"the read after a failed stop", func(e *env, h *readHooks) {
			h.stop = func(context.Context, provider.Ref) (provider.Pending, error) {
				e.setUserLabel("mine", "bob")
				return provider.Pending{}, stopFailed
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &readHooks{}
			e := newEnv(t, withReadHooks(h))
			e.createFor("mine", "alice")
			tt.arm(e, h)
			if _, err := e.b.StopShed(ctxT(t), "mine"); err != nil {
				t.Fatal(err)
			}
			e.idle() // a stop whose failure could not be read stays "stopping"
			if got := e.userOf("mine"); got != config.UserUnresolved {
				t.Errorf("user after the stop found the label naming bob = %q, want %q", got, config.UserUnresolved)
			}
			if r := e.storedRecord("mine"); r.Shed.User != "alice" || r.UserConflict != "bob" {
				t.Errorf("stored record: user %q conflict %q, want alice and bob", r.Shed.User, r.UserConflict)
			}
		})
	}
}

// TestRemoteShedsJoinTheBusAsTheirUser: a remote shed is registered on the
// credential bus with the user and incarnation of the server's own record,
// and a service client's shed stays off it.
func TestRemoteShedsJoinTheBusAsTheirUser(t *testing.T) {
	bridge := plugin.NewBridge(plugin.NewRegistry(), true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })

	e.createFor("box", "alice")
	box := e.settled("box")
	if u, ok := bridge.ShedUser("box"); !ok || u != "alice" {
		t.Fatalf("ShedUser(box) = (%q, %v), want alice", u, ok)
	}
	if u, ok := bridge.IncarnationUser("box", box.CreatedAt); !ok || u != "alice" {
		t.Fatalf("IncarnationUser(box) = (%q, %v), want alice", u, ok)
	}

	if _, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "svc", NoProvision: true, Owner: "nocturn"}); err != nil {
		t.Fatal(err)
	}
	e.settled("svc")
	if u, ok := bridge.ShedUser("svc"); ok {
		t.Fatalf("a service client's shed is on the bus as %q", u)
	}
}

// TestRemoteBusIdentity: the bus identity of a remote record is the reported
// one: a stored owner keeps it off the bus, and a record whose instance
// label names another user routes nowhere (unresolved).
func TestRemoteBusIdentity(t *testing.T) {
	tests := []struct {
		name string
		rec  state.Record
		want vmutil.BusIdentity
	}{
		{"user", state.Record{Shed: config.Shed{User: "alice", CreatedAt: time.Unix(1, 0)}}, vmutil.BusIdentity{User: "alice", CreatedAt: time.Unix(1, 0)}},
		{"client", state.Record{Shed: config.Shed{Owner: "nocturn"}}, vmutil.BusIdentity{Owned: true}},
		{"both", state.Record{Shed: config.Shed{Owner: "nocturn", User: "alice"}}, vmutil.BusIdentity{Owned: true, User: config.UserUnresolved}},
		{"label conflict", state.Record{Shed: config.Shed{User: "alice"}, UserConflict: "bob"}, vmutil.BusIdentity{User: config.UserUnresolved}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := busIdentity(&tc.rec)
			if got.Owned != tc.want.Owned || got.User != tc.want.User || !got.CreatedAt.Equal(tc.want.CreatedAt) {
				t.Fatalf("busIdentity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// waitShedUser waits until the bridge routes the running shed name as want
// (ok false: routed nowhere).
func waitShedUser(t *testing.T, bridge *plugin.Bridge, name, want string, wantOK bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u, ok := bridge.ShedUser(name)
		if u == want && ok == wantOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge routes %s as (%q, %v), want (%q, %v)", name, u, ok, want, wantOK)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRemoteBusRouteFollowsTheReportedUser: a running shed's bus route
// follows every change of its reported ownership that a read of its instance
// makes. A label naming another user makes it unresolved, so its requests
// stop going to the previous user's machine; the label put back in line
// routes it to its user again.
func TestRemoteBusRouteFollowsTheReportedUser(t *testing.T) {
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })
	e.createFor("box", "alice")
	waitShedUser(t, bridge, "box", "alice", true)

	e.setUserLabel("box", "bob")
	e.sweep()
	e.idle()
	if got := e.userOf("box"); got != config.UserUnresolved {
		t.Fatalf("test set-up: box reported as %q, want %q", got, config.UserUnresolved)
	}
	waitShedUser(t, bridge, "box", "", false)

	e.setUserLabel("box", "alice")
	e.sweep()
	e.idle()
	waitShedUser(t, bridge, "box", "alice", true)
}
