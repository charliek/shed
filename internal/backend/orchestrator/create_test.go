// Mock-backed tests for orchestrator.CreateShed. These pin the
// orchestrator's call ordering, error-propagation, and cleanup-LIFO
// behavior without touching either real backend — a regression in
// CreateShed's shape (a hook called in the wrong order, a failure
// that doesn't unwind, a success that doesn't commit) shows up as a
// test failure here.

package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// ---------------------------------------------------------------------------
// Mock fixtures
// ---------------------------------------------------------------------------

type mockPreFlight struct{ fromSnapshot bool }

func (p mockPreFlight) IsFromSnapshot() bool { return p.fromSnapshot }

type mockUpper struct{}

type mockNet struct{}

type mockMeta struct{ name string }

func (m mockMeta) Name() string { return m.name }

type mockVM struct{}

// mockBackend records every hook invocation in order and exposes
// failure-injection knobs so each error-propagation path is testable
// independently. Default behavior is a clean happy path that registers
// the cleanups a real backend would.
type mockBackend struct {
	// Recorded ordered list of hook names called (in call order).
	calls []string

	// Injected errors keyed by hook name.
	failAt map[string]error

	// What the mock's hooks Register on cleanup.
	cleanupsRun []string
}

func newMockBackend() *mockBackend {
	return &mockBackend{failAt: map[string]error{}}
}

func (b *mockBackend) record(name string) { b.calls = append(b.calls, name) }

// makeCleanup returns a closure that, when invoked by Cleanup.Run,
// appends a marker so the test can assert LIFO ordering.
func (b *mockBackend) makeCleanup(name string) func() error {
	return func() error {
		b.cleanupsRun = append(b.cleanupsRun, name)
		return nil
	}
}

func (b *mockBackend) PreFlight(ctx context.Context, req config.CreateShedRequest, c *backend.Cleanup) (PreFlightResult, error) {
	b.record("PreFlight")
	if err := b.failAt["PreFlight"]; err != nil {
		return nil, err
	}
	// PreFlight may register protective cleanups (the .creating marker
	// removal in real backends); the mock simulates one to exercise
	// the early-stage unwind path.
	c.Register("remove creating marker", b.makeCleanup("remove creating marker"))
	return mockPreFlight{}, nil
}

func (b *mockBackend) AllocateNetwork(ctx context.Context, req config.CreateShedRequest, c *backend.Cleanup) (NetworkResources, error) {
	b.record("AllocateNetwork")
	if err := b.failAt["AllocateNetwork"]; err != nil {
		return nil, err
	}
	c.Register("release network", b.makeCleanup("release network"))
	return mockNet{}, nil
}

func (b *mockBackend) AllocateUpper(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, c *backend.Cleanup) (UpperInfo, error) {
	b.record("AllocateUpper")
	if err := b.failAt["AllocateUpper"]; err != nil {
		return nil, err
	}
	c.Register("delete upper", b.makeCleanup("delete upper"))
	return mockUpper{}, nil
}

func (b *mockBackend) BuildAndPersistMetadata(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, upper UpperInfo, net NetworkResources, c *backend.Cleanup) (MetadataHandle, error) {
	b.record("BuildAndPersistMetadata")
	// Register BEFORE the simulated meta.Save — same invariant as the
	// real backends (see PR #137 review): MkdirAll can leave a partial
	// dir behind that LIFO must still unwind.
	c.Register("delete instance dir", b.makeCleanup("delete instance dir"))
	if err := b.failAt["BuildAndPersistMetadata"]; err != nil {
		return nil, err
	}
	return mockMeta{name: req.Name}, nil
}

func (b *mockBackend) StartVM(ctx context.Context, meta MetadataHandle, upper UpperInfo, net NetworkResources, c *backend.Cleanup) (VMHandle, error) {
	b.record("StartVM")
	if err := b.failAt["StartVM"]; err != nil {
		return nil, err
	}
	c.Register("stop VM", b.makeCleanup("stop VM"))
	return mockVM{}, nil
}

