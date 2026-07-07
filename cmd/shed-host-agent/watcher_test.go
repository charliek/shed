package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitFor(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestRunWatchLoopOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		runWatchLoop(ctx, DiscoveryConfig{Watch: "off"}, func() { ch <- struct{}{} }, slog.Default())
		close(done)
	}()

	if !waitFor(ch, time.Second) {
		t.Fatal("no initial reconcile")
	}
	// "off" must not reconcile again.
	select {
	case <-ch:
		t.Fatal("off mode reconciled more than once")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if !waitFor(done, time.Second) {
		t.Fatal("off mode did not return after cancel")
	}
}

func TestRunWatchLoopPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan struct{}, 100)
	go runWatchLoop(ctx, DiscoveryConfig{Watch: "poll", PollInterval: "20ms"},
		func() { ch <- struct{}{} }, slog.Default())

	if !waitFor(ch, time.Second) {
		t.Fatal("no initial reconcile")
	}
	if !waitFor(ch, time.Second) {
		t.Fatal("no poll-tick reconcile")
	}
}

func TestRunWatchLoopFsnotify(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "config.yaml")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan struct{}, 100)
	go runWatchLoop(ctx, DiscoveryConfig{Watch: "fsnotify", Source: source, Debounce: "20ms"},
		func() { ch <- struct{}{} }, slog.Default())

	if !waitFor(ch, 2*time.Second) {
		t.Fatal("no initial reconcile")
	}

	// Write the source until a reconcile fires; the retry loop tolerates the
	// brief window before the directory watch is registered.
	got := false
	for i := 0; i < 50 && !got; i++ {
		if err := os.WriteFile(source, []byte("servers: {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		got = waitFor(ch, 100*time.Millisecond)
	}
	if !got {
		t.Fatal("fsnotify did not trigger a reconcile on write")
	}
}
