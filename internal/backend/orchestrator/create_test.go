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

type mockUpper struct {
	path string
	size int64
}

func (u mockUpper) Path() string     { return u.path }
func (u mockUpper) SizeBytes() int64 { return u.size }

type mockNet struct{ summary string }

func (n mockNet) Summary() string { return n.summary }

type mockMeta struct{ name string }

func (m mockMeta) Name() string { return m.name }

type mockVM struct{ backend string }

func (v mockVM) Backend() string { return v.backend }

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

func (b *mockBackend) Name() string { return config.BackendVZ }

func (b *mockBackend) PreFlight(ctx context.Context, req config.CreateShedRequest) (PreFlightResult, error) {
	b.record("PreFlight")
	if err := b.failAt["PreFlight"]; err != nil {
		return nil, err
	}
	return mockPreFlight{}, nil
}

func (b *mockBackend) AllocateUpper(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, c *backend.Cleanup) (UpperInfo, error) {
	b.record("AllocateUpper")
	if err := b.failAt["AllocateUpper"]; err != nil {
		return nil, err
	}
	c.Register("delete upper", b.makeCleanup("delete upper"))
	return mockUpper{path: "/tmp/test-upper", size: 1 << 30}, nil
}

func (b *mockBackend) AllocateNetwork(ctx context.Context, req config.CreateShedRequest, c *backend.Cleanup) (NetworkResources, error) {
	b.record("AllocateNetwork")
	if err := b.failAt["AllocateNetwork"]; err != nil {
		return nil, err
	}
	c.Register("release network", b.makeCleanup("release network"))
	return mockNet{summary: "mock-net"}, nil
}

func (b *mockBackend) BuildAndPersistMetadata(ctx context.Context, req config.CreateShedRequest, pre PreFlightResult, upper UpperInfo, net NetworkResources, c *backend.Cleanup) (MetadataHandle, error) {
	b.record("BuildAndPersistMetadata")
	// Register BEFORE the "save would create a partial dir" — same
	// invariant as the real backends (see PR #137 review).
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
	return mockVM{backend: config.BackendVZ}, nil
}

func (b *mockBackend) FinalizeVM(ctx context.Context, meta MetadataHandle, vm VMHandle, c *backend.Cleanup) error {
	b.record("FinalizeVM")
	if err := b.failAt["FinalizeVM"]; err != nil {
		return err
	}
	c.Register("remove from vms map", b.makeCleanup("remove from vms map"))
	return nil
}

func (b *mockBackend) MountWorkspace(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) error {
	b.record("MountWorkspace")
	return b.failAt["MountWorkspace"]
}

func (b *mockBackend) SetupCredentials(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) {
	b.record("SetupCredentials")
}

func (b *mockBackend) CloneRepo(ctx context.Context, req config.CreateShedRequest, meta MetadataHandle, vm VMHandle) error {
	b.record("CloneRepo")
	return b.failAt["CloneRepo"]
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
		"AllocateUpper",
		"AllocateNetwork",
		"BuildAndPersistMetadata",
		"StartVM",
		"FinalizeVM",
		"MountWorkspace",
		"SetupCredentials",
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
func TestCreateShed_FailureUnwindLIFO(t *testing.T) {
	cases := []struct {
		failAt       string
		wantCalled   []string
		wantCleanups []string
	}{
		{
			failAt:       "PreFlight",
			wantCalled:   []string{"PreFlight"},
			wantCleanups: nil, // nothing registered yet
		},
		{
			failAt:       "AllocateUpper",
			wantCalled:   []string{"PreFlight", "AllocateUpper"},
			wantCleanups: nil, // AllocateUpper failed before registering
		},
		{
			failAt:       "AllocateNetwork",
			wantCalled:   []string{"PreFlight", "AllocateUpper", "AllocateNetwork"},
			wantCleanups: []string{"delete upper"},
		},
		{
			failAt:     "BuildAndPersistMetadata",
			wantCalled: []string{"PreFlight", "AllocateUpper", "AllocateNetwork", "BuildAndPersistMetadata"},
			// Cleanup for delete-instance-dir IS registered before the
			// fail-injection point (mock mirrors the real backend's
			// "register before Save" invariant from PR #137).
			wantCleanups: []string{"delete instance dir", "release network", "delete upper"},
		},
		{
			failAt:       "StartVM",
			wantCalled:   []string{"PreFlight", "AllocateUpper", "AllocateNetwork", "BuildAndPersistMetadata", "StartVM"},
			wantCleanups: []string{"delete instance dir", "release network", "delete upper"},
		},
		{
			failAt:       "FinalizeVM",
			wantCalled:   []string{"PreFlight", "AllocateUpper", "AllocateNetwork", "BuildAndPersistMetadata", "StartVM", "FinalizeVM"},
			wantCleanups: []string{"stop VM", "delete instance dir", "release network", "delete upper"},
		},
		{
			failAt: "MountWorkspace",
			wantCalled: []string{
				"PreFlight", "AllocateUpper", "AllocateNetwork", "BuildAndPersistMetadata",
				"StartVM", "FinalizeVM", "MountWorkspace",
			},
			wantCleanups: []string{"remove from vms map", "stop VM", "delete instance dir", "release network", "delete upper"},
		},
		{
			failAt: "CloneRepo",
			wantCalled: []string{
				"PreFlight", "AllocateUpper", "AllocateNetwork", "BuildAndPersistMetadata",
				"StartVM", "FinalizeVM", "MountWorkspace", "SetupCredentials", "CloneRepo",
			},
			wantCleanups: []string{"remove from vms map", "stop VM", "delete instance dir", "release network", "delete upper"},
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
