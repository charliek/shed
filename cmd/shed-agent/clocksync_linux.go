//go:build linux
// +build linux

package main

import (
	"context"
	"log"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// defaultClockSyncInterval is how often the agent compares the system clock
	// to the RTC. Independent of defaultClockSyncThreshold (they happen to share
	// the same value).
	defaultClockSyncInterval = 30 * time.Second

	// defaultClockSyncThreshold is the minimum system-vs-RTC gap that triggers a
	// step. It is deliberately large: with systemd-timesyncd running, normal
	// operation keeps the two within a second, so a gap this size means the guest
	// was paused. Keeping it well above any NTP-vs-RTC disagreement means the
	// agent only handles the big post-sleep jump and never fights timesyncd's
	// fine-grained discipline.
	defaultClockSyncThreshold = 30 * time.Second

	// rtcSinceEpochPath is the sysfs node exposing the RTC as UTC epoch seconds.
	// Present on the VZ PL031 RTC; absent on Firecracker x86 (which emulates no
	// RTC — its time comes from a paravirtualized clock, kvm-clock/TSC), where
	// readRTCFrom returns ok=false and the loop no-ops.
	rtcSinceEpochPath = "/sys/class/rtc/rtc0/since_epoch"
)

// readRTC reads the host-backed RTC via the sysfs since_epoch node. Returns
// ok=false where no RTC exists (Firecracker x86), which disables the resync.
func readRTC() (time.Time, bool) {
	return readRTCFrom(rtcSinceEpochPath)
}

// stepClock sets CLOCK_REALTIME to t. Requires CAP_SYS_TIME — shed-agent runs as
// root (no User= in shed-agent.service). If that service is ever hardened with
// User=/CapabilityBoundingSet=, CAP_SYS_TIME must be retained or this returns
// EPERM and clock-sync goes inert. Callers only pass RTC times that cleared
// shouldStepClock's plausibility window, so t.UnixNano() is safely in range.
func stepClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

// runClockSync periodically steps the guest system clock forward to the
// host-backed RTC, correcting the drift that accumulates while the host sleeps
// (see clocksync.go for the why). It re-reads the RTC every tick so a
// late-appearing RTC is still picked up, and no-ops cleanly where none exists.
// Owned by the server: runs under s.wg/s.ctx and exits on ctx cancellation.
//
// Logging is transition-only to avoid journal spam on the tick cadence: the
// "no RTC" line is logged once, when the RTC first reads absent; actual steps
// (rare — only after a sleep) are always logged; repeated step errors are
// deduplicated by message.
func (s *Server) runClockSync(ctx context.Context) {
	// One startup line so an operator can confirm the loop is live: with
	// transition-only logging below, a healthy VZ that hasn't slept would
	// otherwise log nothing until the first correction.
	log.Printf("clock-sync: started (checking the host RTC every %s, stepping when drift > %s)",
		defaultClockSyncInterval, defaultClockSyncThreshold)

	rtcWasPresent := true // VZ's normal state; flips + logs once if absent (FC)
	var lastErr string

	reconcile := func() {
		out := reconcileClock(time.Now, readRTC, stepClock, defaultClockSyncThreshold)

		if out.rtcPresent != rtcWasPresent {
			if out.rtcPresent {
				log.Printf("clock-sync: RTC present at %s; active", rtcSinceEpochPath)
			} else {
				log.Printf("clock-sync: no RTC at %s; idle (systemd-timesyncd handles drift)", rtcSinceEpochPath)
			}
			rtcWasPresent = out.rtcPresent
		}

		switch {
		case out.stepped:
			log.Printf("clock-sync: stepped clock forward %s (system %s -> RTC %s)",
				out.to.Sub(out.from).Round(time.Second),
				out.from.UTC().Format(time.RFC3339), out.to.UTC().Format(time.RFC3339))
			lastErr = ""
		case out.err != nil && out.err.Error() != lastErr:
			log.Printf("clock-sync: failed to step clock to RTC %s: %v",
				out.to.UTC().Format(time.RFC3339), out.err)
			lastErr = out.err.Error()
		}
	}

	reconcile() // immediate pass (covers an agent restart shortly after a sleep)

	ticker := time.NewTicker(defaultClockSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