func (b *mockBackend) FinalizeStartedVM(ctx context.Context, meta MetadataHandle, vm VMHandle, c *backend.Cleanup) error {
	b.record("FinalizeStartedVM")
	if err := b.failAt["FinalizeStartedVM"]; err != nil {
		return err
	}
	c.Register("remove from vms map", b.makeCleanup("remove from vms map"))
	return nil
}

func (b *mockBackend) MountLocalDir(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) error {
	b.record("MountLocalDir")
	return b.failAt["MountLocalDir"]
}

func (b *mockBackend) SetupCredentials(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) {
	b.record("SetupCredentials")
}

func (b *mockBackend) ConfigureEgressProxy(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle, c *backend.Cleanup) error {
	b.record("ConfigureEgressProxy")
	// Mirror the real hook: open the listener + register its teardown
	// BEFORE the fallible injection, so a failure inside the hook unwinds
	// its own listener along with every earlier cleanup.
	c.Register("close egress listener", b.makeCleanup("close egress listener"))
	return b.failAt["ConfigureEgressProxy"]
}

func (b *mockBackend) CloneRepo(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) {
	b.record("CloneRepo")
}

func (b *mockBackend) RunProvisioning(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) {
	b.record("RunProvisioning")
}

func (b *mockBackend) ToShedResult(meta MetadataHandle) *config.Shed {
	b.record("ToShedResult")
	return &config.Shed{Name: meta.Name(), Backend: config.BackendVZ}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestCreateShed_HappyPathOrder pins the hook call sequence. A regression
// that reorders a step or skips one shows up here.
func TestCreateShed_HappyPathOrder(t *testing.T) {
	b := newMockBackend()
	got, err := CreateShed(context.Background(), b, config.CreateShedRequest{Name: "x"})
	if err != nil {
		t.Fatalf("happy-path CreateShed returned error: %v", err)
	}
	if got == nil || got.Name != "x" {
		t.Fatalf("ToShedResult returned %v; want a Shed named x", got)
	}

	want := []string{
		"PreFlight",
		"AllocateNetwork",
		"AllocateUpper",
		"BuildAndPersistMetadata",
		"StartVM",
		"FinalizeStartedVM",
		"MountLocalDir",
		"SetupCredentials",
		"ConfigureEgressProxy",
		"CloneRepo",
		"RunProvisioning",
		"ToShedResult",
	}
	if !sliceEqual(b.calls, want) {
		t.Errorf("hook call order =\n  %v\nwant\n  %v", b.calls, want)
	}

	// Success path: cleanups NEVER ran (Commit zeroed the stack).
	if len(b.cleanupsRun) != 0 {
		t.Errorf("cleanups ran on success path: %v", b.cleanupsRun)
	}
}

// TestCreateShed_FailureUnwindLIFO injects a failure at each hook that
// returns an error and asserts the registered cleanups unwind in
// reverse order. The test name in the table identifies the failure point.
//
// Hooks that DON'T return an error (SetupCredentials, CloneRepo,
// RunProvisioning) are best-effort by design and not failable here.
func TestCreateShed_FailureUnwindLIFO(t *testing.T) {
	cases := []struct {
		failAt       string
		wantCalled   []string
		wantCleanups []string
	}{
		{
			failAt:       "PreFlight",
			wantCalled:   []string{"PreFlight"},
			wantCleanups: nil, // PreFlight failed before registering anything (mock registers AFTER fail check)
		},
		{
			failAt:       "AllocateNetwork",
			wantCalled:   []string{"PreFlight", "AllocateNetwork"},
			wantCleanups: []string{"remove creating marker"},
		},
		{
			failAt:       "AllocateUpper",
			wantCalled:   []string{"PreFlight", "AllocateNetwork", "AllocateUpper"},
			wantCleanups: []string{"release network", "remove creating marker"},
		},
		{
			failAt:     "BuildAndPersistMetadata",
			wantCalled: []string{"PreFlight", "AllocateNetwork", "AllocateUpper", "BuildAndPersistMetadata"},
			// "delete instance dir" IS registered before the simulated
			// save (mock mirrors the real backend's "register before
			// Save" invariant from PR #137).
			wantCleanups: []string{"delete instance dir", "delete upper", "release network", "remove creating marker"},
		},
		{
			failAt:       "StartVM",
			wantCalled:   []string{"PreFlight", "AllocateNetwork", "AllocateUpper", "BuildAndPersistMetadata", "StartVM"},
			wantCleanups: []string{"delete instance dir", "delete upper", "release network", "remove creating marker"},
		},
		{
			failAt:       "FinalizeStartedVM",
			wantCalled:   []string{"PreFlight", "AllocateNetwork", "AllocateUpper", "BuildAndPersistMetadata", "StartVM", "FinalizeStartedVM"},
			wantCleanups: []string{"stop VM", "delete instance dir", "delete upper", "release network", "remove creating marker"},
		},
		{
			failAt: "MountLocalDir",
			wantCalled: []string{
				"PreFlight", "AllocateNetwork", "AllocateUpper", "BuildAndPersistMetadata",
				"StartVM", "FinalizeStartedVM", "MountLocalDir",
			},
			wantCleanups: []string{"remove from vms map", "stop VM", "delete instance dir", "delete upper", "release network", "remove creating marker"},
		},
		{
			// Failable egress hook: SetupCredentials (best-effort) ran
			// first and registered nothing; the egress hook registered its
			// own "close egress listener" before failing, so it unwinds
			// FIRST (LIFO), then every earlier resource cleanup.
			failAt: "ConfigureEgressProxy",
			wantCalled: []string{
				"PreFlight", "AllocateNetwork", "AllocateUpper", "BuildAndPersistMetadata",
				"StartVM", "FinalizeStartedVM", "MountLocalDir", "SetupCredentials", "ConfigureEgressProxy",
			},
			wantCleanups: []string{"close egress listener", "remove from vms map", "stop VM", "delete instance dir", "delete upper", "release network", "remove creating marker"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.failAt, func(t *testing.T) {
			b := newMockBackend()
			b.failAt[tc.failAt] = errors.New("injected failure")
			_, err := CreateShed(context.Background(), b, config.CreateShedRequest{Name: "fail-" + tc.failAt})
			if err == nil {
				t.Fatal("expected error from injected failure")
			}
			if !strings.Contains(err.Error(), "injected failure") {
				t.Errorf("error %q does not contain the injected message", err)
			}
			if !sliceEqual(b.calls, tc.wantCalled) {
				t.Errorf("hook calls =\n  %v\nwant\n  %v", b.calls, tc.wantCalled)
			}
			if !sliceEqual(b.cleanupsRun, tc.wantCleanups) {
				t.Errorf("cleanup unwind order =\n  %v\nwant (LIFO)\n  %v", b.cleanupsRun, tc.wantCleanups)
			}
		})
	}
}

// TestCreateShed_BestEffortHooksDoNotAbort verifies that hooks marked
// best-effort (SetupCredentials, CloneRepo, RunProvisioning) cannot
// abort the create. The mock's best-effort hooks don't return errors
// to inject — this test would catch a regression where someone made
// them return error AND the orchestrator started propagating.
func TestCreateShed_BestEffortHooksDoNotAbort(t *testing.T) {
	b := newMockBackend()
	got, err := CreateShed(context.Background(), b, config.CreateShedRequest{Name: "best-effort"})
	if err != nil {
		t.Fatalf("CreateShed returned error from best-effort path: %v", err)
	}
	if got == nil || got.Name != "best-effort" {
		t.Fatalf("ToShedResult returned %v; want a Shed", got)
	}
	// All three best-effort hooks must have been called (the failable
	// egress hook sits between SetupCredentials and CloneRepo and is a
	// happy-path no-op here).
	wantTail := []string{"SetupCredentials", "ConfigureEgressProxy", "CloneRepo", "RunProvisioning", "ToShedResult"}
	gotTail := b.calls[len(b.calls)-len(wantTail):]
	if !sliceEqual(gotTail, wantTail) {
		t.Errorf("best-effort tail = %v, want %v", gotTail, wantTail)
	}
}

// sliceEqual compares two string slices for full equality (including
// length 0 vs nil treated as equal).
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
