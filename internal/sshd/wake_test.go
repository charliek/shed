package sshd

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// wakeFakeBackend is a synchronous (Firecracker/VZ-shaped) backend: StartShed
// returns at once and GetShed reports running after runningAfter polls.
// Methods the wake path never calls are left to the embedded nil interface.
type wakeFakeBackend struct {
	backend.Backend
	mu           sync.Mutex
	status       string
	reason       string
	missing      bool
	startErr     error
	runningAfter int // polls after a start until running; <0 never
	polls        int
	starts       int
}

func (b *wakeFakeBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.missing {
		return nil, config.ErrShedNotFoundSentinel
	}
	if b.status == config.StatusStarting && b.runningAfter >= 0 {
		if b.polls >= b.runningAfter {
			b.status = config.StatusRunning
		}
		b.polls++
	}
	return &config.Shed{Name: name, Status: b.status, StatusReason: b.reason}, nil
}

func (b *wakeFakeBackend) StartShed(_ context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	if b.startErr != nil {
		return nil, b.startErr
	}
	b.status = config.StatusStarting
	return &config.Shed{Name: name, Status: b.status}, nil
}

// asyncWakeBackend adds WaitSettled: a waited-on job moves starting to
// running (or to final) and stopping to stopped.
type asyncWakeBackend struct {
	wakeFakeBackend
	final   string // what a start settles on; default running
	hang    bool   // WaitSettled blocks until ctx ends
	waits   int
	started context.Context
}

func (b *asyncWakeBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	b.started = ctx
	b.mu.Unlock()
	return b.wakeFakeBackend.StartShed(ctx, name)
}

func (b *asyncWakeBackend) WaitSettled(ctx context.Context, name string) (*config.Shed, error) {
	backend.Status(ctx, "job: "+name)
	if b.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.waits++
	switch b.status {
	case config.StatusStarting:
		b.status = config.StatusRunning
		if b.final != "" {
			b.status = b.final
		}
	case config.StatusStopping:
		b.status = config.StatusStopped
	}
	return &config.Shed{Name: name, Status: b.status, StatusReason: b.reason}, nil
}

// TestSyncWakeUnchanged pins the Firecracker/VZ wake: a synchronous start,
// then a 250 ms poll for at most 10 s.
func TestSyncWakeUnchanged(t *testing.T) {
	if containerReadyTimeout != 10*time.Second || containerReadyPollInterval != 250*time.Millisecond {
		t.Fatalf("wake timing changed: %s / %s", containerReadyTimeout, containerReadyPollInterval)
	}
	old := readyTimeout
	readyTimeout = 600 * time.Millisecond
	t.Cleanup(func() { readyTimeout = old })

	tests := []struct {
		name       string
		b          *wakeFakeBackend
		announce   bool
		wantErr    string
		wantStarts int
		wantStderr string
	}{
		{name: "running", b: &wakeFakeBackend{status: config.StatusRunning}},
		{name: "stopped wakes", b: &wakeFakeBackend{status: config.StatusStopped, runningAfter: 1}, announce: true, wantStarts: 1, wantStderr: "Starting shed 'a1'..."},
		{name: "sftp wakes quietly", b: &wakeFakeBackend{status: config.StatusStopped, runningAfter: 1}, wantStarts: 1},
		{name: "starting waits", b: &wakeFakeBackend{status: config.StatusStarting, runningAfter: 1}},
		{name: "not found", b: &wakeFakeBackend{missing: true}, wantErr: "shed 'a1' not found"},
		{name: "start fails", b: &wakeFakeBackend{status: config.StatusStopped, startErr: config.ErrZoneCapacityUnavailableSentinel}, wantErr: "failed to start shed: zone capacity unavailable", wantStarts: 1},
		{name: "never ready", b: &wakeFakeBackend{status: config.StatusStarting, runningAfter: -1}, wantErr: "shed not ready: timeout waiting for shed to be ready"},
		{name: "stopping is not running", b: &wakeFakeBackend{status: config.StatusStopping}, wantErr: "shed 'a1' is not running (status: stopping)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{backend: tt.b}
			var stderr bytes.Buffer
			shed, err := s.ensureRunning(context.Background(), "a1", &stderr, tt.announce)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || shed.Status != config.StatusRunning {
				t.Fatalf("shed %+v err %v, want running", shed, err)
			}
			if tt.b.starts != tt.wantStarts {
				t.Errorf("starts = %d, want %d", tt.b.starts, tt.wantStarts)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) || (tt.wantStderr == "" && stderr.Len() > 0) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestAsyncWake(t *testing.T) {
	tests := []struct {
		name       string
		b          *asyncWakeBackend
		wantErr    string
		wantStarts int
		wantWaits  int
		wantStderr []string
	}{
		{name: "running", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusRunning}}},
		{name: "stopped wakes and waits", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusStopped, runningAfter: -1}},
			wantStarts: 1, wantWaits: 1, wantStderr: []string{"Starting shed 'a1'...", "job: a1"}},
		{name: "starting waits", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusStarting, runningAfter: -1}},
			wantWaits: 1},
		{name: "stopping waits then starts", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusStopping, runningAfter: -1}},
			wantStarts: 1, wantWaits: 2, wantStderr: []string{"Waiting for shed 'a1' to stop...", "Starting shed 'a1'..."}},
		{name: "start refused", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusStopped, runningAfter: -1, reason: config.StatusReasonCapacity}, final: config.StatusStopped},
			wantStarts: 1, wantWaits: 1, wantErr: "shed 'a1' is not running (status: stopped, reason: capacity)"},
		{name: "error shed", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusError, reason: config.StatusReasonCreateFailed}},
			wantErr: "shed 'a1' is not running (status: error, reason: create_failed)"},
		{name: "budget expires", b: &asyncWakeBackend{wakeFakeBackend: wakeFakeBackend{status: config.StatusStarting, runningAfter: -1}, hang: true},
			wantErr: "shed not ready: timed out after 50ms (status: starting)"},
	}
	old := asyncWakeTimeout
	asyncWakeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { asyncWakeTimeout = old })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{backend: tt.b}
			var stderr bytes.Buffer
			shed, err := s.ensureRunning(context.Background(), "a1", &stderr, true)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || shed.Status != config.StatusRunning {
				t.Fatalf("shed %+v err %v, want running", shed, err)
			}
			if tt.b.starts != tt.wantStarts || tt.b.waits != tt.wantWaits {
				t.Errorf("starts %d waits %d, want %d %d", tt.b.starts, tt.b.waits, tt.wantStarts, tt.wantWaits)
			}
			for _, w := range tt.wantStderr {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr %q lacks %q", stderr.String(), w)
				}
			}
			if tt.b.started != nil && tt.b.started.Done() != nil {
				t.Error("an async start must not be cancellable by the session")
			}
		})
	}
}
