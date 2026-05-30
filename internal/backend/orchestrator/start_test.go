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

// RestoreStoppedMetadata is registered by the orchestrator (not the
// backend) as a cleanup, so its execution shows up in cleanupsRun via
// the closure the orchestrator wraps around it — recording the call
// here is enough to verify the cleanup fired and slot it into the
// LIFO ordering.
func (b *mockStarter) RestoreStoppedMetadata(meta MetadataHandle) error {
	b.cleanupsRun = append(b.cleanupsRun, "restore stopped metadata")
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
			// LIFO: registered order is "restore stopped metadata"
			// (orchestrator, BEFORE StartVM) → "stop VM" (StartVM) →
			// "remove from vms map" (PersistRunningState); unwind runs
			// reverse. "Restore stopped metadata" running LAST is the
			// CodeRabbit-flagged invariant — only rewrite disk after
			// "stop VM" has actually terminated the VMM, matching
			// StopShed's verify-before-clear shape.
			wantCleanups: []string{"remove from vms map", "stop VM", "restore stopped metadata"},
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

// TestStartShed_RestoreStoppedMetadataNotInvokedOnEarlyFail ensures
// the metadata-restore cleanup never actually invokes
// RestoreStoppedMetadata when PersistRunningState hasn't succeeded.
// The orchestrator registers the cleanup early (BEFORE StartVM, so
// LIFO puts it AFTER "stop VM" — see start.go for the rationale), but
// the closure is gated on a persistedRunning flag set immediately
// after PersistRunningState returns. If the metadata never reached
// Running on disk, rewriting "Stopped" could clobber whatever
// CheckNotRunning left the in-memory shape as.
func TestStartShed_RestoreStoppedMetadataNotInvokedOnEarlyFail(t *testing.T) {
	for _, failAt := range []string{"LoadMetadata", "CheckNotRunning", "StartVM", "PersistRunningState"} {
		t.Run(failAt, func(t *testing.T) {
			b := newMockStarter()
			b.failAt[failAt] = errors.New("injected failure")
			if _, err := StartShed(context.Background(), b, StartRequest{Name: "x"}); err == nil {
				t.Fatal("expected error from injected failure")
			}
			for _, c := range b.cleanupsRun {
				if c == "restore stopped metadata" {
					t.Fatalf("RestoreStoppedMetadata fired on %s failure; cleanups=%v", failAt, b.cleanupsRun)
				}
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
