package netpolicy

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// fakeExit is what the fake returns for an nft that ran and failed (a missing
// table on `list`).
type fakeExit struct{}

func (fakeExit) Error() string { return "exit status 1" }
func (fakeExit) ExitCode() int { return 1 }

type fakeCall struct {
	args  []string
	stdin string
}

// fakeRunner records every call. It keeps the "kernel" state of this
// server's tables: applying a full script installs them, the teardown script
// removes them, and list answers from that state.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []fakeCall
	missing   bool  // nft is not installed
	applyErr  error // returned by -f
	installed bool
}

func (f *fakeRunner) Available() bool { return !f.missing }

func (f *fakeRunner) Run(_ context.Context, stdin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{args: args, stdin: stdin})
	if f.missing {
		return nil, exec.ErrNotFound
	}
	if len(args) > 0 && args[0] == "list" {
		if f.installed {
			return []byte("table"), nil
		}
		return []byte("Error: No such file or directory"), fakeExit{}
	}
	if f.applyErr != nil {
		return []byte("Error: boom"), f.applyErr
	}
	f.installed = strings.Contains(stdin, "{")
	return nil, nil
}

func (f *fakeRunner) applies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "-f" {
			out = append(out, c.stdin)
		}
	}
	return out
}

func (f *fakeRunner) lastApply(t *testing.T) string {
	t.Helper()
	a := f.applies()
	if len(a) == 0 {
		t.Fatal("no apply")
	}
	return a[len(a)-1]
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func guarded(name, tap, ip string) Shed {
	return Shed{Name: name, TAP: tap, MAC: "02:fc:00:00:00:02", IP: ip, Profile: Isolated}
}

func TestManagerEveryChangeAppliesTheWholeSet(t *testing.T) {
	f := &fakeRunner{}
	m := NewManager(testConfig(), f, Open)
	ctx := context.Background()

	if m.Needed() {
		t.Fatal("needed with nothing guarded and an open default")
	}
	if err := m.Upsert(ctx, guarded("a", "shed-tap-0", "172.30.0.2")); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(ctx, guarded("b", "shed-tap-1", "172.30.0.3")); err != nil {
		t.Fatal(err)
	}
	last := f.lastApply(t)
	// The second apply carries both sheds and replaces both tables.
	for _, want := range []string{"172.30.0.2", "172.30.0.3", "delete table bridge shed_mini3_dev", "delete table inet shed_mini3_dev"} {
		if !strings.Contains(last, want) {
			t.Errorf("second apply lacks %q", want)
		}
	}
	if !m.Needed() || !m.IsGuarded("a") {
		t.Error("state")
	}

	// Ports: 9P and egress land in host_allow; unchanged ports apply nothing.
	if err := m.SetP9Ports(ctx, "a", []uint16{41003, 41001}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastApply(t), "172.30.0.2 . 41001, 172.30.0.2 . 41003") {
		t.Errorf("9P ports not in host_allow:\n%s", f.lastApply(t))
	}
	n := len(f.applies())
	if err := m.SetP9Ports(ctx, "a", []uint16{41001, 41003}); err != nil {
		t.Fatal(err)
	}
	if len(f.applies()) != n {
		t.Error("an unchanged port set re-applied")
	}
	if err := m.SetEgressPort(ctx, "a", 40001); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastApply(t), "172.30.0.2 . 40001") {
		t.Error("egress port not in host_allow")
	}
	if err := m.SetP9Ports(ctx, "a", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.lastApply(t), "41001") || !strings.Contains(f.lastApply(t), "40001") {
		t.Error("closing the 9P servers should drop only their ports")
	}

	// An open shed's ports change nothing at all.
	n = f.count()
	if err := m.SetP9Ports(ctx, "open-one", []uint16{1}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEgressPort(ctx, "open-one", 2); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "open-one"); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(ctx, Shed{Name: "open-one", Profile: Open}); err != nil {
		t.Fatal(err)
	}
	if f.count() != n {
		t.Errorf("open shed caused %d nft calls", f.count()-n)
	}

	// Removing the last guarded shed with an open default deletes the tables.
	if err := m.Remove(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.lastApply(t), "172.30.0.2") {
		t.Error("removed shed still applied")
	}
	if err := m.Remove(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if got := f.lastApply(t); got != RenderTeardown("mini3-dev") {
		t.Errorf("last apply = %q, want the teardown", got)
	}
	if m.Needed() {
		t.Error("still needed")
	}
}

func TestManagerErrorsCarryTheSentinel(t *testing.T) {
	f := &fakeRunner{applyErr: errors.New("exit status 1")}
	m := NewManager(testConfig(), f, Open)
	err := m.Upsert(context.Background(), guarded("a", "shed-tap-0", "172.30.0.2"))
	if !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Fatalf("err = %v, want the enforcement sentinel", err)
	}
	// The desired state keeps the shed, so a later apply installs it.
	if !m.IsGuarded("a") {
		t.Error("failed apply dropped the shed from the desired state")
	}
	f.applyErr = nil
	if err := m.Reapply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastApply(t), "172.30.0.2") {
		t.Error("reapply lacks the shed")
	}

	bad := NewManager(Config{ServerName: "x", Bridge: "br0", Gateway: "nope"}, &fakeRunner{}, Isolated)
	if err := bad.Reapply(context.Background()); !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Errorf("render error = %v, want the sentinel", err)
	}
}

