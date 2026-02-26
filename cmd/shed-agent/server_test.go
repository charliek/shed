//go:build linux
// +build linux

package main

import (
	"context"
	"testing"
	"time"
)

func TestStopDrainsWithinTimeout(t *testing.T) {
	// Verify that Stop() returns promptly when no connections are active.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		ctx:    ctx,
		cancel: cancel,
	}

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("Stop() took %v with no active connections, expected < 1s", elapsed)
	}
}

func TestStopTimesOutOnHungConnection(t *testing.T) {
	// Simulate a hung connection handler by incrementing the WaitGroup
	// without ever calling Done(). Stop() should return after drainTimeout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		ctx:    ctx,
		cancel: cancel,
	}

	// Simulate a connection that never finishes
	s.wg.Add(1)

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	// Should complete within drainTimeout + small buffer
	if elapsed < drainTimeout {
		t.Errorf("Stop() returned in %v, expected >= %v (drain timeout)", elapsed, drainTimeout)
	}
	if elapsed > drainTimeout+2*time.Second {
		t.Errorf("Stop() took %v, expected close to %v", elapsed, drainTimeout)
	}

	// Clean up: decrement the WaitGroup so the goroutine inside Stop can exit
	s.wg.Done()
}

func TestStopReturnsEarlyWhenConnectionsDrain(t *testing.T) {
	// Verify that Stop() returns as soon as active connections finish,
	// without waiting the full drain timeout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		ctx:    ctx,
		cancel: cancel,
	}

	// Simulate a connection that finishes after 500ms
	s.wg.Add(1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.wg.Done()
	}()

	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)

	// Should complete well before drain timeout
	if elapsed > 2*time.Second {
		t.Errorf("Stop() took %v, expected < 2s (connection drains in 500ms)", elapsed)
	}
}
