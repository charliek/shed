package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// runWatchLoop drives reconcile based on the discovery watch mode. It performs
// an initial reconcile, then triggers more until ctx is cancelled:
//   - "off":      reconcile once, then idle until shutdown.
//   - "poll":     reconcile on a ticker.
//   - "fsnotify": reconcile on debounced filesystem events for the discovery
//     source's directory; falls back to polling if the watcher can't start.
func runWatchLoop(ctx context.Context, dc DiscoveryConfig, reconcile func(), logger *slog.Logger) {
	reconcile()

	switch dc.Watch {
	case "off":
		<-ctx.Done()
	case "poll":
		runPollLoop(ctx, dc, reconcile, logger)
	case "fsnotify", "":
		runFsnotifyLoop(ctx, dc, reconcile, logger)
	default:
		logger.Warn("unknown discovery.watch mode, using poll", "mode", dc.Watch)
		runPollLoop(ctx, dc, reconcile, logger)
	}
}

func runPollLoop(ctx context.Context, dc DiscoveryConfig, reconcile func(), logger *slog.Logger) {
	interval := parseDurationOr(dc.PollInterval, 10*time.Second, "discovery.poll_interval", logger)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	logger.Info("watching discovery source by polling", "source", dc.Source, "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func runFsnotifyLoop(ctx context.Context, dc DiscoveryConfig, reconcile func(), logger *slog.Logger) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("fsnotify unavailable, falling back to polling", "error", err)
		runPollLoop(ctx, dc, reconcile, logger)
		return
	}
	defer w.Close()

	// Watch the parent directory rather than the file itself so atomic rewrites
	// (write temp + rename, which shed's CLI uses) are still observed — a
	// single-file watch would be left pointing at the replaced inode.
	dir := filepath.Dir(dc.Source)
	if err := w.Add(dir); err != nil {
		logger.Warn("cannot watch discovery directory, falling back to polling", "dir", dir, "error", err)
		runPollLoop(ctx, dc, reconcile, logger)
		return
	}
	logger.Info("watching discovery source via fsnotify", "dir", dir, "source", dc.Source)

	debounce := parseDurationOr(dc.Debounce, 500*time.Millisecond, "discovery.debounce", logger)
	base := filepath.Base(dc.Source)

	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			// Coalesce bursts (editors and atomic-rename emit several events).
			// A spurious extra reconcile is harmless — Reconcile is idempotent.
			if timer == nil {
				timer = time.NewTimer(debounce)
				timerC = timer.C
			} else {
				timer.Reset(debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			logger.Warn("fsnotify error", "error", err)
		case <-timerC:
			timer = nil
			timerC = nil
			reconcile()
		}
	}
}

// parseDurationOr parses d, falling back to def (with a warning) when empty,
// unparseable, or non-positive.
func parseDurationOr(d string, def time.Duration, name string, logger *slog.Logger) time.Duration {
	parsed, err := time.ParseDuration(d)
	if err != nil || parsed <= 0 {
		logger.Warn("invalid duration, using default", "field", name, "value", d, "default", def)
		return def
	}
	return parsed
}