func TestManagerMissingNFT(t *testing.T) {
	ctx := context.Background()

	// An open-only server with no nft makes no calls at all.
	f := &fakeRunner{missing: true}
	m := NewManager(testConfig(), f, Open)
	if err := m.Rebuild(ctx, []Shed{{Name: "o", Profile: Open}}); err != nil {
		t.Fatalf("open-only rebuild: %v", err)
	}
	if err := m.Teardown(ctx); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if f.count() != 0 {
		t.Errorf("open-only server with no nft made %d calls", f.count())
	}

	// A guarded shed needs nft: the error carries the sentinel.
	err := m.Upsert(ctx, guarded("a", "shed-tap-0", "172.30.0.2"))
	if !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) || !strings.Contains(err.Error(), "nft is not installed") {
		t.Errorf("err = %v", err)
	}
	// So does a non-open default with nothing guarded yet.
	m2 := NewManager(testConfig(), &fakeRunner{missing: true}, Isolated)
	if err := m2.Rebuild(ctx, nil); !errors.Is(err, config.ErrNetworkEnforcementUnavailableSentinel) {
		t.Errorf("isolated default without nft: %v", err)
	}
	if _, err := m2.Present(ctx); err == nil {
		t.Error("Present without nft should be an error")
	}
}

func TestManagerRebuild(t *testing.T) {
	ctx := context.Background()

	// Guarded sheds (a stopped one is no different) are applied.
	f := &fakeRunner{}
	m := NewManager(testConfig(), f, Open)
	if err := m.Rebuild(ctx, []Shed{guarded("a", "shed-tap-0", "172.30.0.2"), {Name: "o", Profile: Open}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastApply(t), "172.30.0.2") || !f.installed {
		t.Error("rebuild did not apply")
	}
	ok, err := m.Present(ctx)
	if err != nil || !ok {
		t.Errorf("Present = %v, %v", ok, err)
	}

	// Nothing needed and stale tables present: they are deleted.
	if err := m.Rebuild(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if f.lastApply(t) != RenderTeardown("mini3-dev") || f.installed {
		t.Error("stale tables not deleted")
	}

	// Nothing needed and nothing present: only reads.
	n := len(f.applies())
	if err := m.Rebuild(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.applies()) != n {
		t.Error("rebuild with nothing to do applied")
	}

	// An isolated default needs the tables with no guarded shed.
	f2 := &fakeRunner{}
	m2 := NewManager(testConfig(), f2, Isolated)
	if !m2.Needed() {
		t.Error("isolated default not needed")
	}
	if err := m2.Rebuild(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !f2.installed {
		t.Error("isolated default did not install the tables")
	}
}

func TestManagerTeardownTouchesOnlyItsTables(t *testing.T) {
	f := &fakeRunner{}
	m := NewManager(testConfig(), f, Open)
	if err := m.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.lastApply(t)
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.HasSuffix(line, " shed_mini3_dev") {
			t.Errorf("teardown line %q is not about this server's tables", line)
		}
	}
}

func TestExecRunnerArgs(t *testing.T) {
	// The prefix is placed before the nft arguments, and stdin reaches the
	// process (cat echoes it back).
	r := ExecRunner{Bin: "sh", Prefix: []string{"-c", `printf '%s|' "$@"; cat`, "sh"}}
	if !r.Available() {
		t.Skip("no sh")
	}
	out, err := r.Run(context.Background(), "script", "-f", "-")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "-f|-|script" {
		t.Errorf("out = %q", out)
	}
	if (ExecRunner{Bin: "/no/such/nft-binary"}).Available() {
		t.Error("missing binary reported available")
	}
}
