// Mock-backed tests for orchestrator.StartShed. Mirrors create_test.go
// in shape (mockStarter + happy/failure/best-effort tables) and pins
// the StartShed-specific contract: hook call order, LIFO unwind on
// failure, best-effort hooks cannot abort the start.

package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// mockStarter mirrors mockBackend's shape but implements
// BackendStarter. Reuses mockMeta + mockVM from create_test.go since
// they're already orchestrator-opaque.
type mockStarter struct {
	calls       []string
	failAt      map[string]error
	cleanupsRun []string
}

func newMockStarter() *mockStarter {
	return &mockStarter{failAt: map[string]error{}}
}

func (b *mockStarter) record(name string) { b.calls = append(b.calls, name) }

func (b *mockStarter) makeCleanup(name string) func() error {
	return func() error {
		b.cleanupsRun = append(b.cleanupsRun, name)
		return nil
	}
}

func (b *mockStarter) LoadMetadata(ctx context.Context, req StartRequest) (MetadataHandle, error) {
	b.record("LoadMetadata")
	if err := b.failAt["LoadMetadata"]; err != nil {
		return nil, err
	}
	return mockMeta{name: req.Name}, nil
}

func (b *mockStarter) CheckNotRunning(ctx context.Context, meta MetadataHandle) error {
	b.record("CheckNotRunning")
	return b.failAt["CheckNotRunning"]
}

func (b *mockStarter) StartVM(ctx context.Context, meta MetadataHandle, c *backend.Cleanup) (VMHandle, error) {
	b.record("StartVM")
	if err := b.failAt["StartVM"]; err != nil {
		return nil, err
	}
	c.Register("stop VM", b.makeCleanup("stop VM"))
	return mockVM{}, nil
}

func (b *mockStarter) PersistRunningState(ctx context.Context, meta MetadataHandle, vm VMHandle, c *backend.Cleanup) error {
	b.record("PersistRunningState")
	if err := b.failAt["PersistRunningState"]; err != nil {
		return err
	}
	c.Register("remove from vms map", b.makeCleanup("remove from vms map"))
	return nil
}

func (b *mockStarter) MountLocalDir(ctx context.Context, meta MetadataHandle, vm VMHandle) error {
	b.record("MountLocalDir")
	return b.failAt["MountLocalDir"]
}

func (b *mockStarter) SetupCredentials(ctx context.Context, meta MetadataHandle, vm VMHandle) {
	b.record("SetupCredentials")
}

func (b *mockStarter) RunStartupHook(ctx context.Context, meta MetadataHandle, vm VMHandle) {
	b.record("RunStartupHook")
}

func (b *mockStarter) ToShedResult(meta MetadataHandle) *config.Shed {
	b.record("ToShedResult")
	return &config.Shed{Name: meta.Name(), Backend: config.BackendVZ}
}

// TestStartShed_HappyPathOrder pins the hook call sequence.
func TestStartShed_HappyPathOrder(t *testing.T) {
	b := newMockStarter()
	got, err := StartShed(context.Background(), b, StartRequest{Name: "x"})
	if err != nil {
		t.Fatalf("happy-path StartShed returned error: %v", err)
	}
	if got == nil || got.Name != "x" {
		t.Fatalf("ToShedResult returned %v; want a Shed named x", got)
	}

	want := []string{
		"LoadMetadata",
		"CheckNotRunning",
		"StartVM",
		"PersistRunningState",
		"MountLocalDir",
		"SetupCredentials",
		"RunStartupHook",
		"ToShedResult",
	}
	if !sliceEqual(b.calls, want) {
		t.Errorf("hook call order =\n  %v\nwant\n  %v", b.calls, want)
	}

	if len(b.cleanupsRun) != 0 {
		t.Errorf("cleanups ran on success path: %v", b.cleanupsRun)
	}
}

// TestStartShed_FailureUnwindLIFO injects a failure at each
// error-returning hook and verifies the cleanup stack unwinds in
// reverse-registration order.
func TestStartShed_FailureUnwindLIFO(t *testing.T) {
	cases := []struct {
		failAt       string
		wantCalled   []string
		wantCleanups []string
	}{
		{
			failAt:       "LoadMetadata",
			wantCalled:   []string{"LoadMetadata"},
			wantCleanups: nil,
		},
		{
			failAt:       "CheckNotRunning",
			wantCalled:   []string{"LoadMetadata", "CheckNotRunning"},
			wantCleanups: nil,
		},
		{
			failAt:       "StartVM",
			wantCalled:   []string{"LoadMetadata", "CheckNotRunning", "StartVM"},
			wantCleanups: nil,
		},
		{
			failAt:       "PersistRunningState",
			wantCalled:   []string{"LoadMetadata", "CheckNotRunning", "StartVM", "PersistRunningState"},
			wantCleanups: []string{"stop VM"},
		},
		{
			failAt: "MountLocalDir",
			wantCalled: []string{
				"LoadMetadata", "CheckNotRunning", "StartVM", "PersistRunningState",
				"MountLocalDir",
			},
			wantCleanups: []string{"remove from vms map", "stop VM"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.failAt, func(t *testing.T) {
			b := newMockStarter()
			b.failAt[tc.failAt] = errors.New("injected failure")
			_, err := StartShed(context.Background(), b, StartRequest{Name: "fail-" + tc.failAt})
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

// TestStartShed_BestEffortHooksDoNotAbort verifies that the best-effort
// post-mount hooks (SetupCredentials, RunStartupHook) cannot abort the
// start — would catch a regression that made them return error AND
// the orchestrator started propagating it.
func TestStartShed_BestEffortHooksDoNotAbort(t *testing.T) {
	b := newMockStarter()
	got, err := StartShed(context.Background(), b, StartRequest{Name: "best-effort"})
	if err != nil {
		t.Fatalf("StartShed returned error from best-effort path: %v", err)
	}
	if got == nil || got.Name != "best-effort" {
		t.Fatalf("ToShedResult returned %v; want a Shed", got)
	}
	wantTail := []string{"SetupCredentials", "RunStartupHook", "ToShedResult"}
	gotTail := b.calls[len(b.calls)-len(wantTail):]
	if !sliceEqual(gotTail, wantTail) {
		t.Errorf("best-effort tail = %v, want %v", gotTail, wantTail)
	}
}
